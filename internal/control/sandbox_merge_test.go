package control

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
)

// newWorktreeTestServer is newTestServer (server_test.go) plus a real
// stateDir set on the Manager — worktree creation needs a real absolute
// path to put worktrees under (internal/sandbox.WorktreeMounts), and
// newTestServer's own Manager never calls SetStateDir, which would
// otherwise resolve worktree paths relative to wherever `go test` happens
// to run from.
func newWorktreeTestServer(t *testing.T) (srv *Server, socketPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MURO_PROFILES_DIR", dir)

	store := state.NewStore(filepath.Join(dir, "state.json"))
	if err := store.Load(); err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	stateDir = t.TempDir()
	mgr := sandbox.NewManager(store, &fakeIsolator{}, nil, nil)
	mgr.SetStateDir(stateDir)
	srv = NewServer(mgr, store, nil)

	socketPath = filepath.Join(dir, "control.sock")
	go func() {
		if err := srv.ListenAndServe(socketPath); err != nil {
			t.Logf("ListenAndServe: %v", err)
		}
	}()
	t.Cleanup(func() { _ = srv.Close() })

	waitForSocket(t, socketPath)
	return srv, socketPath, stateDir
}

func runGitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

func initTestRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runGitOK(t, dir, "init", "-q", "-b", branch)
	runGitOK(t, dir, "config", "user.email", "test@example.com")
	runGitOK(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, dir, "add", "README.md")
	runGitOK(t, dir, "commit", "-q", "-m", "initial")
}

func saveWorktreeTestProfile(t *testing.T, name, repoHost, mountPath string) {
	t.Helper()
	if err := config.SaveProfile(&config.Profile{
		Name:          name,
		Agent:         "true",
		RestartPolicy: "never",
		Git: config.GitPolicy{Repos: []config.GitRepoPolicy{
			{Host: repoHost, Worktree: true, MountPath: mountPath},
		}},
	}); err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
}

func TestSandboxMerge_EndToEnd(t *testing.T) {
	_, socketPath, _ := newWorktreeTestServer(t)

	repo := t.TempDir()
	initTestRepo(t, repo, "main")
	saveWorktreeTestProfile(t, "wt-profile", repo, "/workspace/foo")

	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var ran SandboxView
	runReq := SandboxRunRequest{Profile: "wt-profile", Name: "agent-1", Namespace: "default"}
	if err := c.Call(TypeSandboxRun, runReq, &ran); err != nil {
		t.Fatalf("sandbox.run: %v", err)
	}
	if len(ran.Worktrees) != 1 {
		t.Fatalf("Worktrees = %+v, want 1 entry", ran.Worktrees)
	}
	wt := ran.Worktrees[0]
	if wt.HasUnmergedCommits {
		t.Error("expected no unmerged commits right after run")
	}

	// Commit inside the worktree, the same way the git tool-proxy would on
	// the agent's behalf.
	if err := os.WriteFile(filepath.Join(wt.Host, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, wt.Host, "add", "feature.txt")
	runGitOK(t, wt.Host, "commit", "-q", "-m", "add feature")

	var shown SandboxView
	if err := c.Call(TypeSandboxShow, SandboxShowRequest{Name: "agent-1"}, &shown); err != nil {
		t.Fatalf("sandbox.show: %v", err)
	}
	if len(shown.Worktrees) != 1 || !shown.Worktrees[0].HasUnmergedCommits {
		t.Fatalf("expected sandbox.show to report unmerged commits after committing, got %+v", shown.Worktrees)
	}

	var stopResp SandboxStopResponse
	if err := c.Call(TypeSandboxStop, SandboxStopRequest{Name: "agent-1"}, &stopResp); err != nil {
		t.Fatalf("sandbox.stop: %v", err)
	}

	// Delete must be refused while unmerged.
	if err := c.Call(TypeSandboxDelete, SandboxDeleteRequest{Name: "agent-1"}, nil); err == nil {
		t.Fatal("expected sandbox.delete to be refused with unmerged worktree commits")
	}

	var mergeResp SandboxMergeResponse
	mergeReq := SandboxMergeRequest{Name: "agent-1", MountPath: "/workspace/foo", Message: "Add feature"}
	if err := c.Call(TypeSandboxMerge, mergeReq, &mergeResp); err != nil {
		t.Fatalf("sandbox.merge: %v", err)
	}
	if !mergeResp.OK || mergeResp.Commit == "" {
		t.Fatalf("mergeResp = %+v, want OK with a non-empty commit", mergeResp)
	}
	content, err := os.ReadFile(filepath.Join(repo, "feature.txt"))
	if err != nil {
		t.Fatalf("feature.txt missing from real repo after merge: %v", err)
	}
	if string(content) != "feature\n" {
		t.Errorf("feature.txt content = %q, want the merged content", content)
	}

	// Delete should now succeed — nothing left to protect.
	if err := c.Call(TypeSandboxDelete, SandboxDeleteRequest{Name: "agent-1"}, nil); err != nil {
		t.Fatalf("sandbox.delete after merge: %v", err)
	}
}

func TestSandboxMerge_MissingMountPathRejected(t *testing.T) {
	_, socketPath, _ := newWorktreeTestServer(t)
	saveTestProfile(t, "plain-profile")

	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var ran SandboxView
	if err := c.Call(TypeSandboxRun, SandboxRunRequest{Profile: "plain-profile", Name: "agent-1"}, &ran); err != nil {
		t.Fatalf("sandbox.run: %v", err)
	}

	err = c.Call(TypeSandboxMerge, SandboxMergeRequest{Name: "agent-1", Message: "msg"}, nil)
	if err == nil {
		t.Fatal("expected sandbox.merge to reject a request with no mount_path")
	}
	if !strings.Contains(err.Error(), "mount_path") {
		t.Errorf("error = %v, want it to mention mount_path", err)
	}
}

func TestSandboxDelete_DiscardWorktreeFlagThreadedThroughControlAPI(t *testing.T) {
	_, socketPath, _ := newWorktreeTestServer(t)

	repo := t.TempDir()
	initTestRepo(t, repo, "main")
	saveWorktreeTestProfile(t, "wt-profile", repo, "/workspace/foo")

	c, err := Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.Close()

	var ran SandboxView
	if err := c.Call(TypeSandboxRun, SandboxRunRequest{Profile: "wt-profile", Name: "agent-1"}, &ran); err != nil {
		t.Fatalf("sandbox.run: %v", err)
	}
	wt := ran.Worktrees[0]
	if err := os.WriteFile(filepath.Join(wt.Host, "feature.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitOK(t, wt.Host, "add", "feature.txt")
	runGitOK(t, wt.Host, "commit", "-q", "-m", "wip")

	if err := c.Call(TypeSandboxStop, SandboxStopRequest{Name: "agent-1"}, nil); err != nil {
		t.Fatalf("sandbox.stop: %v", err)
	}

	deleteReq := SandboxDeleteRequest{Name: "agent-1", DiscardWorktrees: []string{"/workspace/foo"}}
	if err := c.Call(TypeSandboxDelete, deleteReq, nil); err != nil {
		t.Fatalf("sandbox.delete with discard_worktrees: %v", err)
	}
	if _, err := os.Stat(wt.Host); !os.IsNotExist(err) {
		t.Errorf("expected worktree to be discarded, stat err = %v", err)
	}
}
