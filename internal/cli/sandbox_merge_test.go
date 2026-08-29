package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thomkin/muro/internal/control"
)

func TestSelectWorktree_SingleWorktreeNoFlagReturnsIt(t *testing.T) {
	worktrees := []control.WorktreeView{{MountPath: "/workspace/foo"}}
	got, err := selectWorktree(worktrees, "", "default", "agent-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MountPath != "/workspace/foo" {
		t.Errorf("MountPath = %q, want /workspace/foo", got.MountPath)
	}
}

func TestSelectWorktree_NoWorktreesIsAnError(t *testing.T) {
	if _, err := selectWorktree(nil, "", "default", "agent-1"); err == nil {
		t.Fatal("expected an error for a sandbox with no worktrees")
	}
}

func TestSelectWorktree_MultipleWithoutRepoFlagIsAnError(t *testing.T) {
	worktrees := []control.WorktreeView{{MountPath: "/workspace/foo"}, {MountPath: "/workspace/bar"}}
	_, err := selectWorktree(worktrees, "", "default", "agent-1")
	if err == nil {
		t.Fatal("expected an error when multiple worktrees exist and --repo is not given")
	}
	if !strings.Contains(err.Error(), "/workspace/foo") || !strings.Contains(err.Error(), "/workspace/bar") {
		t.Errorf("error = %v, want it to list both available mount paths", err)
	}
}

func TestSelectWorktree_RepoFlagPicksTheMatchingOne(t *testing.T) {
	worktrees := []control.WorktreeView{{MountPath: "/workspace/foo"}, {MountPath: "/workspace/bar"}}
	got, err := selectWorktree(worktrees, "/workspace/bar", "default", "agent-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.MountPath != "/workspace/bar" {
		t.Errorf("MountPath = %q, want /workspace/bar", got.MountPath)
	}
}

func TestSelectWorktree_RepoFlagNotFoundIsAnError(t *testing.T) {
	worktrees := []control.WorktreeView{{MountPath: "/workspace/foo"}}
	if _, err := selectWorktree(worktrees, "/workspace/nonexistent", "default", "agent-1"); err == nil {
		t.Fatal("expected an error for a --repo value that doesn't match any worktree")
	}
}

// setEditor points $EDITOR at a small shell script for the duration of the
// test, so editMergeMessage's real $EDITOR invocation (openInEditor) can be
// exercised without an actual interactive editor.
func setEditor(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EDITOR", path)
}

func TestEditMergeMessage_UnmodifiedDraftSurvivesCommentStripping(t *testing.T) {
	setEditor(t, `exit 0`) // leaves the file exactly as written

	msg, err := editMergeMessage("Add feature\n\nDetails here.", "diff --git a/x b/x\n+added line", "agent/default/a1", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(msg, "Add feature") {
		t.Errorf("message = %q, want it to start with the draft subject", msg)
	}
	if !strings.Contains(msg, "Details here.") {
		t.Errorf("message = %q, want the draft body kept", msg)
	}
	if strings.Contains(msg, "diff --git") || strings.Contains(msg, "added line") {
		t.Errorf("message = %q, the diff comment block must be stripped, not included in the final message", msg)
	}
	if strings.Contains(msg, "#") {
		t.Errorf("message = %q, no comment markers should survive", msg)
	}
}

func TestEditMergeMessage_EditorCanReplaceTheMessage(t *testing.T) {
	setEditor(t, `printf 'A totally different message\n' > "$1"`)

	msg, err := editMergeMessage("original draft", "some diff", "agent/default/a1", "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg != "A totally different message" {
		t.Errorf("message = %q, want the operator's edited replacement", msg)
	}
}

func TestEditMergeMessage_BlankMessageIsAnError(t *testing.T) {
	setEditor(t, `: > "$1"`) // truncate to empty

	if _, err := editMergeMessage("draft", "diff", "agent/default/a1", "main"); err == nil {
		t.Fatal("expected an error for a blank merge message")
	}
}

func TestEditMergeMessage_OnlyCommentsLeftIsAnError(t *testing.T) {
	// The editor leaves only comment lines (e.g. the operator deleted the
	// real message but left the diff context) — must be treated the same
	// as blank, not as "the diff is the message."
	setEditor(t, `printf '# just a comment\n' > "$1"`)

	if _, err := editMergeMessage("draft", "diff", "agent/default/a1", "main"); err == nil {
		t.Fatal("expected an error when only comment lines remain")
	}
}
