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
//
// Network access is deny-by-default too, in the same spirit, layered on
// top of --unshare-net's total isolation: Launch bridges each sandbox to
// proxyAddr specifically (slirp4netns) and then restricts it to ONLY that
// destination (nftables) — DESIGN.md §8's slirp4netns host requirement and
// the egress-restriction decision from that same design discussion.
type BwrapIsolator struct {
	bwrapPath string
	proxyAddr string
	addrAlloc *outboundAddrAllocator
}

// NewBwrapIsolator checks that bwrap is installed and that unprivileged
// user namespaces actually work on this host (DESIGN.md §8), returning a
// clear, actionable error otherwise rather than letting a launch fail with
// a cryptic bwrap error later. proxyAddr is murod's URL-allowlist proxy
// listen address (host:port) — every sandbox this isolator launches is
// bridged toward exactly that destination and nothing else.
func NewBwrapIsolator(proxyAddr string) (*BwrapIsolator, error) {
	path, err := exec.LookPath(bwrapBinary)
	if err != nil {
		return nil, fmt.Errorf("bwrap not found on PATH: install bubblewrap (see DESIGN.md §8): %w", err)
	}
	if err := checkUnprivilegedUserns(path); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("slirp4netns"); err != nil {
		return nil, fmt.Errorf("slirp4netns not found on PATH: install it (see DESIGN.md §8) — without it, sandboxes have no route to murod's own proxy: %w", err)
	}
	if _, err := exec.LookPath("nsenter"); err != nil {
		return nil, fmt.Errorf("nsenter not found on PATH (util-linux) — needed to install per-sandbox egress restrictions: %w", err)
	}
	if _, err := exec.LookPath("nft"); err != nil {
		return nil, fmt.Errorf("nft not found on PATH (nftables) — needed to install per-sandbox egress restrictions: %w", err)
	}
	return &BwrapIsolator{bwrapPath: path, proxyAddr: proxyAddr, addrAlloc: newOutboundAddrAllocator()}, nil
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
		// --uid 0 --gid 0: presents the sandboxed process as uid/gid 0
		// *inside its own user namespace only* — bwrap's uid_map still maps
		// that back to the real, unprivileged host user (confirmed: "0
		// <host-uid> 1" in /proc/<pid>/uid_map), so this grants no actual
		// host privilege, the same safe pattern rootless Docker/Podman
		// containers use to present as "root" internally. This is required
		// for Stage 3's nftables egress restriction: `nft` refuses to
		// initialize its netlink cache ("Operation not permitted (you must
		// be root)") unless the calling process's *effective uid inside its
		// own namespace* is 0 — confirmed empirically; without --uid 0,
		// bwrap's default identity mapping (host uid N -> sandbox uid N,
		// never 0) makes nft refuse regardless of actual capabilities held.
		// Read-only/read-write mount enforcement is unaffected: bind-mount
		// flags (MS_RDONLY) are enforced by the kernel at the VFS layer
		// regardless of the calling process's uid, so this doesn't weaken
		// TestReadOnlyMountRejectsWrite's guarantee even though the
		// sandboxed process now nominally "is root".
		"--uid", "0",
		"--gid", "0",
		// Deliberately NOT --die-with-parent: that flag kills the sandboxed
		// process the instant its parent (murod, via os/exec) exits — which
		// includes a clean `murod` shutdown/restart, not just a crash.
		// IMPLEMENTATION.md §6 and cmd/murod's shutdown handler are explicit
		// that a daemon restart must never kill an already-running sandbox
		// (only `muro sandbox stop` should); state.Reconcile already handles
		// re-adopting a still-alive sandbox by PID on murod's next startup.
		// A real end-to-end test (SIGTERM'ing murod while a sandbox was
		// still running) caught this flag doing exactly that.
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

	// HTTP_PROXY/HTTPS_PROXY (both cases — some tools only check lowercase)
	// point every sandbox at murod's proxy via slirpGatewayAddr, NOT
	// proxyAddr's own host — see slirpGatewayAddr's doc comment
	// (network.go) for why: proxyAddr's literal host (127.0.0.1) is never
	// actually reachable from inside the sandbox's network namespace,
	// confirmed empirically. Stage 2/3 networking (network.go, wired in by
	// Launch after this function returns) is what makes the gateway
	// address actually reachable, and Stage 3 specifically is what makes
	// it the ONLY thing reachable, so a sandboxed process genuinely has no
	// way to bypass this by dialing somewhere else directly (SPEC.md §1).
	// A profile that explicitly sets its own HTTP_PROXY/HTTPS_PROXY in Env
	// wins over this default — spec.Env is applied after, not merged over.
	proxyURL := "http://" + gatewayProxyAddr(b.proxyAddr)
	env := map[string]string{
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		"http_proxy":  proxyURL,
		"https_proxy": proxyURL,
	}
	for k, v := range spec.Env {
		env[k] = v
	}

	// Sorted for deterministic argv — makes buildArgs's output testable
	// and bwrap invocations reproducible/loggable.
	envKeys := make([]string, 0, len(env))
	for k := range env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		args = append(args, "--setenv", k, env[k])
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
			Setsid: true,
			// Deliberately NOT Setctty: making pts the sandboxed process's
			// controlling terminal ties its survival to murod's own
			// process lifetime, not just bwrap's --die-with-parent (which
			// is already omitted, see buildArgs). murod holds the pty
			// MASTER (ptmx) in its own fd table for the sandbox's whole
			// life, so when murod exits, the kernel closes ptmx — and
			// losing a controlling terminal's master delivers SIGHUP to
			// its session, killing the sandbox. This is standard tty
			// semantics (the same thing that kills a shell when its
			// terminal window closes), not a bwrap quirk, and it directly
			// broke the "a daemon restart must not kill a running sandbox"
			// requirement (IMPLEMENTATION.md §6) — found via a real
			// end-to-end SIGTERM test. Without Setctty, stdin/stdout/
			// stderr are still the pty slave (raw-mode I/O for attach
			// still works), just not the session's controlling terminal,
			// so losing the master doesn't hang it up. Tradeoff: kernel-
			// generated job-control signals from special tty characters
			// (e.g. a raw Ctrl-C auto-becoming SIGINT) no longer happen —
			// acceptable here since muro sandbox attach's client side
			// already puts the local terminal in raw mode and forwards
			// bytes untouched (internal/cli/attach.go), never relying on
			// local kernel signal generation either. A pty master that
			// survives a daemon restart (the tmux/screen-style fix — a
			// small long-lived per-sandbox helper process murod
			// reconnects to across restarts) is a real, separate
			// architectural piece worth deliberate design attention later,
			// not invented unreviewed here.
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

	// Stage 2/3: bridge this sandbox's otherwise fully-isolated network
	// namespace to the proxy (slirp4netns) and then restrict it to ONLY
	// that destination (nftables). A failure here means the sandbox is
	// running but genuinely has no usable network path at all (not even
	// to the proxy) — that's a strictly safer failure mode than silently
	// leaving it on whatever slirp4netns granted before nft could restrict
	// it, so this tears the whole launch down rather than returning a
	// half-networked sandbox.
	h.netAddr = b.addrAlloc.allocate()
	bridge, err := setupNetworkBridge(h.PID(), h.netAddr, b.proxyAddr)
	if errors.Is(err, errSandboxAlreadyExited) {
		// The sandboxed command already finished (a fast one-shot command
		// like `test -f ...` routinely does) before network setup even got
		// a chance to run — that's a successful launch, not a failure; it
		// never needed the network it didn't get. h.netAddr stays set to
		// an address nothing is actually bridged to, which is harmless
		// (Manager's registerHandleNetworkAddr will register a dead
		// address with the proxy — an inbound connection can never
		// legitimately arrive from it, since nothing is listening on the
		// sandbox side of a bridge that was never built).
		return h, nil
	}
	if err != nil {
		_ = b.Stop(h)
		return nil, fmt.Errorf("set up sandbox network bridge: %w", err)
	}
	h.netBridge = bridge

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
// period) rather than the sandboxed process directly. The slirp4netns
// bridge (Stage 2), if any, is stopped alongside it — it has no reason to
// keep running once the namespace it was bridging is gone, and leaving it
// up would leak a process per sandbox stop.
//
// Since --uid 0 --gid 0 was added (buildArgs, required for Stage 3's nft),
// bwrap's process tree is THREE levels deep, not two: the outer process
// (bh.cmd.Process, what os/exec started) forks an inner process that
// becomes PID 1 of the new pid namespace, which in turn runs the
// sandboxed target. Confirmed empirically, a real regression this
// networking work introduced: signaling only the outer process leaves the
// inner tree running, orphaned — the outer doesn't propagate the signal
// to its child. Worse, the inner process, being PID 1 of its own pid
// namespace, IGNORES SIGTERM by kernel design (the kernel silently drops
// unhandled signals sent to a namespace's init process, except SIGKILL,
// which can never be blocked or ignored) — so even signaling the correct
// (inner) PID with SIGTERM would never work. This is why the inner PID is
// SIGKILLed directly below, immediately, rather than going through the
// same graceful-then-forceful path as the outer: waiting out a grace
// period on a signal known in advance to be ignored would only make every
// `muro sandbox stop` pointlessly slower, with no chance of a different
// outcome.
func (b *BwrapIsolator) Stop(h Handle) error {
	bh, ok := h.(*bwrapHandle)
	if !ok {
		return fmt.Errorf("stop: not a bwrap handle")
	}
	defer bh.netBridge.stop()

	proc := bh.cmd.Process
	if proc == nil {
		return nil
	}

	if innerPID, err := innerNamespacePID(proc.Pid); err == nil {
		if inner, err := os.FindProcess(innerPID); err == nil {
			_ = inner.Signal(syscall.SIGKILL)
		}
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
// bwrap invocation. It also implements the optional networkAddrProvider
// interface (network.go) via NetworkAddr, since a real bwrap-launched
// sandbox always has Stage 2/3 networking set up in Launch.
type bwrapHandle struct {
	cmd *exec.Cmd
	pty *os.File // non-nil only if LaunchSpec.PTY was true

	netAddr   string         // this sandbox's assigned outbound loopback address, e.g. "127.0.0.5"
	netBridge *networkBridge // nil only if Launch failed before reaching network setup

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

// NetworkAddr implements networkAddrProvider (network.go).
func (h *bwrapHandle) NetworkAddr() string {
	return h.netAddr
}

func (h *bwrapHandle) Wait() (int, error) {
	h.waitOnce.Do(func() {
		err := h.cmd.Wait()
		if h.pty != nil {
			h.pty.Close()
		}
		// The sandbox's process just exited — on its own, not necessarily
		// via an explicit Stop() — so its network bridge (Stage 2/3,
		// network.go) has nothing left to bridge and must be torn down
		// here too, not only in Stop(). Without this, any sandbox that
		// exits naturally (the common case — Stop() is only the "muro
		// sandbox stop" path) leaked its slirp4netns process forever;
		// confirmed by real accumulation across this package's own
		// integration test runs before this fix (netBridge.stop's
		// sync.Once makes this safe to also run from Stop()'s defer,
		// whichever gets there first).
		h.netBridge.stop()
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
