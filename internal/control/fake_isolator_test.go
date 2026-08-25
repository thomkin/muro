package control

import (
	"context"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
)

// fakeIsolator is a trivial in-memory sandbox.Isolator for exercising the
// control server end-to-end without a real bwrap-backed sandbox — this
// package's tests care about the JSON protocol/dispatch logic, not real
// kernel-level isolation (that's covered separately by
// test/integration against the real BwrapIsolator).
type fakeIsolator struct {
	mu   sync.Mutex
	next int
}

type fakeHandle struct {
	pid    int
	exitCh chan int
	// pty is one end of a real bidirectional socketpair (syscall.Socketpair),
	// wrapped as *os.File to satisfy Handle.Stdio()'s signature. Using a
	// real bidirectional fd (rather than a one-directional os.Pipe) lets
	// attach tests exercise genuine read/write passthrough, matching what
	// a real pty master fd behaves like.
	pty *os.File
	// peer is the other end, kept by the test to act as "the agent side"
	// of the fake pty — writing simulated agent output and reading
	// forwarded client input.
	peer *os.File
}

func (h *fakeHandle) PID() int                          { return h.pid }
func (h *fakeHandle) Wait() (int, error)                { return <-h.exitCh, nil }
func (h *fakeHandle) Stdio() (io.ReadWriteCloser, bool) { return h.pty, h.pty != nil }

func newFakeHandle(pid int, withPTY bool) (*fakeHandle, error) {
	h := &fakeHandle{pid: pid, exitCh: make(chan int, 1)}
	if !withPTY {
		return h, nil
	}
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	h.pty = os.NewFile(uintptr(fds[0]), "fake-pty")
	h.peer = os.NewFile(uintptr(fds[1]), "fake-pty-peer")
	return h, nil
}

func (f *fakeIsolator) Launch(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Handle, error) {
	f.mu.Lock()
	f.next++
	pid := 10000 + f.next
	f.mu.Unlock()
	return newFakeHandle(pid, spec.PTY)
}

func (f *fakeIsolator) UpdateMounts(_ sandbox.Handle, mounts []config.Mount) (bool, error) {
	// Match the real BwrapIsolator's documented behavior: a live mount
	// namespace can't gain/lose bind mounts, so any actual mount change
	// is reported as not hot-applicable.
	return len(mounts) == 0, nil
}

func (f *fakeIsolator) Stop(h sandbox.Handle) error {
	fh := h.(*fakeHandle)
	select {
	case fh.exitCh <- 0:
	default:
	}
	if fh.pty != nil {
		_ = fh.pty.Close()
	}
	if fh.peer != nil {
		_ = fh.peer.Close()
	}
	return nil
}
