package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// slirpGatewayAddr is the host-reachable address a sandbox must dial to
// reach murod's proxy — NOT the proxy's own listen address. Confirmed
// empirically: a bare connection to the proxy's literal 127.0.0.1 from
// inside the sandbox's network namespace never leaves that namespace's own
// private loopback interface at all (standard Linux routing — 127.0.0.1
// traffic stays on `lo`, it doesn't route out via the tap device), so it
// never reaches slirp4netns's host-loopback forwarding despite
// --disable-host-loopback being left off. What slirp4netns actually
// forwards to the real host loopback is traffic aimed at its own gateway
// address — 10.0.2.2, the ".2" of its default --cidr=10.0.2.0/24 (not
// customized here, so this is a fixed default, not configuration this
// package needs to read back from slirp4netns). Both the injected
// HTTP_PROXY/HTTPS_PROXY (buildArgs, below) and the nftables egress rule
// (applyEgressRestriction) target this address, not proxyAddr's host part.
const slirpGatewayAddr = "10.0.2.2"

// gatewayProxyAddr rewrites proxyAddr's host to slirpGatewayAddr, keeping
// its port — this is the address a sandboxed process must actually dial
// (via HTTP_PROXY/HTTPS_PROXY, bwrap.go's buildArgs) to reach murod's
// proxy. Falls back to proxyAddr unchanged if it doesn't parse as
// host:port (defensive only; proxyAddr is always well-formed in practice).
func gatewayProxyAddr(proxyAddr string) string {
	_, port, err := splitHostPort(proxyAddr)
	if err != nil {
		return proxyAddr
	}
	return slirpGatewayAddr + ":" + port
}

// networkAddrProvider is an optional capability a Handle may additionally
// implement to expose the host-loopback address its sandbox's traffic is
// bridged through (Stage 2 networking). It is deliberately NOT part of the
// core Handle interface (isolator.go) — other Isolator implementations
// (internal/control's and internal/sandbox's own test fakes) don't provide
// real networking and must not be forced to grow a method they can't
// meaningfully implement. Manager type-asserts for this, Go's standard
// "optional interface" pattern (cf. io.ReaderFrom, http.Flusher).
type networkAddrProvider interface {
	// NetworkAddr returns the per-sandbox loopback address (e.g.
	// "127.0.0.5") its bridged traffic arrives at the proxy from, or "" if
	// no network bridge is set up for this handle.
	NetworkAddr() string
}

// outboundAddrAllocator hands out distinct 127.0.0.0/8 loopback addresses,
// one per sandbox, so the proxy can tell which sandbox an inbound bridged
// connection belongs to purely from its source address (see
// internal/proxy.Server.RegisterSandboxAddr). Linux treats the entire
// 127.0.0.0/8 range as loopback, not just 127.0.0.1, so this needs no
// routing setup on the host side.
type outboundAddrAllocator struct {
	mu   sync.Mutex
	next int
}

func newOutboundAddrAllocator() *outboundAddrAllocator {
	return &outboundAddrAllocator{next: 2} // 127.0.0.1 is the host's own address; start at .2
}

func (a *outboundAddrAllocator) allocate() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	addr := fmt.Sprintf("127.0.0.%d", a.next)
	a.next++
	if a.next > 254 {
		a.next = 2 // wrap; 253 concurrent sandboxes is far beyond any realistic v1 usage
	}
	return addr
}

// errSandboxAlreadyExited is returned by InnerNamespacePID when the outer
// bwrap process (and therefore necessarily its whole tree) has already
// exited by the time this looked for it — a fast, legitimate command (e.g.
// `test -f ...`, `echo hi`) can complete before Launch's network setup
// even runs. This is not a failure: the sandbox already did whatever it
// was going to do and never needed network access it didn't get a chance
// to use. Callers must treat this as "nothing to bridge, launch still
// succeeds," not propagate it as a Launch error — confirmed as a real,
// common case (not a hypothetical) by test/integration/bwrap_test.go's
// existing fast-exiting test commands, which failed outright before this
// was handled as a distinct case from a genuinely stuck bridge attempt.
var errSandboxAlreadyExited = errors.New("sandbox process already exited before network setup ran")

// InnerNamespacePID discovers the PID that actually owns the sandbox's new
// namespaces. bwrap's outer process (the one os/exec starts) performs
// initial setup in the CALLER's namespaces and only forks+unshares into the
// new user/pid/net/ipc/uts namespaces in a child, which becomes PID 1 of
// the new pid namespace — confirmed empirically (readlink
// /proc/<outer>/ns/net vs /proc/<inner>/ns/net differ; only the child
// matches the new namespace). That child's PID is discoverable via Linux's
// /proc/<pid>/task/<pid>/children, which lists direct children with no
// need for a process-table scan.
//
// This polls briefly since the fork hasn't necessarily happened the
// instant cmd.Start() returns, but any timeout here — whether because the
// outer process has already exited outright, or because it's still alive
// but the children file came up empty — is treated as
// errSandboxAlreadyExited, not a genuine failure. That's deliberately not
// two different cases: bwrap's fork into the new pid namespace happens
// essentially immediately (--unshare-pid unconditionally requires it,
// before the target command is even exec'd, not after some long-running
// work), so there is no real scenario where bwrap starts successfully and
// stays running for any meaningful duration without ever forking that
// child — a timeout here overwhelmingly means a fast one-shot command
// (`test -f ...`, `echo hi`) already ran to completion, and its whole
// process tree (or the specific child this was polling for) was reaped,
// before this function's very first poll sample. Confirmed empirically:
// treating "outer alive, no child yet" as an error, distinct from "outer
// already gone," caused this exact false-positive failure against real
// fast-exiting sandboxes in test/integration/bwrap_test.go.
func InnerNamespacePID(outerPID int) (int, error) {
	childrenPath := fmt.Sprintf("/proc/%d/task/%d/children", outerPID, outerPID)
	// Short: real detection (when there's anything left to detect) is a
	// sub-millisecond /proc read, so this only ever actually elapses for
	// the fast-exit case this function documents — keeping it short avoids
	// tacking multi-second latency onto every short-lived sandbox launch.
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		data, err := os.ReadFile(childrenPath)
		if err == nil {
			fields := strings.Fields(string(data))
			if len(fields) > 0 {
				pid, err := strconv.Atoi(fields[0])
				if err == nil {
					return pid, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return 0, errSandboxAlreadyExited
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// networkBridge is the Stage 2/3 state tied to one launched sandbox: the
// slirp4netns process bridging its otherwise fully-isolated network
// namespace to the host (Stage 2), plus the nftables egress restriction
// applied inside that namespace (Stage 3). Both are torn down together via
// stop().
type networkBridge struct {
	slirpCmd     *exec.Cmd
	outboundAddr string
	stopOnce     sync.Once
}

// setupNetworkBridge bridges outerPID's sandbox to the host, Stage 2+3
// combined: slirp4netns gives it a route to the proxy; nftables then
// restricts that route to ONLY the proxy, nothing else (SPEC.md §1 — OS
// enforcement, not the agent's cooperation with HTTP_PROXY). A real,
// unavoidable race window exists between the sandboxed process starting
// (at cmd.Start(), before this function even runs) and nftables actually
// being installed a few milliseconds later — closing that window
// completely would need bwrap to pause before exec, which bwrap has no
// facility for; this function does the necessary setup as fast as
// reasonably possible, which is the documented, accepted v1 limit rather
// than a true barrier.
//
// The window widened once bwrap moved behind muro-shim (shim.go): Launch
// now waits out an extra process spawn + ready-fd round trip before this
// function even starts, giving a fast one-shot command (`test -f ...`)
// more time to finish and be reaped before InnerNamespacePID's initial
// poll runs. That part was already handled — but the SAME race can now
// also land a moment LATER, between InnerNamespacePID succeeding and
// slirp4netns/nft actually executing against the PID it found; confirmed
// empirically (nsenter: "cannot open /proc/<pid>/ns/user: No such file or
// directory" against fast integration-test commands after the shim
// change). Both slirp4netns and applyEgressRestriction failures are
// therefore re-checked against isAlivePID before being treated as a real
// error — a genuine tool failure against a still-alive target is
// reported normally; a failure against a target that's simply gone by
// now is the identical "nothing to bridge, launch still succeeds" case
// InnerNamespacePID's own doc comment already describes, not a new kind
// of failure.
func setupNetworkBridge(outerPID int, outboundAddr, proxyAddr string) (*networkBridge, error) {
	innerPID, err := InnerNamespacePID(outerPID)
	if errors.Is(err, errSandboxAlreadyExited) {
		return nil, errSandboxAlreadyExited // propagated as-is; Launch treats this specially
	}
	if err != nil {
		return nil, fmt.Errorf("find sandbox network namespace: %w", err)
	}

	slirpCmd, err := startSlirp4netns(innerPID, outboundAddr)
	if err != nil {
		if !isAlivePID(innerPID) {
			return nil, errSandboxAlreadyExited
		}
		return nil, fmt.Errorf("bridge sandbox network (slirp4netns): %w", err)
	}

	if err := applyEgressRestriction(innerPID, proxyAddr); err != nil {
		_ = stopSlirp4netns(slirpCmd)
		if !isAlivePID(innerPID) {
			return nil, errSandboxAlreadyExited
		}
		return nil, fmt.Errorf("restrict sandbox egress (nft): %w", err)
	}

	return &networkBridge{slirpCmd: slirpCmd, outboundAddr: outboundAddr}, nil
}

// stop is safe to call more than once (e.g. once from Wait() as the
// sandboxed process exits on its own, once from an explicit Stop() racing
// against it) — only the first call actually does anything, matching the
// same sync.Once pattern bwrapHandle.Wait/Stop already use for bwrap's own
// process, for the identical reason: exec.Cmd.Wait must only ever be
// invoked once, and stopSlirp4netns calls it.
func (nb *networkBridge) stop() {
	if nb == nil || nb.slirpCmd == nil {
		return
	}
	nb.stopOnce.Do(func() {
		_ = stopSlirp4netns(nb.slirpCmd)
	})
}

// startSlirp4netns bridges innerPID's network namespace to the host,
// giving it a route to the host's loopback (slirp4netns's default
// behavior — --disable-host-loopback is deliberately NOT passed, since
// without it the guest CAN reach 127.0.0.1:* on the host, which is exactly
// the "route to the daemon's proxy" SPEC.md §6.1 calls for) and pinning
// its outbound-facing source address to outboundAddr so the proxy can
// identify which sandbox a connection came from purely by source IP
// (internal/proxy.Server.RegisterSandboxAddr).
//
// Deliberately passes only innerPID as the target (slirp4netns's
// --netns-type defaults to "pid", which resolves the namespace from a
// live PID the same way `nsenter --target <pid>` does) rather than a
// manually-dereferenced --userns-path — empirically, the manual-path form
// fails with "setns(CLONE_NEWNET): Operation not permitted" even though
// the equivalent --target-style resolution succeeds; the PID-target
// default is what actually works.
func startSlirp4netns(innerPID int, outboundAddr string) (*exec.Cmd, error) {
	readR, readW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create ready-fd pipe: %w", err)
	}
	defer readW.Close() // our copy; the child keeps its own dup after Start

	cmd := exec.Command("slirp4netns",
		"--configure",
		"--outbound-addr="+outboundAddr,
		"--ready-fd=3",
		strconv.Itoa(innerPID),
		"tap0",
	)
	cmd.ExtraFiles = []*os.File{readW} // becomes fd 3 in the child, matching --ready-fd=3
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		readR.Close()
		return nil, fmt.Errorf("start slirp4netns: %w", err)
	}
	readW.Close() // parent's copy is no longer needed once the child has its own

	// Wait for slirp4netns to signal readiness (it writes then closes fd 3
	// once the interface is configured) rather than a fixed sleep — bounds
	// the Stage-2-specific portion of the documented race window to
	// "however long slirp4netns actually takes," not a guessed constant.
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 1)
		_, _ = readR.Read(buf) // returns on any write or on EOF (fd closed)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		readR.Close()
		return nil, fmt.Errorf("slirp4netns did not become ready within 3s")
	}
	readR.Close()

	// A ready signal only means "the interface is configured," not "the
	// process is still alive" — an early exit (e.g. slirp4netns rejecting
	// a flag) can still close fd 3 as part of normal process teardown.
	// Confirm it's actually still running before declaring success.
	if cmd.ProcessState != nil {
		return nil, fmt.Errorf("slirp4netns exited immediately after reporting ready (exit %v)", cmd.ProcessState)
	}

	return cmd, nil
}

// pid returns the slirp4netns process's PID, or 0 if there is none (nb is
// nil, or Launch never got as far as starting it). Manager persists this
// into state.Sandbox.SlirpPID so a Handle reconstructed after a murod
// restart (shim.go) can still tear the bridge down by PID even though it
// was never that bridge's parent process.
func (nb *networkBridge) pid() int {
	if nb == nil || nb.slirpCmd == nil || nb.slirpCmd.Process == nil {
		return 0
	}
	return nb.slirpCmd.Process.Pid
}

// stopSlirpByPID tears down a slirp4netns process this Go process is NOT
// the parent of — the case after a murod restart, reconstructing a Handle
// for a sandbox whose bridge was started by the PREVIOUS murod process.
// os.Process.Wait only works for an actual child, so unlike
// stopSlirp4netns (which owns a live *exec.Cmd and can just Wait it),
// this polls PID liveness instead of blocking on wait4(2).
func stopSlirpByPID(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return // already gone
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !isAlivePID(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Signal(syscall.SIGKILL)
}

func stopSlirp4netns(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		if isProcessDeadErr(err) {
			return nil
		}
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// applyEgressRestriction installs an nftables ruleset inside innerPID's
// network namespace restricting all outbound traffic to ONLY proxyAddr
// (host:port) — default-DROP, one explicit ACCEPT. This is what actually
// closes the gap slirp4netns opens: slirp4netns alone gives the sandbox
// NAT'd access to reach whatever destination it wants (it's a generic
// unprivileged-networking tool, not a policy enforcer), so without this a
// process could bypass HTTP_PROXY/HTTPS_PROXY entirely with a raw socket
// and reach the real internet directly (SPEC.md §1: OS-enforced isolation,
// not the agent's cooperation).
//
// No DNS rule is included: with HTTP_PROXY/HTTPS_PROXY set, hostname
// resolution happens proxy-side (murod resolves the real destination),
// never sandbox-side — DESIGN.md's proxy design was reasoned through with
// this in mind, so there is nothing for the sandbox to resolve and
// therefore nothing to allowlist for DNS.
//
// Namespace access: nft must run WITH the capabilities the sandbox's own
// user namespace grants (an unprivileged network namespace's rules can
// only be installed by something operating inside that namespace's
// context) — done via `nsenter --target <pid> --user --net
// --preserve-credentials`. Empirically, joining --user and --net via
// manually-dereferenced --user=<path> --net=<path> fails with EPERM even
// though the process/namespace itself is perfectly valid; --target-style
// resolution (matching how slirp4netns's own default PID-based mode
// works, see startSlirp4netns) is what actually succeeds.
// --preserve-credentials is required too: without it, nsenter's own
// internal setgroups(2) call fails first (bwrap maps the sandbox's owning
// UID 1:1 rather than to 0, which nsenter's default credential-adjustment
// path doesn't handle), well before namespace entry is even attempted.
func applyEgressRestriction(innerPID int, proxyAddr string) error {
	_, port, err := splitHostPort(proxyAddr)
	if err != nil {
		return fmt.Errorf("parse proxy address %q: %w", proxyAddr, err)
	}

	// Defensive re-check, SECURITY_REVIEW.md finding #4: innerPID was
	// discovered by the caller some time before this function runs, and a
	// fast-exiting sandbox's PID can be reaped and reused by an unrelated
	// process in that window (the same class of race InnerNamespacePID's
	// own doc comment already describes). Without this, a "successful" nft
	// run below could silently apply this ruleset to that unrelated
	// process's namespace instead of erroring — narrows, does not fully
	// eliminate, the race (a check-then-act gap always remains); that's the
	// accepted standard elsewhere in this file too, not a regression here.
	if !isAlivePID(innerPID) {
		return errSandboxAlreadyExited
	}

	// Destination is slirpGatewayAddr, not proxyAddr's own host — see its
	// doc comment. The sandbox's own outbound traffic toward the proxy is
	// addressed to the gateway from the sandbox's point of view; that's
	// what slirp4netns forwards to the real host loopback, so that's what
	// this rule needs to allow.
	ruleset := fmt.Sprintf(`
table inet muro_egress {
	chain output {
		type filter hook output priority 0; policy drop;
		oif "lo" accept
		ip daddr %s tcp dport %s accept
	}
}
`, slirpGatewayAddr, port)

	cmd := exec.Command("nsenter",
		"--target", strconv.Itoa(innerPID),
		"--user", "--net", "--preserve-credentials",
		"--", "nft", "-f", "-",
	)
	cmd.Stdin = strings.NewReader(ruleset)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", fmt.Errorf("no port in address")
	}
	return addr[:i], addr[i+1:], nil
}
