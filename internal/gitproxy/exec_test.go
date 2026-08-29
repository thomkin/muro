package gitproxy

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// initRepo creates a real git repo at dir on branch `branch` with one
// commit, using the host's real git — this package's tests intentionally
// exercise real git subprocesses (git rev-parse, git push --dry-run), not
// mocks, matching this codebase's established preference for real
// subprocess integration over faked ones wherever cheap enough to do for
// real.
func initRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runOK(t, dir, "init", "-q", "-b", branch)
	runOK(t, dir, "config", "user.email", "test@example.com")
	runOK(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, dir, "add", "README.md")
	runOK(t, dir, "commit", "-q", "-m", "initial")
}

func TestCheckCurrentBranch(t *testing.T) {
	dir := t.TempDir()
	initRepo(t, dir, "ai")

	if err := CheckCurrentBranch(context.Background(), dir, []string{"ai", "ai/*"}); err != nil {
		t.Fatalf("expected branch ai to be allowed: %v", err)
	}
	if err := CheckCurrentBranch(context.Background(), dir, []string{"main"}); err == nil {
		t.Fatal("expected rejection: current branch ai does not match allowed pattern main")
	}
}

func TestParsePushPorcelain(t *testing.T) {
	sample := "To /tmp/bare.git\n" +
		"*\trefs/heads/ai:refs/heads/ai\t[new branch]\n" +
		"Done\n"
	updates, err := ParsePushPorcelain(sample)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(updates) != 1 || updates[0].Flag != "*" || updates[0].To != "refs/heads/ai" {
		t.Fatalf("got %+v", updates)
	}

	rejected := "To /tmp/bare.git\n" +
		"!\trefs/heads/ai:refs/heads/main\t[remote rejected] (main is protected)\n"
	updates, err = ParsePushPorcelain(rejected)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(updates) != 1 || updates[0].Flag != "!" {
		t.Fatalf("got %+v", updates)
	}

	noPrefix := "=\tmain:main\tup to date\n"
	updates, err = ParsePushPorcelain(noPrefix)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(updates) != 1 || updates[0].To != "main" {
		t.Fatalf("got %+v", updates)
	}
}

func TestCheckPushPlan(t *testing.T) {
	allowed := []string{"ai", "ai/*"}

	ok := []RefUpdate{{Flag: "*", To: "refs/heads/ai"}}
	if err := CheckPushPlan(ok, allowed); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	rejected := []RefUpdate{{Flag: "!", To: "refs/heads/ai", Summary: "boom"}}
	if err := CheckPushPlan(rejected, allowed); err == nil {
		t.Fatal("expected error for a git-rejected ref")
	}

	wrongBranch := []RefUpdate{{Flag: "*", To: "refs/heads/main"}}
	if err := CheckPushPlan(wrongBranch, allowed); err == nil {
		t.Fatal("expected error for a disallowed destination branch")
	}

	if err := CheckPushPlan(nil, allowed); err == nil {
		t.Fatal("expected error for an empty push plan")
	}
}

// TestPushEndToEnd exercises the real path: RunGit's dry-run against a
// real local bare "remote," parsed and checked, then a real push — and
// separately, a policy-violating branch rejected by CheckPushPlan before
// any real push would occur.
func TestPushEndToEnd(t *testing.T) {
	remoteDir := t.TempDir()
	runOK(t, remoteDir, "init", "-q", "--bare")

	workDir := t.TempDir()
	initRepo(t, workDir, "ai")
	runOK(t, workDir, "remote", "add", "origin", remoteDir)

	ctx := context.Background()
	allowed := []string{"ai", "ai/*"}

	dryOut, dryErr, dryExit, err := RunGit(ctx, workDir, pushArgsWithDryRun([]string{"push", "origin", "ai"}))
	if err != nil || dryExit != 0 {
		t.Fatalf("dry-run push failed: err=%v exit=%d stderr=%s", err, dryExit, dryErr)
	}
	updates, err := ParsePushPorcelain(dryOut)
	if err != nil {
		t.Fatalf("parse porcelain: %v", err)
	}
	if err := CheckPushPlan(updates, allowed); err != nil {
		t.Fatalf("push plan should be allowed: %v", err)
	}

	// Real push.
	_, stderr, exitCode, err := RunGit(ctx, workDir, []string{"push", "origin", "ai"})
	if err != nil || exitCode != 0 {
		t.Fatalf("real push failed: err=%v exit=%d stderr=%s", err, exitCode, stderr)
	}

	// The bare "remote" must now have the ai branch.
	out := runOK(t, remoteDir, "branch", "--list", "ai")
	if !strings.Contains(out, "ai") {
		t.Fatalf("expected ai branch on remote, got %q", out)
	}

	// Policy violation: pushing to a "main" destination must be rejected
	// by CheckPushPlan without ever touching the real push.
	dryOut2, _, dryExit2, err := RunGit(ctx, workDir, pushArgsWithDryRun([]string{"push", "origin", "ai:main"}))
	if err != nil || dryExit2 != 0 {
		t.Fatalf("dry-run push (main) failed: err=%v exit=%d", err, dryExit2)
	}
	updates2, err := ParsePushPorcelain(dryOut2)
	if err != nil {
		t.Fatalf("parse porcelain: %v", err)
	}
	if err := CheckPushPlan(updates2, allowed); err == nil {
		t.Fatal("expected CheckPushPlan to reject a push targeting main")
	}
}
