// Package workspace applies reviewed starter files without executing project code.
// Its process lock coordinates goth, not editors or arbitrary same-user programs.
package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gothstack.com/cli/internal/device"
	"gothstack.com/cli/internal/plan"
)

var (
	ErrWorkspace = errors.New("workspace is unsafe, unsupported or unreadable; use a user-owned directory with a normal .git directory")
	ErrBusy      = errors.New("another goth process is using this workspace")
	ErrConflict  = errors.New("starter files conflict with local changes; preserve your work and resolve the reported path before retrying")
	ErrRecovery  = errors.New("workspace update was interrupted; recovery data is retained in .git/gothstack/journal.json; retry goth start, and keep this file if recovery reports a conflict")
)

const meta = ".git/gothstack"
const statePath = meta + "/state.json"
const journalPath = meta + "/journal.json"
const stagePath = meta + "/stage"
const maxStateBytes = 2 << 20
const maxJournalBytes = 12 << 20

type Workspace struct {
	root *os.Root
	git  *gitRepo
	lock *os.File
	// Fault injection is package-private and used only by filesystem tests.
	afterWrite func(int) error
}
type state struct {
	Schema  int               `json:"schema"`
	Origin  string            `json:"origin"`
	Project string            `json:"project"`
	Course  string            `json:"course"`
	Lesson  string            `json:"lesson"`
	Version int64             `json:"version"`
	Digest  string            `json:"digest"`
	Files   map[string]string `json:"files"`
}
type snapshot struct {
	Exists  bool   `json:"exists"`
	Content []byte `json:"content"`
	Mode    uint32 `json:"mode"`
}
type change struct {
	Path   string   `json:"path"`
	Before snapshot `json:"before"`
	After  snapshot `json:"after"`
}
type journal struct {
	Schema  int      `json:"schema"`
	Changes []change `json:"changes"`
}

// Prepared is bound to one open, locked workspace and one reviewed projection.
// Call Apply only after the server confirms the exact plan was started.
type Prepared struct {
	workspace      *Workspace
	digest, branch string
	changes        []change
}

func (p *Prepared) Branch() string { return p.branch }

// Open requires an explicitly selected existing directory. It initializes only
// when .git is absent; linked worktrees and symlinked Git directories are refused.
func Open(ctx context.Context, dir string) (*Workspace, error) {
	return openWorkspace(ctx, dir, true)
}

// OpenStarted refuses to initialize an unrelated directory when checking.
func OpenStarted(ctx context.Context, dir string) (*Workspace, error) {
	return openWorkspace(ctx, dir, false)
}

func openWorkspace(ctx context.Context, dir string, initialize bool) (_ *Workspace, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, ErrWorkspace
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || !owned(info) {
		return nil, ErrWorkspace
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, ErrWorkspace
	}
	w := &Workspace{root: root}
	defer func() {
		if err != nil {
			w.Close()
		}
	}()
	g, err := newGit(ctx, abs)
	if err != nil {
		return nil, err
	}
	w.git = g
	info, err = root.Lstat(".git")
	if errors.Is(err, os.ErrNotExist) {
		if !initialize {
			return nil, ErrNotStarted
		}
		if err = g.initialize(ctx); err != nil {
			return nil, err
		}
	} else if err != nil || !info.IsDir() || !owned(info) {
		return nil, ErrWorkspace
	}
	if err = w.directory(".git", false); err != nil {
		return nil, err
	}
	if err = w.directory(meta, initialize); err != nil {
		return nil, err
	}
	f, err := root.OpenFile(meta+"/lock", os.O_RDWR|os.O_CREATE|noFollow, 0600)
	if err != nil {
		return nil, ErrWorkspace
	}
	w.lock = f
	info, err = f.Stat()
	if err != nil || !regular(info) {
		return nil, ErrWorkspace
	}
	if err = lock(f); err != nil {
		return nil, ErrBusy
	}
	if err = w.checkScratchClean(); err != nil {
		return nil, err
	}
	if err = w.recover(); err != nil {
		return nil, err
	}
	return w, nil
}
func (w *Workspace) Close() {
	if w.lock != nil {
		_ = w.lock.Close()
	}
	if w.root != nil {
		_ = w.root.Close()
	}
	if w.git != nil {
		w.git.close()
	}
}
func regular(i os.FileInfo) bool { return i.Mode().IsRegular() && owned(i) }
func (w *Workspace) directory(p string, create bool) error {
	// Inspect every component so an internal symlink is refused too. os.Root
	// independently prevents escape if a component changes during the operation.
	current := ""
	for _, part := range strings.Split(p, "/") {
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		info, e := w.root.Lstat(current)
		if errors.Is(e, os.ErrNotExist) && create {
			e = w.root.Mkdir(current, 0700)
			if e == nil || errors.Is(e, os.ErrExist) {
				info, e = w.root.Lstat(current)
			}
		}
		if e != nil || !info.IsDir() || !owned(info) {
			return ErrWorkspace
		}
	}
	return nil
}
func (w *Workspace) read(p string, limit int) (snapshot, error) {
	return w.inspect(p, limit, true)
}
func (w *Workspace) inspect(p string, limit int, contents bool) (snapshot, error) {
	// Missing parents mean a missing file; present parents must be plain directories.
	parent := path.Dir(p)
	if parent != "." {
		for current := parent; current != "."; current = path.Dir(current) {
			i, e := w.root.Lstat(current)
			if errors.Is(e, os.ErrNotExist) {
				continue
			}
			if e != nil || !i.IsDir() || !owned(i) {
				return snapshot{}, ErrWorkspace
			}
		}
	}
	i, e := w.root.Lstat(p)
	if errors.Is(e, os.ErrNotExist) {
		return snapshot{}, nil
	}
	if e != nil || !regular(i) || contents && i.Size() > int64(limit) {
		return snapshot{}, ErrWorkspace
	}
	if !contents {
		return snapshot{Exists: true, Mode: uint32(i.Mode().Perm())}, nil
	}
	f, e := w.root.OpenFile(p, os.O_RDONLY|noFollow, 0)
	if e != nil {
		return snapshot{}, ErrWorkspace
	}
	defer f.Close()
	actual, e := f.Stat()
	if e != nil || !regular(actual) || !os.SameFile(i, actual) {
		return snapshot{}, ErrWorkspace
	}
	raw, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e != nil || len(raw) > limit {
		return snapshot{}, ErrWorkspace
	}
	return snapshot{true, raw, uint32(actual.Mode().Perm())}, nil
}
func hash(raw []byte) string { s := sha256.Sum256(raw); return hex.EncodeToString(s[:]) }
func same(a, b snapshot) bool {
	return a.Exists == b.Exists && (!a.Exists || a.Mode == b.Mode && hash(a.Content) == hash(b.Content))
}
func strictJSON(raw []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(v) != nil {
		return ErrWorkspace
	}
	// Internal metadata is written canonically. This also rejects duplicate keys,
	// case aliases and unknown additions without accepting ambiguous recovery data.
	canonical, e := json.Marshal(v)
	if e != nil || string(canonical) != string(raw) {
		return ErrWorkspace
	}
	return nil
}
func validState(s state) bool {
	if s.Schema != 1 || len(s.Files) > 4096 || s.Files == nil || !plan.ValidSlug(s.Course) || !plan.ValidSlug(s.Lesson) || s.Version < 1 || len(s.Digest) != 64 {
		return false
	}
	origin, e := device.Origin(s.Origin)
	if e != nil || origin != s.Origin {
		return false
	}
	for p, h := range s.Files {
		if !plan.PortablePath(p) || len(h) != 64 {
			return false
		}
		if _, e := hex.DecodeString(h); e != nil {
			return false
		}
	}
	return true
}
func (w *Workspace) Prepare(origin string, p plan.Lesson) (*Prepared, error) {
	digest, e := p.Digest()
	if e != nil {
		return nil, plan.ErrInvalid
	}
	normalized, e := device.Origin(origin)
	if e != nil || normalized != origin {
		return nil, ErrWorkspace
	}
	project := p.Missions[0].Plan.Project
	files := map[string]string{}
	paths := map[string]string{}
	total := 0
	for _, m := range p.Missions {
		if m.Plan.Project != project {
			return nil, plan.ErrInvalid
		}
		for _, f := range m.Plan.Files {
			if old, ok := files[f.Path]; ok {
				if old != f.Content {
					return nil, plan.ErrInvalid
				}
				continue
			}
			key := strings.ToLower(f.Path)
			if _, ok := paths[key]; ok {
				return nil, plan.ErrInvalid
			}
			files[f.Path] = f.Content
			paths[key] = f.Path
			total += len(f.Content)
		}
	}
	if len(files) > 1024 || total > 2<<20 {
		return nil, plan.ErrInvalid
	}
	for key := range paths {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if _, ok := paths[parent]; ok {
				return nil, plan.ErrInvalid
			}
		}
	}
	previous, e := w.read(statePath, maxStateBytes)
	if e != nil {
		return nil, e
	}
	s := state{Schema: 1, Origin: origin, Project: project, Files: map[string]string{}}
	if previous.Exists {
		if strictJSON(previous.Content, &s) != nil || !validState(s) || s.Origin != origin || s.Project != project {
			return nil, ErrWorkspace
		}
	}
	// Keep the incremental project portable across case-sensitive and
	// case-insensitive filesystems, including paths from earlier lessons.
	combined := map[string]string{}
	for name := range s.Files {
		combined[strings.ToLower(name)] = name
	}
	for name := range files {
		key := strings.ToLower(name)
		if old, ok := combined[key]; ok && old != name {
			return nil, plan.ErrInvalid
		}
		combined[key] = name
	}
	for key := range combined {
		for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
			if _, ok := combined[parent]; ok {
				return nil, plan.ErrInvalid
			}
		}
	}
	s.Course = p.CourseSlug
	s.Lesson = p.LessonSlug
	s.Version = p.LessonVersion
	s.Digest = digest
	prepared := &Prepared{workspace: w, digest: digest, branch: fmt.Sprintf("goth-%s-v%d", p.LessonID, p.LessonVersion)}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	oldBytes := 0
	for _, name := range names {
		if e := w.portableExisting(name); e != nil {
			return nil, fmt.Errorf("%w: %s", e, name)
		}
		old, e := w.read(name, plan.MaxFileBytes)
		if e != nil {
			return nil, fmt.Errorf("%w: %s", e, name)
		}
		oldBytes += len(old.Content)
		if oldBytes > 2<<20 {
			return nil, ErrWorkspace
		}
		content := []byte(files[name])
		expected := hash(content)
		baseline, tracked := s.Files[name]
		if old.Exists && hash(old.Content) != expected && (!tracked || hash(old.Content) != baseline) || !old.Exists && tracked {
			return nil, fmt.Errorf("%w: %s", ErrConflict, name)
		}
		mode := uint32(0644)
		if old.Exists {
			mode = old.Mode
			if mode&0200 == 0 {
				return nil, fmt.Errorf("%w: %s", ErrConflict, name)
			}
		}
		next := snapshot{true, content, mode}
		// Include unchanged paths in the preflight snapshot: a learner edit between
		// review and confirmation must not be silently adopted as a starter baseline.
		prepared.changes = append(prepared.changes, change{name, old, next})
		s.Files[name] = expected
	}
	if len(s.Files) > 4096 {
		return nil, ErrWorkspace
	}
	raw, e := json.Marshal(s)
	if e != nil || len(raw) > maxStateBytes {
		return nil, ErrWorkspace
	}
	prepared.changes = append(prepared.changes, change{statePath, previous, snapshot{true, raw, 0600}})
	return prepared, nil
}

// Apply checks every reviewed file again after network confirmation, then makes
// the branch and writes a recovery journal before replacing any starter file.
func (p *Prepared) Apply(ctx context.Context, confirmed plan.Lesson) error {
	d, e := confirmed.Digest()
	if e != nil || d != p.digest {
		return plan.ErrInvalid
	}
	w := p.workspace
	for _, c := range p.changes {
		now, e := w.read(c.Path, maxStateBytes)
		if e != nil {
			return e
		}
		if !same(now, c.Before) {
			return fmt.Errorf("%w: %s", ErrConflict, c.Path)
		}
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = w.git.switchBranch(ctx, p.branch); e != nil {
		return e
	}
	changes := []change{}
	for _, c := range p.changes {
		if !same(c.Before, c.After) {
			changes = append(changes, c)
		}
	}
	if len(changes) == 0 {
		return nil
	}
	raw, e := json.Marshal(journal{1, changes})
	if e != nil || len(raw) > maxJournalBytes {
		return ErrWorkspace
	}
	if e = w.exclusive(journalPath, raw, 0600); e != nil {
		return ErrRecovery
	}
	if e = syncDirectory(w.root, meta); e != nil {
		return ErrRecovery
	}
	for i, c := range changes {
		if ctx.Err() != nil {
			return ErrRecovery
		}
		if e = w.replace(c.Path, c.Before, c.After); e != nil {
			return errors.Join(ErrRecovery, e)
		}
		if w.afterWrite != nil {
			if e = w.afterWrite(i); e != nil {
				return e
			}
		}
	}
	// state.json is last. Recovery recognizes a fully applied transaction from
	// all final snapshots; no separate marker can get ahead of the file writes.
	return w.finish()
}
func (w *Workspace) exclusive(p string, raw []byte, mode os.FileMode) error {
	f, e := w.root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL|noFollow, mode)
	if e != nil {
		return ErrWorkspace
	}
	e = f.Chmod(mode)
	if e == nil {
		_, e = f.Write(raw)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil || closeErr != nil {
		return ErrWorkspace
	}
	return nil
}
func (w *Workspace) replace(p string, before, after snapshot) error {
	current, e := w.read(p, maxStateBytes)
	if e != nil {
		return e
	}
	if !same(current, before) {
		return fmt.Errorf("%w: %s", ErrConflict, p)
	}
	if !after.Exists {
		if !before.Exists {
			return nil
		}
		if e = w.root.Remove(p); e != nil {
			return ErrWorkspace
		}
		return syncDirectory(w.root, path.Dir(p))
	}
	if e = w.directory(path.Dir(p), true); e != nil {
		return e
	}
	if e = w.exclusive(stagePath, after.Content, os.FileMode(after.Mode)); e != nil {
		return e
	}
	// Recheck after staging. Editors do not use goth's lock; a residual external
	// write/rename race cannot be eliminated portably. The journal retains backups.
	current, e = w.read(p, maxStateBytes)
	if e != nil || !same(current, before) {
		_ = w.root.Remove(stagePath)
		return fmt.Errorf("%w: %s", ErrConflict, p)
	}
	if e = w.root.Rename(stagePath, p); e != nil {
		return ErrWorkspace
	}
	if e = syncDirectory(w.root, path.Dir(p)); e != nil {
		return e
	}
	return syncDirectory(w.root, meta)
}
func (w *Workspace) finish() error {
	if e := w.root.Remove(journalPath); e != nil {
		return ErrRecovery
	}
	if e := syncDirectory(w.root, meta); e != nil {
		return ErrRecovery
	}
	return nil
}
func (w *Workspace) recover() error {
	disk, e := w.read(journalPath, maxJournalBytes)
	if e != nil {
		return ErrRecovery
	}
	staged, e := w.read(stagePath, maxStateBytes)
	if e != nil {
		return ErrRecovery
	}
	if !disk.Exists {
		if staged.Exists {
			return ErrRecovery
		}
		return nil
	}
	var j journal
	if strictJSON(disk.Content, &j) != nil || j.Schema != 1 || len(j.Changes) < 1 || len(j.Changes) > 1025 {
		return ErrRecovery
	}
	seen := map[string]bool{}
	allAfter := true
	for i, c := range j.Changes {
		if c.Path != statePath && !plan.PortablePath(c.Path) || seen[strings.ToLower(c.Path)] || !c.After.Exists || c.After.Mode > 0777 || c.Before.Mode > 0777 || !c.Before.Exists && (len(c.Before.Content) > 0 || c.Before.Mode != 0) {
			return ErrRecovery
		}
		if i == len(j.Changes)-1 && c.Path != statePath {
			return ErrRecovery
		}
		if c.Path == statePath {
			var s state
			if strictJSON(c.After.Content, &s) != nil || !validState(s) {
				return ErrRecovery
			}
		}
		limit := plan.MaxFileBytes
		if c.Path == statePath {
			limit = maxStateBytes
		}
		if len(c.Before.Content) > limit || len(c.After.Content) > limit {
			return ErrRecovery
		}
		seen[strings.ToLower(c.Path)] = true
		now, e := w.read(c.Path, maxStateBytes)
		if e != nil {
			return ErrRecovery
		}
		if !same(now, c.After) {
			allAfter = false
			if !same(now, c.Before) {
				return fmt.Errorf("%w: %s", ErrRecovery, c.Path)
			}
		}
	}
	if staged.Exists {
		known := false
		for _, c := range j.Changes {
			if same(staged, c.Before) || same(staged, c.After) {
				known = true
				break
			}
		}
		if !known {
			return ErrRecovery
		}
		if e = w.root.Remove(stagePath); e != nil {
			return ErrRecovery
		}
	}
	if !allAfter {
		for i := len(j.Changes) - 1; i >= 0; i-- {
			c := j.Changes[i]
			now, e := w.read(c.Path, maxStateBytes)
			if e != nil {
				return ErrRecovery
			}
			if same(now, c.Before) {
				continue
			}
			if !same(now, c.After) {
				return ErrRecovery
			}
			if e = w.replace(c.Path, c.After, c.Before); e != nil {
				return ErrRecovery
			}
		}
	}
	return w.finish()
}

// Reject case aliases already present on disk. Bound directory inspection so a
// huge unrelated folder cannot make a lesson preflight consume unlimited memory.
func (w *Workspace) portableExisting(name string) error {
	parent := "."
	for _, component := range strings.Split(name, "/") {
		dir, err := w.root.Open(parent)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return ErrWorkspace
		}
		count := 0
		for {
			entries, readErr := dir.ReadDir(128)
			count += len(entries)
			if count > 8192 {
				dir.Close()
				return ErrWorkspace
			}
			for _, entry := range entries {
				if strings.EqualFold(entry.Name(), component) && entry.Name() != component {
					dir.Close()
					return ErrConflict
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				dir.Close()
				return ErrWorkspace
			}
		}
		dir.Close()
		parent = path.Join(parent, component)
	}
	return nil
}
