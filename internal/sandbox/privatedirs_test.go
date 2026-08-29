package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirMounts_CreatesHostDirsAndRWMounts(t *testing.T) {
	stateDir := t.TempDir()

	mounts, err := PrivateDirMounts(stateDir, "sb_test1", []string{"/home/agent/.claude/projects"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1: %+v", len(mounts), mounts)
	}
	m := mounts[0]
	if m.SandboxPath != "/home/agent/.claude/projects" || m.Mode != "rw" {
		t.Errorf("got %+v, want sandbox_path=/home/agent/.claude/projects mode=rw", m)
	}
	if info, err := os.Stat(m.Host); err != nil || !info.IsDir() {
		t.Errorf("expected %q to exist as a directory: %v", m.Host, err)
	}
}

func TestPrivateDirMounts_IdempotentSameSandboxID(t *testing.T) {
	stateDir := t.TempDir()

	first, err := PrivateDirMounts(stateDir, "sb_same", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the agent writing something, proving the directory persists
	// and isn't recreated empty on a second resolve for the same sandbox ID
	// (the whole point — a restart must not wipe it).
	marker := filepath.Join(first[0].Host, "marker.txt")
	if err := os.WriteFile(marker, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := PrivateDirMounts(stateDir, "sb_same", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Host != first[0].Host {
		t.Fatalf("host path changed across calls for the same sandbox ID: %q vs %q", first[0].Host, second[0].Host)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker file lost on second resolve — private dir was recreated instead of reused: %v", err)
	}
}

func TestPrivateDirMounts_DifferentSandboxIDsGetIsolatedDirs(t *testing.T) {
	stateDir := t.TempDir()

	a, err := PrivateDirMounts(stateDir, "sb_a", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := PrivateDirMounts(stateDir, "sb_b", []string{"/data"})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].Host == b[0].Host {
		t.Errorf("two different sandbox IDs resolved to the same private host directory: %q", a[0].Host)
	}
}

func TestRemovePrivateDirs_DeletesEverythingForThatSandbox(t *testing.T) {
	stateDir := t.TempDir()

	mounts, err := PrivateDirMounts(stateDir, "sb_gone", []string{"/data", "/cache"})
	if err != nil {
		t.Fatal(err)
	}

	if err := RemovePrivateDirs(stateDir, "sb_gone"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, m := range mounts {
		if _, err := os.Stat(m.Host); !os.IsNotExist(err) {
			t.Errorf("expected %q to be gone after RemovePrivateDirs, stat err = %v", m.Host, err)
		}
	}
}

func TestRemovePrivateDirs_MissingIsNotAnError(t *testing.T) {
	stateDir := t.TempDir()
	if err := RemovePrivateDirs(stateDir, "sb_never_existed"); err != nil {
		t.Errorf("unexpected error removing a sandbox with no private dirs: %v", err)
	}
}
