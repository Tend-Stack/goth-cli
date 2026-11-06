package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ErrGit = errors.New("could not prepare the lesson branch; use Git 2.48 or newer and check the repository")
var ErrBranch = errors.New("the lesson branch differs from the current commit; switch to that branch before starting")
var oidPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var gitVersion = regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)\.`)

type gitRepo struct{ dir, executable, hooks string }

func newGit(ctx context.Context, dir string) (*gitRepo, error) {
	exe, err := exec.LookPath("git")
	if err != nil {
		return nil, ErrGit
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, ErrGit
	}
	hooks, err := os.MkdirTemp("", "goth-empty-hooks-")
	if err != nil {
		return nil, ErrGit
	}
	g := &gitRepo{dir: dir, executable: exe, hooks: hooks}
	out, code, err := g.run(ctx, "", "--version")
	m := gitVersion.FindStringSubmatch(out)
	if err != nil || code != 0 || len(m) != 3 {
		g.close()
		return nil, ErrGit
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || major == 2 && minor < 48 {
		g.close()
		return nil, ErrGit
	}
	return g, nil
}
func (g *gitRepo) close() { _ = os.Remove(g.hooks) }

// Only fixed Git operations use this runner. Clear inherited Git overrides,
// disable hooks/global config and bound the trusted executable's process group.
// No checkout, filter, commit, fetch, push or project executable is invoked.
func (g *gitRepo) run(ctx context.Context, input string, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	base := []string{"-c", "core.hooksPath=" + g.hooks, "-c", "core.fsmonitor=false", "-c", "core.attributesFile=" + os.DevNull, "-c", "user.name=GoTHStack CLI", "-c", "user.email=cli@localhost"}
	cmd := exec.CommandContext(ctx, g.executable, append(base, args...)...)
	cmd.Dir = g.dir
	for _, item := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(item), "GIT_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = strings.NewReader(input)
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	boundGit(cmd)
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return strings.TrimSpace(out.text), exit.ExitCode(), nil
		}
		return "", -1, ErrGit
	}
	return strings.TrimSpace(out.text), 0, nil
}

type boundedOutput struct{ text string }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.text)+len(p) > 8192 {
		return 0, ErrGit
	}
	b.text += string(p)
	return len(p), nil
}
func (g *gitRepo) command(ctx context.Context, input string, args ...string) (string, int, error) {
	base := []string{"--git-dir=" + filepath.Join(g.dir, ".git"), "--work-tree=" + g.dir}
	return g.run(ctx, input, append(base, args...)...)
}
func (g *gitRepo) initialize(ctx context.Context) error {
	_, code, err := g.run(ctx, "", "init", "--initial-branch=main", "--template="+g.hooks, ".")
	if err != nil {
		return err
	}
	if code != 0 {
		return ErrGit
	}
	return nil
}

// switchBranch uses Git's transactional compare-and-swap for HEAD and refs.
// Existing branches must name the current commit; no files/index are checked out.
// https://git-scm.com/docs/git-update-ref/2.48.0
func (g *gitRepo) switchBranch(ctx context.Context, branch string) error {
	target := "refs/heads/" + branch
	_, code, err := g.run(ctx, "", "check-ref-format", target)
	if err != nil || code != 0 {
		return ErrGit
	}
	oldRef, refCode, err := g.command(ctx, "", "symbolic-ref", "--quiet", "HEAD")
	if err != nil || refCode != 0 && refCode != 1 {
		return ErrGit
	}
	if refCode == 0 {
		if !strings.HasPrefix(oldRef, "refs/heads/") {
			return ErrGit
		}
		_, code, err = g.run(ctx, "", "check-ref-format", oldRef)
		if err != nil || code != 0 {
			return ErrGit
		}
	}
	head, headCode, err := g.command(ctx, "", "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil || headCode != 0 && headCode != 1 || headCode == 0 && !oidPattern.MatchString(head) {
		return ErrGit
	}
	other, otherCode, err := g.command(ctx, "", "rev-parse", "--verify", "--quiet", target)
	if err != nil || otherCode != 0 && otherCode != 1 || otherCode == 0 && !oidPattern.MatchString(other) {
		return ErrGit
	}
	if otherCode == 0 && (headCode != 0 || other != head) {
		return ErrBranch
	}
	var commands strings.Builder
	commands.WriteString("start\n")
	if refCode == 0 && oldRef == target {
		commands.WriteString("option no-deref\nsymref-verify HEAD " + oldRef + "\n")
	} else {
		if otherCode == 1 && headCode == 0 {
			commands.WriteString("create " + target + " " + head + "\n")
		} else if otherCode == 0 {
			commands.WriteString("verify " + target + " " + other + "\n")
		} else {
			commands.WriteString("verify " + target + "\n")
		}
		commands.WriteString("option no-deref\nsymref-update HEAD " + target)
		if refCode == 0 {
			commands.WriteString(" ref " + oldRef + "\n")
		} else {
			if headCode != 0 {
				return ErrGit
			}
			commands.WriteString(" oid " + head + "\n")
		}
	}
	commands.WriteString("prepare\ncommit\n")
	_, code, err = g.command(ctx, commands.String(), "update-ref", "--stdin", "-m", fmt.Sprintf("goth start: %s", branch))
	if err != nil {
		return err
	}
	if code != 0 {
		return ErrGit
	}
	// Git cannot verify HEAD's old symbolic referent in the same transaction
	// (it rejects duplicate HEAD updates). Detect a concurrent commit before
	// touching starter files; both refs and all learner files remain available.
	if refCode == 0 && oldRef != target {
		actual, actualCode, actualErr := g.command(ctx, "", "rev-parse", "--verify", "--quiet", oldRef)
		if actualErr != nil || actualCode != headCode || actual != head {
			return ErrGit
		}
	}
	return nil
}
