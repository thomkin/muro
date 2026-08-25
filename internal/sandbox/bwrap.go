package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thomkin/muro/internal/config"
)

const bwrapBinary = "bwrap"

// AgentSocketMountPath is the fixed, sandbox-internal path every sandbox
// with the MQTT agent-to-agent bridge enabled gets its agent socket mounted
// at — a constant, not something a profile configures, matching the same
// "fixed, not profile-controlled" convention as bwrap's own /proc, /dev,
// /tmp scaffolding paths.
const AgentSocketMountPath = "/run/muro/agent.sock"

// AgentSocketPath returns the host-side path murod listens on for
// sandboxID's agent socket. A pure function of stateDir+sandboxID so both
// Manager (which starts the listener BEFORE calling Launch, so it's ready
// the instant a fast-starting sandboxed process could try to connect) and
// BwrapIsolator.Launch (which mounts it) independently compute the
// identical path — the same "both sides derive it, neither persists a
// value the other must read back" pattern config.SandboxLogPath already
// establishes for LogPath. Lives in the same runDir as shim.sock/
// exit_status (Launch, below), just not shim-owned — shim.go's Inject
// socket is the pty-injection half of the bridge, a separate concept.
func AgentSocketPath(stateDir, sandboxID string) string {
	return filepath.Join(stateDir, "sandboxes", sandboxID, "agent.sock")
}

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
	shimPath  string
	stateDir  string // base dir under which each sandbox gets a shim.go runtime subdirectory
	proxyAddr string
	addrAlloc *outboundAddrAllocator
}

// NewBwrapIsolator checks that bwrap, muro-shim, and the Stage 2/3
// networking tools are installed and that unprivileged user namespaces
// actually work on this host (DESIGN.md §8), returning a clear, actionable
// error otherwise rather than letting a launch fail with a cryptic error
// later. proxyAddr is murod's URL-allowlist proxy listen address
// (host:port) — every sandbox this isolator launches is bridged toward
// exactly that destination and nothing else. stateDir is murod's XDG state
// directory (internal/config.StateDir()) — each sandbox gets a
// stateDir/sandboxes/<id>/ subdirectory holding its shim's socket and exit
// status file (shim.go), named by a stable ID rather than any PID so a
// restarted murod can find it again purely by reading the path back out of
// state.json.
func NewBwrapIsolator(proxyAddr, stateDir string) (*BwrapIsolator, error) {
	path, err := exec.LookPath(bwrapBinary)
	if err != nil {
		return nil, fmt.Errorf("bwrap not found on PATH: install bubblewrap (see DESIGN.md §8): %w", err)
	}
	if err := checkUnprivilegedUserns(path); err != nil {
		return nil, err
	}
	shimPath, err := exec.LookPath("muro-shim")
	if err != nil {
		return nil, fmt.Errorf("muro-shim not found on PATH — it ships alongside muro/murod/muro-broker and must be installed the same way: %w", err)
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
	return &BwrapIsolator{
		bwrapPath: path,
		shimPath:  shimPath,
		stateDir:  stateDir,
		proxyAddr: proxyAddr,
		addrAlloc: newOutboundAddrAllocator(),
	}, nil
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

	mounts := spec.Mounts
	if spec.AgentSocketPath != "" {
		// The agent-to-agent MQTT bridge's outbound half (agentsocket.go):
		// murod already listens on spec.AgentSocketPath on the host side
		// (started by Manager before Launch, so it's ready the instant the
		// sandboxed process could possibly connect) — this mount is what
		// makes it reachable from inside the sandbox, at a fixed path every
		// sandbox gets consistently, not something a profile author writes
		// themselves. rw: `muro pubsub publish` needs to connect() to it,
		// which requires write access to the socket special file's
		// directory entry semantics on Linux (connecting to a Unix socket
		// bind-mounted read-only still works for connect, but bind-mounting
		// rw here avoids any surprise — the socket's own connect-permission
		// is governed by its 0600 file mode, not this mount's ro/rw flag,
		// so this doesn't weaken anything).
		mounts = append(append([]config.Mount(nil), mounts...), config.Mount{
			Host:        spec.AgentSocketPath,
			SandboxPath: AgentSocketMountPath,
			Mode:        "rw",
		})
	}
	for _, m := range mounts {
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

// Launch starts one sandboxed process — not by exec'ing bwrap directly,
// but via muro-shim (shim.go, cmd/muro-shim): a small standalone process
// that allocates the pty, execs bwrap as ITS OWN child, and keeps running
// independent of murod's own process lifetime. This is what lets a
// sandbox survive a murod restart — the original in-process pty-holding
// design (murod itself holding the pty master) could not, since the
// kernel EOFs/EIOs the pty slave the instant the master's last reference
// (murod's own fd table entry) closes; see git history ("Wire cmd/murod
// (Stage 1)") for how that was found. Handle.PID() below returns the
// SHIM's PID, not bwrap's — that's the PID state.Reconcile checks for
// liveness across a restart, and the shim is what's actually still there.
//
// Network isolation is real and total before the bridge below runs:
// --unshare-net gives the sandbox a fresh, unconfigured network namespace
// (not even a usable loopback) with no bridge back to anything.
func (b *BwrapIsolator) Launch(ctx context.Context, spec LaunchSpec) (Handle, error) {
	args := b.buildArgs(spec)

	sandboxID := spec.SandboxID
	if sandboxID == "" {
		// Defensive fallback only — Manager always sets this via
		// buildLaunchSpec; a direct Isolator caller (e.g. a test) that
		// doesn't still gets a working, if less readable, run directory.
		sandboxID = fmt.Sprintf("anon-%d", time.Now().UnixNano())
	}
	runDir := filepath.Join(b.stateDir, "sandboxes", sandboxID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox run directory: %w", err)
	}
	socketPath := filepath.Join(runDir, "shim.sock")
	statusPath := filepath.Join(runDir, "exit_status")
	os.Remove(socketPath) // stale socket from a previous shim at this same ID, if any

	var injectSocketPath string
	if spec.PTY {
		// Injection only makes sense for a pty-backed sandbox (there's
		// nothing to write keystrokes into otherwise) — matches the
		// existing SocketPath/attach convention of only listening when
		// spec.PTY is true (cmd/muro-shim's run()).
		injectSocketPath = filepath.Join(runDir, "inject.sock")
		os.Remove(injectSocketPath)
	}

	shimSpec := ShimSpec{
		BwrapPath:        b.bwrapPath,
		Args:             args,
		PTY:              spec.PTY,
		SocketPath:       socketPath,
		StatusPath:       statusPath,
		LogPath:          spec.LogPath,
		InjectSocketPath: injectSocketPath,
	}
	specFile, err := writeShimSpec(runDir, shimSpec)
	if err != nil {
		return nil, fmt.Errorf("write shim spec: %w", err)
	}

	readR, readW, err := os.Pipe()
	if err != nil {
		os.Remove(specFile)
		return nil, fmt.Errorf("create shim ready-fd pipe: %w", err)
	}
	defer readW.Close() // our copy; the shim keeps its own dup after Start

	shimCmd := exec.CommandContext(ctx, b.shimPath, specFile)
	shimCmd.ExtraFiles = []*os.File{readW} // becomes fd 3 in the shim, matching slirp4netns's own --ready-fd=3 convention
	// Setsid: the whole point — detaches the shim into its own session so
	// it (and its bwrap child) survive murod exiting, whether cleanly or
	// not. This is the fix for the gap the comment this replaced
	// documented as "worth deliberate design attention later".
	shimCmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := shimCmd.Start(); err != nil {
		readR.Close()
		os.Remove(specFile)
		return nil, fmt.Errorf("start muro-shim: %w", err)
	}
	readW.Close()

	bwrapOuterPID, err := readShimReady(readR, 5*time.Second)
	readR.Close()
	if err != nil {
		_ = shimCmd.Process.Kill()
		return nil, fmt.Errorf("muro-shim did not become ready: %w", err)
	}

	h := &bwrapHandle{
		shimCmd:          shimCmd,
		shimPID:          shimCmd.Process.Pid,
		socketPath:       socketPath,
		statusPath:       statusPath,
		injectSocketPath: injectSocketPath,
	}

	// Stage 2/3: bridge this sandbox's otherwise fully-isolated network
	// namespace to the proxy (slirp4netns) and then restrict it to ONLY
	// that destination (nftables). This still runs from murod's own
	// process (not the shim) — it only ever needed a PID to target via
	// nsenter/slirp4netns's --netns-type=pid resolution, not an actual
	// parent-child relationship, so moving bwrap's launch into the shim
	// doesn't require moving this too. A failure here means the sandbox
	// is running but genuinely has no usable network path at all (not
	// even to the proxy) — that's a strictly safer failure mode than
	// silently leaving it on whatever slirp4netns granted before nft
	// could restrict it, so this tears the whole launch down rather than
	// returning a half-networked sandbox.
	h.netAddr = b.addrAlloc.allocate()
	bridge, err := setupNetworkBridge(bwrapOuterPID, h.netAddr, b.proxyAddr)
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

// Reattach reconstructs a Handle for a sandbox whose shim (and therefore
// whose real sandboxed process) is still alive, but which THIS
// BwrapIsolator instance never itself Launched — the case right after a
// murod restart, once state.Reconcile has confirmed the persisted PID is
// still live. This is what lets `muro sandbox attach`/`stop` keep working
// across a restart even though nothing here started that process. It
// implements the optional Reattacher interface (isolator.go); Manager
// checks for it via a type assertion rather than every Isolator being
// required to support it (a test fake, for instance, has no equivalent
// concept and doesn't need one).
func (b *BwrapIsolator) Reattach(pid int, shimSocket string, slirpPID int, netAddr string) (Handle, error) {
	if !isAlivePID(pid) {
		return nil, fmt.Errorf("shim pid %d is not alive", pid)
	}
	if shimSocket == "" {
		return nil, fmt.Errorf("no shim socket recorded for pid %d", pid)
	}
	return &bwrapHandle{
		shimPID:    pid,
		socketPath: shimSocket,
		statusPath: filepath.Join(filepath.Dir(shimSocket), "exit_status"),
		slirpPID:   slirpPID,
		netAddr:    netAddr,
	}, nil
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

// Stop signals the shim (SIGTERM, escalating to SIGKILL after a grace
// period) — not bwrap directly, and not the sandboxed process directly.
// The shim owns the "SIGTERM the outer bwrap process, SIGKILL the inner
// pid-namespace-init immediately since it ignores SIGTERM by kernel
// design" logic that used to live here (see shim.go / cmd/muro-shim for
// that reasoning, ported over verbatim) — it moved there because the shim,
// not murod, is bwrap's actual parent process now (Launch). BwrapIsolator
// only needs to know how to stop ITS child, the shim, and trust the shim
// to correctly tear down what's underneath it before exiting.
//
// The slirp4netns bridge (Stage 2), if any, is stopped alongside it — it
// has no reason to keep running once the namespace it was bridging is
// gone, and leaving it up would leak a process per sandbox stop. For a
// Handle reconstructed after a murod restart (Reattach), netBridge is nil
// (this process was never that bridge's parent, so there's no live
// *exec.Cmd to Wait on) — slirpPID, persisted across the restart in
// state.Sandbox, is what lets this still tear it down correctly.
func (b *BwrapIsolator) Stop(h Handle) error {
	bh, ok := h.(*bwrapHandle)
	if !ok {
		return fmt.Errorf("stop: not a bwrap handle")
	}
	defer func() {
		if bh.netBridge != nil {
			bh.netBridge.stop()
		} else {
			stopSlirpByPID(bh.slirpPID)
		}
	}()

	if bh.shimPID == 0 {
		return nil
	}
	proc, err := os.FindProcess(bh.shimPID) // Unix: always succeeds regardless of liveness
	if err != nil {
		return nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if isProcessDeadErr(err) {
			return nil
		}
		return fmt.Errorf("signal shim (SIGTERM): %w", err)
	}

	// Route through bh.Wait (not cmd.Wait directly): Manager's watch loop
	// also calls Handle.Wait on this same handle after Launch, and
	// exec.Cmd.Wait must only ever be invoked once — bh.Wait's sync.Once
	// makes this call and the watch loop's call safe to race against each
	// other, whichever gets there first.
	done := make(chan struct{})
	go func() {
		bh.Wait()
		close(done)
	}()

	// 7s, not 5s: the shim needs up to ~3s of its own internal
	// grace-then-SIGKILL for bwrap (shim.go/cmd/muro-shim) before it exits
	// on its own; this timeout has to comfortably exceed that plus normal
	// scheduling overhead, or Stop would routinely SIGKILL a shim that was
	// already correctly in the middle of shutting down cleanly.
	select {
	case <-done:
		return nil
	case <-time.After(7 * time.Second):
		if err := proc.Signal(syscall.SIGKILL); err != nil && !isProcessDeadErr(err) {
			return fmt.Errorf("signal shim (SIGKILL): %w", err)
		}
		<-done
		return nil
	}
}

func isProcessDeadErr(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// isAlivePID reports whether pid refers to a currently running process —
// the standard signal-0 liveness check (internal/state/reconcile.go has
// its own copy of the same technique for the same reason: no shared
// dependency between these two packages is worth introducing for
// something this small).
func isAlivePID(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return !isProcessDeadErr(proc.Signal(syscall.Signal(0)))
}

// bwrapHandle implements Handle. It has two lifecycles, distinguished by
// whether shimCmd is set:
//   - Freshly launched (Launch, this process's own child): shimCmd is the
//     live *exec.Cmd this process started, and Wait blocks on it directly
//     via exec.Cmd.Wait, exactly recovering the shim's own exit code —
//     which the shim (cmd/muro-shim) deliberately propagates from its
//     bwrap child's real exit code (os.Exit(bwrapExitCode)), so this
//     still means what Manager's restart_policy logic needs it to mean.
//   - Reconstructed after a murod restart (Reattach): shimCmd is nil,
//     since this process is not that shim's parent and has no wait4(2)
//     handle on it — Wait instead polls shimPID for liveness and recovers
//     the real exit code from statusPath, which the shim wrote just
//     before exiting (shim.go's ShimStatus).
//
// It also implements the optional networkAddrProvider (network.go) and
// Reattacher-adjacent shimRuntimeInfo (manager.go) interfaces, since a
// real bwrap-launched sandbox always has Stage 2/3 networking and a shim
// runtime directory.
type bwrapHandle struct {
	shimCmd    *exec.Cmd // nil for a Reattach-reconstructed handle
	shimPID    int       // always set; PID() and signaling use this directly rather than shimCmd.Process, which is nil in the reconstructed case
	socketPath string
	statusPath string
	// injectSocketPath is only set on a freshly-Launched handle, empty on
	// a Reattach-reconstructed one — nothing downstream needs it post-
	// restart (Manager's inbox-listener reads state.Sandbox.InjectSocket,
	// already persisted from the original Launch, directly from the
	// Store rather than through this interface, so there was no need to
	// thread it through Reattacher's signature too).
	injectSocketPath string

	netAddr   string         // this sandbox's assigned outbound loopback address, e.g. "127.0.0.5"
	netBridge *networkBridge // nil if Launch failed before reaching network setup, OR if reconstructed (see slirpPID)
	slirpPID  int            // persisted fallback for netBridge==nil (Reattach) so Stop can still tear the bridge down by PID

	waitOnce sync.Once
	exitCode int
	waitErr  error
}

func (h *bwrapHandle) PID() int { return h.shimPID }

// NetworkAddr implements networkAddrProvider (network.go).
func (h *bwrapHandle) NetworkAddr() string { return h.netAddr }

// ShimSocket and SlirpPID implement shimRuntimeInfo (manager.go) — Manager
// persists both into state.Sandbox so a restarted murod's Reattach (above)
// has what it needs without this process's cooperation.
func (h *bwrapHandle) ShimSocket() string { return h.socketPath }

// InjectSocket implements shimRuntimeInfo (manager.go) — Manager persists
// it into state.Sandbox.InjectSocket so the MQTT inbox-listener can dial it
// directly from the Store, without needing a live Handle at all.
func (h *bwrapHandle) InjectSocket() string { return h.injectSocketPath }
func (h *bwrapHandle) SlirpPID() int {
	if h.netBridge != nil {
		return h.netBridge.pid()
	}
	return h.slirpPID
}

func (h *bwrapHandle) Wait() (int, error) {
	h.waitOnce.Do(func() {
		if h.shimCmd != nil {
			h.waitLive()
			return
		}
		h.waitReconstructed()
	})
	return h.exitCode, h.waitErr
}

func (h *bwrapHandle) waitLive() {
	err := h.shimCmd.Wait()
	// The sandbox's process just exited — on its own, not necessarily via
	// an explicit Stop() — so its network bridge (Stage 2/3, network.go)
	// has nothing left to bridge and must be torn down here too, not only
	// in Stop(). Without this, any sandbox that exits naturally (the
	// common case) leaked its slirp4netns process forever; confirmed by
	// real accumulation across this package's own integration test runs
	// before this fix (netBridge.stop's sync.Once makes this safe to also
	// run from Stop()'s defer, whichever gets there first).
	h.netBridge.stop()
	if err == nil {
		h.exitCode, h.waitErr = 0, nil
		return
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		h.exitCode, h.waitErr = exitErr.ExitCode(), nil
		return
	}
	h.exitCode, h.waitErr = -1, err
}

// waitReconstructed handles the Reattach case: this process is not the
// shim's parent, so there is no wait4(2) call available — poll PID
// liveness instead, then recover the real exit code the shim recorded
// (ShimStatus, shim.go) just before it exited.
//
// Also tears down the network bridge via stopSlirpByPID(h.slirpPID) once
// the shim is confirmed gone — the same reasoning waitLive's h.netBridge.
// stop() call already documents (a sandbox that exits on its own, not via
// an explicit Stop(), still needs its slirp4netns process cleaned up or it
// leaks forever), but netBridge is always nil for a reconstructed handle
// (this process was never that bridge's parent — see bwrapHandle's own
// field doc comment), so the PID-based fallback is what's needed here.
// This path was unreachable until Manager.Reattach started spawning a
// watchLoop (which is what actually calls Wait on a reconstructed handle)
// — confirmed as a real, previously-latent gap by direct reproduction: a
// sandbox reattached after a murod restart, then crashing on its own,
// left its original slirp4netns process running indefinitely without it.
func (h *bwrapHandle) waitReconstructed() {
	for isAlivePID(h.shimPID) {
		time.Sleep(100 * time.Millisecond)
	}
	stopSlirpByPID(h.slirpPID)
	data, err := os.ReadFile(h.statusPath)
	if err != nil {
		h.exitCode, h.waitErr = -1, fmt.Errorf("shim exited, exit status unavailable: %w", err)
		return
	}
	var st ShimStatus
	if err := json.Unmarshal(data, &st); err != nil {
		h.exitCode, h.waitErr = -1, fmt.Errorf("shim exited, exit status unreadable: %w", err)
		return
	}
	if st.Err != "" {
		h.exitCode, h.waitErr = -1, errors.New(st.Err)
		return
	}
	h.exitCode, h.waitErr = st.ExitCode, nil
}

// Stdio dials the shim's Unix socket fresh on every call rather than
// returning a single long-held file — see isolator.go's Handle.Stdio doc
// comment for why. Cleanly reports ok=false for a sandbox launched without
// PTY:true: the shim never listens on SocketPath at all in that case
// (cmd/muro-shim), so the dial fails naturally rather than needing this
// side to separately track whether a pty was requested.
func (h *bwrapHandle) Stdio() (io.ReadWriteCloser, bool) {
	if h.socketPath == "" {
		return nil, false
	}
	conn, err := net.Dial("unix", h.socketPath)
	if err != nil {
		return nil, false
	}
	return conn, true
}

// writeShimSpec marshals spec to a private (0600), single-use temp file in
// dir for muro-shim to read on startup — it deletes the file itself once
// read, so this doesn't need its own cleanup path.
func writeShimSpec(dir string, spec ShimSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal shim spec: %w", err)
	}
	f, err := os.CreateTemp(dir, ".shim-spec-*.json")
	if err != nil {
		return "", fmt.Errorf("create shim spec file: %w", err)
	}
	name := f.Name()
	if err := os.Chmod(name, 0o600); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("chmod shim spec file: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(name)
		return "", fmt.Errorf("write shim spec file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("close shim spec file: %w", err)
	}
	return name, nil
}

// readShimReady blocks until muro-shim writes its one ready line to r (or
// closes it without writing, or the timeout elapses) and parses it — "OK
// <bwrap-outer-pid>" or "ERR <message>" (shim.go's shimReadyOKPrefix/
// shimReadyErrPrefix). r is read to EOF rather than line-by-line since the
// shim writes exactly one line and then closes its end of the pipe,
// exactly mirroring startSlirp4netns's existing ready-fd pattern
// (network.go) for consistency.
func readShimReady(r *os.File, timeout time.Duration) (int, error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := io.ReadAll(r)
		ch <- result{data, err}
	}()

	select {
	case res := <-ch:
		if res.err != nil {
			return 0, res.err
		}
		line := strings.TrimSpace(string(res.data))
		switch {
		case strings.HasPrefix(line, shimReadyOKPrefix):
			pid, err := strconv.Atoi(strings.TrimPrefix(line, shimReadyOKPrefix))
			if err != nil {
				return 0, fmt.Errorf("malformed ready line %q: %w", line, err)
			}
			return pid, nil
		case strings.HasPrefix(line, shimReadyErrPrefix):
			return 0, errors.New(strings.TrimPrefix(line, shimReadyErrPrefix))
		default:
			return 0, fmt.Errorf("unexpected (or empty) ready line %q", line)
		}
	case <-time.After(timeout):
		return 0, fmt.Errorf("timed out after %s", timeout)
	}
}

// OpenPTY allocates a pseudo-terminal pair using only stdlib + golang.org/
// x/sys/unix (already a project dependency) — no third-party pty library.
// This is the standard dependency-free Linux sequence: open /dev/ptmx for
// the master, unlock it (TIOCSPTLCK) and read its slave number (TIOCGPTN),
// then open /dev/pts/<n> for the slave. Known limitation: this only
// supports Linux's /dev/ptmx multiplexer (not the BSD-style pty pairs some
// non-Linux Unixes use), which is fine given DESIGN.md §3's Linux-only
// scope, and it does not set an initial terminal size — Manager/the attach
// path is expected to send a resize once a real terminal is attached
// (DESIGN.md §12 mentions resize forwarding as part of attach, not launch).
func OpenPTY() (ptmx *os.File, pts *os.File, err error) {
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
