package state

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Reconcile checks every sandbox the Store believes is StateRunning against
// the live process table and marks anything whose recorded PID is no
// longer alive as StateStopped. This runs once at murod startup so a crash
// while the daemon was down doesn't leave stale "running" entries in
// state.json (DESIGN.md §5).
//
// Liveness is checked with the standard portable approach on Unix: sending
// signal 0 to the PID via os.Process.Signal, which performs the kernel's
// existence/permission check without actually delivering a signal. This
// intentionally does not go further to verify the PID is actually a bwrap
// process (e.g. by inspecting /proc/<pid>/cmdline) — PID reuse by an
// unrelated process in the narrow window between a murod crash and its
// restart is an accepted, very unlikely edge case for v1; the important
// property is that a PID no longer alive at all is always caught.
func Reconcile(store *Store) error {
	for _, sb := range store.List("") {
		if sb.State != StateRunning {
			continue
		}
		if isAlive(sb.PID) {
			continue
		}
		sb.State = StateStopped
		if err := store.Put(sb); err != nil {
			return fmt.Errorf("reconcile %s/%s: %w", sb.Namespace, sb.Name, err)
		}
	}
	return nil
}

// isAlive reports whether pid refers to a currently running process.
func isAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Unix, os.FindProcess never fails to find a PID (it always
		// succeeds and returns a Process for any given pid), so this
		// branch is effectively unreachable in practice but handled for
		// safety.
		return false
	}
	// Signal 0: no signal is actually delivered, but the kernel still does
	// its existence/permission check. ESRCH means the process is
	// definitively gone. EPERM means it exists but murod isn't allowed to
	// signal it (e.g. a differently-owned process reusing the PID) — that
	// still counts as "alive" here, since the only thing being asked is
	// whether the PID is gone, not whether murod could kill it.
	//
	// Since Go 1.23, os.FindProcess/Signal on Linux can detect that a
	// process is already gone at call time (rather than only discovering it
	// via the signal syscall itself) and return the sentinel
	// os.ErrProcessDone instead of a raw syscall.ESRCH — both must be
	// treated as "dead" here, or an already-reaped PID is wrongly reported
	// alive.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errorsIsDead(err)
}

func errorsIsDead(err error) bool {
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}
