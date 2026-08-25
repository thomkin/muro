package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thomkin/muro/internal/config"
)

const bwrapBinary = "bwrap"

// BwrapIsolator is the v1 Isolator implementation: it execs the bwrap
// (bubblewrap) binary rather than driving Linux namespace syscalls
// directly (DESIGN.md §6.1). Filesystem access is deny-by-default — a
// launched sandbox sees only the mounts explicitly listed in its
// LaunchSpec, plus the minimal /proc, /dev, /tmp scaffolding needed to run
// at all.
type BwrapIsolator struct {
	bwrapPath string
}

// NewBwrapIsolator checks that bwrap is installed and that unprivileged
// user namespaces actually work on this host (DESIGN.md §8), returning a
// clear, actionable error otherwise rather than letting a launch fail with
// a cryptic bwrap error later.
func NewBwrapIsolator() (*BwrapIsolator, error) {
	path, err := exec.LookPath(bwrapBinary)
	if err != nil {
		return nil, fmt.Errorf("bwrap not found on PATH: install bubblewrap (see DESIGN.md §8): %w", err)
	}
	if err := checkUnprivilegedUserns(path); err != nil {
		return nil, err
	}
	return &BwrapIsolator{bwrapPath: path}, nil
}

// checkUnprivilegedUserns actually runs a minimal bwrap invocation rather
// than trusting /proc/sys/kernel/unprivileged_userns_clone alone — that
// sysctl doesn't exist on every distro, and even where it reads "1" some
// distros (e.g. via an AppArmor profile) still block unprivileged user
// namespaces for unrelated reasons. A real smoke test is the only check
// that can't be fooled either way. The sysctl is still read first, purely
// to give a more specific error message in the common (Debian-family)
// case where it's the definitive cause.
func checkUnprivilegedUserns(bwrapPath string) error {
	cmd := exec.Command(bwrapPath, "--unshare-user", "--unshare-pid", "--ro-bind", "/", "/", "/bin/true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		return nil
	} else {
		if data, rerr := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone"); rerr == nil &&
			strings.TrimSpace(string(data)) == "0" {
			return fmt.Errorf("unprivileged user namespaces are disabled (kernel.unprivileged_userns_clone=0) — enable with: sudo sysctl -w kernel.unprivileged_userns_clone=1 (see DESIGN.md §8)")
		}
		return fmt.Errorf("bwrap smoke test failed, unprivileged user namespaces may be unavailable on this host (see DESIGN.md §8): %w: %s", err, stderr.String())
	}
}

// buildArgs turns a LaunchSpec into bwrap's argv. It's a pure function
// deliberately kept separate from Launch so it can be unit-tested without
// actually running bwrap.
func (b *BwrapIsolator) buildArgs(spec LaunchSpec) []string {
	args := []string{
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--unshare-cgroup",
		"--unshare-net",
		"--die-with-parent",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}

	for _, m := range spec.Mounts {
		if m.Mode == "rw" {
			args = append(args, "--bind", m.Host, m.SandboxPath)
		} else {
			// Deny-by-default: anything not explicitly "rw" (including an
			// empty/unset Mode) is read-only, never a silent read-write.
			args = append(args, "--ro-bind", m.Host, m.SandboxPath)
		}
	}

	// Sorted for deterministic argv — makes buildArgs's output testable
	// and bwrap invocations reproducible/loggable.
	envKeys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		args = append(args, "--setenv", k, spec.Env[k])
	}

	args = append(args, "--chdir", "/")
	args = append(args, "--")
	args = append(args, spec.Cmd...)
	return args
}

// Launch starts one sandboxed process via bwrap. Network isolation here is
// real and total: --unshare-net gives the sandbox a fresh, unconfigured
// network namespace (not even a usable loopback) with no bridge back to
// anything. Wiring the sandbox's HTTP_PROXY/HTTPS_PROXY toward a
// per-sandbox loopback address reachable from the proxy (DESIGN.md §6.2)
// is cmd/murod's job when it assembles Isolator + proxy.Server together —
// this package only guarantees the isolation is real, not the routing that
// later selectively bridges it.
func (b *BwrapIsolator) Launch(ctx context.Context, spec LaunchSpec) (Handle, error) {
	args := b.buildArgs(spec)
	cmd := exec.CommandContext(ctx, b.bwrapPath, args...)

	h := &bwrapHandle{cmd: cmd}

	if spec.PTY {
		ptmx, pts, err := openPTY()
		if err != nil {
			return nil, fmt.Errorf("allocate pty: %w", err)
		}
		cmd.Stdin = pts
		cmd.Stdout = pts
		cmd.Stderr = pts
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0, // fd 0 in the child, i.e. stdin — which is pts
		}
		if err := cmd.Start(); err != nil {
			ptmx.Close()
			pts.Close()
			return nil, fmt.Errorf("start bwrap: %w", err)
		}
		// The child has its own dup of pts now; our copy is only needed
		// until Start returns.
		pts.Close()
		h.pty = ptmx
	} else {
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start bwrap: %w", err)
		}
	}

	return h, nil
}

// UpdateMounts always reports applied=false for a non-empty mount delta:
// bwrap builds a mount namespace once at process start and has no
// supported way to add or remove bind mounts on a running one (DESIGN.md
// §6.3), so any real mount change here needs a restart — Manager already
// handles that signal (marking the sandbox reload-pending). An empty delta
// is a legitimate no-op and reports applied=true.
func (b *BwrapIsolator) UpdateMounts(h Handle, mounts []config.Mount) (bool, error) {
	if len(mounts) == 0 {
		return true, nil
	}
	return false, nil
}

// Stop signals bwrap (SIGTERM, escalating to SIGKILL after a short grace
// period) rather than the sandboxed process directly. bwrap runs as PID 1
// of the sandbox's own PID namespace (--unshare-pid); killing it tears
// down the whole namespace and every process in it, so there's nothing
// else to individually signal.
func (b *BwrapIsolator) Stop(h Handle) error {
	bh, ok := h.(*bwrapHandle)
	if !ok {
		return fmt.Errorf("stop: not a bwrap handle")
	}
	proc := bh.cmd.Process
	if proc == nil {
		return nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if isProcessDeadErr(err) {
			return nil
		}
		return fmt.Errorf("signal bwrap (SIGTERM): %w", err)
	}

	// Route through bh.Wait (not bh.cmd.Wait directly): Manager's watch
	// loop also calls Handle.Wait on this same handle after Launch, and
	// exec.Cmd.Wait must only ever be invoked once — bh.Wait's sync.Once
	// makes this call and the watch loop's call safe to race against each
	// other, whichever gets there first.
	done := make(chan struct{})
	go func() {
		bh.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessDeadErr(err) {
			return fmt.Errorf("signal bwrap (SIGKILL): %w", err)
		}
		<-done
		return nil
	}
}

func isProcessDeadErr(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// bwrapHandle implements Handle by wrapping the *exec.Cmd started for one
// bwrap invocation.
type bwrapHandle struct {
	cmd *exec.Cmd
	pty *os.File // non-nil only if LaunchSpec.PTY was true

	waitOnce sync.Once
	exitCode int
	waitErr  error
}

func (h *bwrapHandle) PID() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *bwrapHandle) Wait() (int, error) {
	h.waitOnce.Do(func() {
		err := h.cmd.Wait()
		if h.pty != nil {
			h.pty.Close()
		}
		if err == nil {
			h.exitCode = 0
			h.waitErr = nil
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			h.exitCode = exitErr.ExitCode()
			h.waitErr = nil
			return
		}
		h.exitCode = -1
		h.waitErr = err
	})
	return h.exitCode, h.waitErr
}

func (h *bwrapHandle) Stdio() (*os.File, bool) {
	if h.pty == nil {
		return nil, false
	}
	return h.pty, true
}

// openPTY allocates a pseudo-terminal pair using only stdlib + golang.org/
// x/sys/unix (already a project dependency) — no third-party pty library.
// This is the standard dependency-free Linux sequence: open /dev/ptmx for
// the master, unlock it (TIOCSPTLCK) and read its slave number (TIOCGPTN),
// then open /dev/pts/<n> for the slave. Known limitation: this only
// supports Linux's /dev/ptmx multiplexer (not the BSD-style pty pairs some
// non-Linux Unixes use), which is fine given DESIGN.md §3's Linux-only
// scope, and it does not set an initial terminal size — Manager/the attach
// path is expected to send a resize once a real terminal is attached
// (DESIGN.md §12 mentions resize forwarding as part of attach, not launch).
func openPTY() (ptmx *os.File, pts *os.File, err error) {
	ptmx, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	if err := unix.IoctlSetPointerInt(int(ptmx.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", err)
	}

	n, err := unix.IoctlGetInt(int(ptmx.Fd()), unix.TIOCGPTN)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCGPTN: %w", err)
	}

	ptsName := fmt.Sprintf("/dev/pts/%d", n)
	pts, err = os.OpenFile(ptsName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		ptmx.Close()
		return nil, nil, fmt.Errorf("open %s: %w", ptsName, err)
	}

	return ptmx, pts, nil
}
