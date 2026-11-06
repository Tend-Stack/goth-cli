package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gothstack.com/cli/internal/plan"
)

func lessonFixture(t *testing.T, version int64, files ...plan.File) plan.Lesson {
	t.Helper()
	p := plan.Plan{Schema: 1, Project: "pocketpilot", Files: files, Checks: []plan.Check{{ID: "source", Kind: "file_assert", TimeoutSeconds: 1, File: &plan.FileAssert{Path: "main.go", Condition: "exists"}}}}
	_, digest, e := p.Canonical()
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(struct {
		Lesson  string `json:"lesson"`
		Version int64  `json:"version"`
		SHA256  string `json:"sha256"`
	}{"lesson", version, digest})
	l := plan.Lesson{LessonID: "lesson", LessonVersion: version, CourseSlug: "course", LessonSlug: "lesson", LessonCode: "05.1", Missions: []plan.LocalMission{{BlockID: "mission", Mission: plan.Mission{CheckID: hash(raw), CheckVersion: version, CheckSHA256: digest, InstructionsMD: "Build locally.", Evidence: []string{"Source"}}, Plan: p}}}
	if _, e = l.Digest(); e != nil {
		t.Fatal(e)
	}
	return l
}
func openTest(t *testing.T, dir string) *Workspace {
	t.Helper()
	w, e := Open(context.Background(), dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(w.Close)
	return w
}
func applyTest(t *testing.T, w *Workspace, p plan.Lesson) {
	t.Helper()
	ready, e := w.Prepare("https://gothstack.com", p)
	if e != nil {
		t.Fatal(e)
	}
	if e = ready.Apply(context.Background(), p); e != nil {
		t.Fatal(e)
	}
}
func readTest(t *testing.T, dir, name, want string) {
	t.Helper()
	b, e := os.ReadFile(filepath.Join(dir, name))
	if e != nil || string(b) != want {
		t.Fatalf("%s: got %q err=%v", name, b, e)
	}
}
func TestWorkspaceScaffoldUpdateAndPreserve(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	one := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "package main\n"}, plan.File{Path: "views/page.templ", Content: "package views\n"})
	applyTest(t, w, one)
	applyTest(t, w, one)
	readTest(t, dir, "main.go", "package main\n")
	readTest(t, dir, ".git/HEAD", "ref: refs/heads/goth-lesson-v1\n")
	if _, e := os.Stat(filepath.Join(dir, journalPath)); !os.IsNotExist(e) {
		t.Fatal("journal was not removed", e)
	}
	two := lessonFixture(t, 2, plan.File{Path: "main.go", Content: "package main\n// next\n"})
	applyTest(t, w, two)
	readTest(t, dir, "main.go", "package main\n// next\n")
	readTest(t, dir, "views/page.templ", "package views\n")
	if e := os.WriteFile(filepath.Join(dir, "main.go"), []byte("learner work"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Prepare("https://gothstack.com", one); !errors.Is(e, ErrConflict) {
		t.Fatal("edited starter accepted", e)
	}
	readTest(t, dir, "main.go", "learner work")
	if _, e := w.Prepare("https://other.example", two); e == nil {
		t.Fatal("cross-origin state adopted")
	}
	if e := os.Remove(filepath.Join(dir, "main.go")); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Prepare("https://gothstack.com", two); !errors.Is(e, ErrConflict) {
		t.Fatal("deleted learner file recreated", e)
	}
}
func TestWorkspaceRechecksAfterConfirmation(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	ready, e := w.Prepare("https://gothstack.com", p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "main.go"), []byte("concurrent edit"), 0644); e != nil {
		t.Fatal(e)
	}
	if e = ready.Apply(context.Background(), p); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	readTest(t, dir, "main.go", "concurrent edit")
	if _, e = os.Stat(filepath.Join(dir, statePath)); !os.IsNotExist(e) {
		t.Fatal("unexpected state", e)
	}
}
func TestWorkspaceRecoveryAndConflict(t *testing.T) {
	for _, when := range []int{0, 1, 2} {
		t.Run(string(rune('0'+when)), func(t *testing.T) {
			dir := t.TempDir()
			w := openTest(t, dir)
			old := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "old"})
			applyTest(t, w, old)
			next := lessonFixture(t, 2, plan.File{Path: "main.go", Content: "new"}, plan.File{Path: "nested/new.go", Content: "new file"})
			ready, e := w.Prepare("https://gothstack.com", next)
			if e != nil {
				t.Fatal(e)
			}
			w.afterWrite = func(i int) error {
				if i == when {
					return ErrRecovery
				}
				return nil
			}
			if e = ready.Apply(context.Background(), next); !errors.Is(e, ErrRecovery) {
				t.Fatal(e)
			}
			w.Close()
			w = openTest(t, dir)
			if when < 2 {
				readTest(t, dir, "main.go", "old")
				if _, e = os.Stat(filepath.Join(dir, "nested/new.go")); !os.IsNotExist(e) {
					t.Fatal("partial new file survived", e)
				}
			} else {
				readTest(t, dir, "main.go", "new")
			}
			applyTest(t, w, next)
			readTest(t, dir, "main.go", "new")
		})
	}
	t.Run("edited-during-interruption", func(t *testing.T) {
		dir := t.TempDir()
		w := openTest(t, dir)
		p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
		w.afterWrite = func(int) error { return ErrRecovery }
		ready, _ := w.Prepare("https://gothstack.com", p)
		if e := ready.Apply(context.Background(), p); !errors.Is(e, ErrRecovery) {
			t.Fatal(e)
		}
		w.Close()
		if e := os.WriteFile(filepath.Join(dir, "main.go"), []byte("saved learner work"), 0644); e != nil {
			t.Fatal(e)
		}
		if x, e := Open(context.Background(), dir); e == nil {
			x.Close()
			t.Fatal("recovery overwrote concurrent edit")
		}
		readTest(t, dir, "main.go", "saved learner work")
		if _, e := os.Stat(filepath.Join(dir, journalPath)); e != nil {
			t.Fatal("recovery evidence lost", e)
		}
	})
}
func TestWorkspaceRejectsUnsafeTargetsAndLocks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native Unix symlink and hardlink fixtures")
	}
	for _, kind := range []string{"symlink", "directory-symlink", "hardlink", "unknown", "readonly"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			file := filepath.Join(outside, "source")
			if e := os.WriteFile(file, []byte("outside"), 0600); e != nil {
				t.Fatal(e)
			}
			w := openTest(t, dir)
			name := "main.go"
			var e error
			switch kind {
			case "symlink":
				e = os.Symlink(file, filepath.Join(dir, name))
			case "directory-symlink":
				e = os.Symlink(outside, filepath.Join(dir, "views"))
				name = "views/source"
			case "hardlink":
				e = os.Link(file, filepath.Join(dir, name))
			case "unknown":
				e = os.WriteFile(filepath.Join(dir, name), []byte("unknown"), 0600)
			case "readonly":
				e = os.WriteFile(filepath.Join(dir, name), []byte("starter"), 0400)
			}
			if e != nil {
				t.Fatal(e)
			}
			p := lessonFixture(t, 1, plan.File{Path: name, Content: "starter"})
			if _, e = w.Prepare("https://gothstack.com", p); e == nil {
				t.Fatal("unsafe target accepted")
			}
			readTest(t, outside, "source", "outside")
		})
	}
	dir := t.TempDir()
	w := openTest(t, dir)
	if other, e := Open(context.Background(), dir); !errors.Is(e, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal("lock bypass", e)
	}
	w.Close()
	again := openTest(t, dir)
	again.Close()
}
func TestWorkspaceRejectsCorruptJournal(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	w.Close()
	if e := os.WriteFile(filepath.Join(dir, journalPath), []byte(`{"schema":1,"changes":[`), 0600); e != nil {
		t.Fatal(e)
	}
	if w, e := Open(context.Background(), dir); !errors.Is(e, ErrRecovery) {
		if w != nil {
			w.Close()
		}
		t.Fatal(e)
	}
}
func TestWorkspaceUnchangedFileIsRechecked(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	applyTest(t, w, p)
	ready, e := w.Prepare("https://gothstack.com", p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "main.go"), []byte(strings.Repeat("x", 10)), 0644); e != nil {
		t.Fatal(e)
	}
	if e = ready.Apply(context.Background(), p); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
}

func TestWorkspacePortableNamesAcrossLessons(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	applyTest(t, w, lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"}))
	if _, e := w.Prepare("https://gothstack.com", lessonFixture(t, 2, plan.File{Path: "Main.go", Content: "starter"})); e == nil {
		t.Fatal("baseline case collision")
	}
	if _, e := w.Prepare("https://gothstack.com", lessonFixture(t, 2, plan.File{Path: "main.go/nested", Content: "starter"})); e == nil {
		t.Fatal("baseline ancestor collision")
	}
	if e := os.WriteFile(filepath.Join(dir, "Unknown.go"), []byte("learner"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := w.Prepare("https://gothstack.com", lessonFixture(t, 2, plan.File{Path: "unknown.go", Content: "starter"})); e == nil {
		t.Fatal("disk case collision")
	}
}
func TestGitCommitAndIndexRemainUnchanged(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	if e := os.WriteFile(filepath.Join(dir, "learner.txt"), []byte("staged work"), 0644); e != nil {
		t.Fatal(e)
	}
	gitTest(t, dir, "add", "learner.txt")
	gitTest(t, dir, "commit", "-m", "initial")
	head := gitTest(t, dir, "rev-parse", "HEAD")
	if e := os.WriteFile(filepath.Join(dir, "learner.txt"), []byte("new staged work"), 0644); e != nil {
		t.Fatal(e)
	}
	gitTest(t, dir, "add", "learner.txt")
	indexBefore, e := os.ReadFile(filepath.Join(dir, ".git/index"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "learner.txt"), []byte("unstaged work"), 0644); e != nil {
		t.Fatal(e)
	}
	applyTest(t, w, lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"}))
	indexAfter, e := os.ReadFile(filepath.Join(dir, ".git/index"))
	if e != nil || string(indexBefore) != string(indexAfter) {
		t.Fatal("index changed", e)
	}
	if head != gitTest(t, dir, "rev-parse", "HEAD") {
		t.Fatal("commit changed")
	}
	readTest(t, dir, "learner.txt", "unstaged work")
}

func TestWorkspaceRecoveryRejectsUnexpectedStage(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: "starter"})
	w.afterWrite = func(int) error { return ErrRecovery }
	ready, _ := w.Prepare("https://gothstack.com", p)
	if e := ready.Apply(context.Background(), p); !errors.Is(e, ErrRecovery) {
		t.Fatal(e)
	}
	w.Close()
	if e := os.WriteFile(filepath.Join(dir, stagePath), []byte("unexpected saved material"), 0600); e != nil {
		t.Fatal(e)
	}
	if next, e := Open(context.Background(), dir); !errors.Is(e, ErrRecovery) {
		if next != nil {
			next.Close()
		}
		t.Fatal(e)
	}
	readTest(t, dir, stagePath, "unexpected saved material")
}
func TestWorkspaceRejectsGitSymlinkAndMetadataTraversal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix link fixture")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(dir, ".git")); e != nil {
		t.Fatal(e)
	}
	if w, e := Open(context.Background(), dir); e == nil {
		w.Close()
		t.Fatal("linked git accepted")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("outside git mutated")
	}
	dir = t.TempDir()
	w := openTest(t, dir)
	w.Close()
	raw, _ := json.Marshal(journal{1, []change{{Path: "../../escape", After: snapshot{true, []byte("x"), 0600}}}})
	if e = os.WriteFile(filepath.Join(dir, journalPath), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if w, e := Open(context.Background(), dir); !errors.Is(e, ErrRecovery) {
		if w != nil {
			w.Close()
		}
		t.Fatal(e)
	}
}

func TestWorkspaceLargeJournalRecovery(t *testing.T) {
	dir := t.TempDir()
	w := openTest(t, dir)
	p := lessonFixture(t, 1, plan.File{Path: "main.go", Content: strings.Repeat("// starter\n", 4000)})
	w.afterWrite = func(int) error { return ErrRecovery }
	ready, e := w.Prepare("https://gothstack.com", p)
	if e != nil {
		t.Fatal(e)
	}
	if e = ready.Apply(context.Background(), p); !errors.Is(e, ErrRecovery) {
		t.Fatal(e)
	}
	w.Close()
	w = openTest(t, dir)
	if _, e = os.Stat(filepath.Join(dir, "main.go")); !os.IsNotExist(e) {
		t.Fatal("large journal did not roll back", e)
	}
	applyTest(t, w, p)
	readTest(t, dir, "main.go", p.Missions[0].Plan.Files[0].Content)
}
