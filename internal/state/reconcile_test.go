package state

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReconcileMarksDeadPIDStopped(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))

	// Run and wait on a short-lived process so its PID is guaranteed to be
	// reaped and no longer alive by the time Reconcile checks it.
	cmd := exec.Command("/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running /bin/true failed: %v", err)
	}
	deadPID := cmd.Process.Pid

	sb := testSandbox("default", "dead-one")
	sb.PID = deadPID
	sb.State = StateRunning
	if err := s.Put(sb); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	alive := testSandbox("default", "alive-one")
	alive.PID = 1 // init/systemd — always alive on any running Linux system
	alive.State = StateRunning
	if err := s.Put(alive); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	if err := Reconcile(s); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got, _ := s.Get("default", "dead-one")
	if got.State != StateStopped {
		t.Errorf("dead-one State = %q, want %q", got.State, StateStopped)
	}

	got, _ = s.Get("default", "alive-one")
	if got.State != StateRunning {
		t.Errorf("alive-one State = %q, want %q (pid 1 should be alive)", got.State, StateRunning)
	}
}

func TestReconcileIgnoresNonRunningSandboxes(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "state.json"))

	sb := testSandbox("default", "already-stopped")
	sb.PID = 999999 // near-certainly not a live PID
	sb.State = StateStopped
	if err := s.Put(sb); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	if err := Reconcile(s); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got, _ := s.Get("default", "already-stopped")
	if got.State != StateStopped {
		t.Errorf("State = %q, want unchanged %q", got.State, StateStopped)
	}
}
