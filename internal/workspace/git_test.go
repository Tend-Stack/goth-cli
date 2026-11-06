package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=Test", "-c", "user.email=test@localhost"}, args...)...)
	c.Dir = dir
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return string(b)
}
func TestGitBranchTransactions(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g, e := newGit(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	if e = g.initialize(ctx); e != nil {
		t.Fatal(e)
	}
	for _, branch := range []string{"goth-first-v1", "goth-first-v1", "goth-next-v1"} {
		if e = g.switchBranch(ctx, branch); e != nil {
			t.Fatalf("unborn %s: %v", branch, e)
		}
	}
	gitTest(t, dir, "commit", "--allow-empty", "-m", "initial")
	if e = os.WriteFile(filepath.Join(dir, "learner.txt"), []byte("my work"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, branch := range []string{"goth-committed-v1", "goth-committed-v1", "goth-next-v1"} {
		if e = g.switchBranch(ctx, branch); e != nil {
			t.Fatalf("committed %s: %v", branch, e)
		}
	}
	gitTest(t, dir, "commit", "--allow-empty", "-m", "next")
	if e = g.switchBranch(ctx, "goth-committed-v1"); e != ErrBranch {
		t.Fatalf("different branch: %v", e)
	}
	gitTest(t, dir, "checkout", "--detach")
	if e = g.switchBranch(ctx, "goth-detached-v1"); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "learner.txt"))
	if string(b) != "my work" {
		t.Fatal("learner data changed")
	}
}
func TestGitDisablesHooksAndEnvironmentOverrides(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g, e := newGit(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer g.close()
	if e = g.initialize(ctx); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(dir, ".git", "hooks"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, ".git", "hooks", "reference-transaction"), []byte("#!/bin/sh\nexit 42\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("GIT_DIR", t.TempDir())
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
	t.Setenv("GIT_CONFIG_VALUE_0", filepath.Join(dir, ".git", "hooks"))
	if e = g.switchBranch(ctx, "goth-safe-v1"); e != nil {
		t.Fatal(e)
	}
}
