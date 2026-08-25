// Command muro-shim is the persistent process that makes a sandbox
// survive a murod restart. murod (internal/sandbox.BwrapIsolator.Launch)
// spawns one of these per sandbox, detached into its own session, rather
// than exec'ing bwrap directly and holding its pty in-process — the
// original design, which meant the kernel EOFed/EIOed the sandbox's pty
// the instant murod's own process (and therefore its only reference to
// the pty master) exited, killing the sandbox even on a clean daemon
// restart (see git history: "Wire cmd/murod (Stage 1)").
//
// muro-shim's job is narrow and entirely mechanical: read a launch spec
// (internal/sandbox.ShimSpec) from the file named by its one argument,
// allocate a pty if asked, exec bwrap as ITS OWN child with that pty as
// its stdio, report bwrap's PID back to murod over an inherited ready-fd,
// then hold the pty master open and relay it over a Unix socket to
// whichever muro CLI client is currently attached — for as long as bwrap
// itself keeps running, independent of murod. It never imports or knows
// about anything else in muro (profiles, state, the control API); its
// only muro dependency is internal/sandbox, and only for the few pieces
// (ShimSpec/ShimStatus, OpenPTY, InnerNamespacePID) genuinely shared with
// bwrap.go to avoid the two implementations silently drifting apart.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thomkin/muro/internal/sandbox"
)

func main() {
	os.Exit(run())
}

// run's return value becomes this process's own exit code — deliberately
// propagated from bwrap's real exit code (not muro-shim's own trivial
// wrapper logic) once bwrap finishes, so that murod's own
// exec.Cmd.Wait() on this process (for a Handle it's still the parent
// of — bwrapHandle.waitLive, bwrap.go) recovers the sandboxed process's
// actual exit status, exactly as if murod had waited on bwrap directly.
func run() int {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: muro-shim <spec-file>")
		return 2
	}

	// fd 3 is BwrapIsolator.Launch's ready-fd (bwrap.go) — inherited open
	// by this process via ExtraFiles when murod spawned it. It MUST be
	// marked close-on-exec before bwrap is started below, or bwrap (and
	// transitively every process it in turn execs, including whatever the
	// sandbox actually runs) inherits it too — Go does not do this
	// automatically for a raw fd wrapped via os.NewFile, only for the
	// handful it manages itself (Stdin/Stdout/Stderr/ExtraFiles on THIS
	// process's own exec.Cmd calls). Confirmed empirically, a real bug:
	// without this, readShimReady's io.ReadAll on murod's side blocks
	// until every open copy of the pipe's write end closes — for a fast
	// one-shot sandboxed command this was invisible (its accidental extra
	// copy closed within milliseconds), but a longer-lived one (e.g.
	// `sleep 20`) held it open for its entire run, timing Launch out
	// waiting for a "ready" signal that had, in fact, already fired.
	unix.CloseOnExec(3)

	spec, err := readAndConsumeSpec(os.Args[1])
	if err != nil {
		reportErr(err)
		return 1
	}

	cmd := exec.Command(spec.BwrapPath, spec.Args...)

	var ptmx *os.File
	var ln net.Listener
	if spec.PTY {
		m, s, err := sandbox.OpenPTY()
		if err != nil {
			reportErr(fmt.Errorf("allocate pty: %w", err))
			return 1
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s
		// Setsid, deliberately NOT Setctty — identical reasoning to
		// bwrap.go's original comment (now here, since this process is
		// bwrap's actual parent): making the pty slave bwrap's
		// controlling terminal would tie its survival to whichever
		// process holds the pty MASTER open, defeating the entire point
		// of this binary existing. muro-shim itself already holds ptmx
		// for bwrap's whole life, independent of any controlling-terminal
		// semantics.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		// Listen BEFORE Start, not after: reportReady (below) is murod's
		// only synchronization signal that this sandbox is usable, and
		// Handle.Stdio (bwrap.go) dials SocketPath the moment a caller
		// asks — with no separate "the attach socket is ready too" signal,
		// listening only after reportReady left a real race where a fast
		// caller could dial before this line ever ran. Confirmed
		// empirically against TestPTY_LaunchProducesUsablePseudoTerminal.
		if err := os.MkdirAll(filepath.Dir(spec.SocketPath), 0o700); err != nil {
			log.Printf("muro-shim: create socket dir: %v", err)
		} else {
			os.Remove(spec.SocketPath) // stale socket from a previous shim instance at this path, if any
			ln, err = net.Listen("unix", spec.SocketPath)
			if err != nil {
				log.Printf("muro-shim: listen on socket: %v", err)
			}
		}

		if err := cmd.Start(); err != nil {
			s.Close()
			m.Close()
			reportErr(fmt.Errorf("start bwrap: %w", err))
			return 1
		}
		s.Close() // bwrap has its own dup now; our copy was only needed until Start returned
		ptmx = m
	} else {
		if err := cmd.Start(); err != nil {
			reportErr(fmt.Errorf("start bwrap: %w", err))
			return 1
		}
	}
	defer func() {
		if ptmx != nil {
			ptmx.Close()
		}
	}()

	reportReady(cmd.Process.Pid)

	if ln != nil {
		defer ln.Close()
		defer os.Remove(spec.SocketPath)

		bcast := &ptyBroadcaster{}
		if spec.LogPath != "" {
			if lf, err := openLogFile(spec.LogPath); err != nil {
				log.Printf("muro-shim: open log file %s: %v (continuing without log capture)", spec.LogPath, err)
			} else {
				defer lf.Close()
				bcast.logFile = lf
			}
		}
		// One continuous reader for ptmx's whole life, independent of
		// whether anything is attached — this is what makes `muro logs`
		// see output produced while nobody was attached, and (since this
		// process itself survives a murod restart) output produced while
		// murod was down too. Previously ptmx was only ever read from
		// inside relay(), i.e. only while a client happened to be
		// connected; anything the sandbox wrote while unattended was lost.
		go drainToLog(ptmx, bcast)
		go acceptLoop(ln, ptmx, bcast)
	}

	installSignalForwarding(cmd)

	waitErr := cmd.Wait()
	return writeFinalStatus(spec.StatusPath, waitErr)
}

// readAndConsumeSpec reads and parses the ShimSpec at path, then deletes
// it — it's single-use, written fresh by BwrapIsolator.Launch for exactly
// this one invocation (bwrap.go's writeShimSpec).
func readAndConsumeSpec(path string) (sandbox.ShimSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sandbox.ShimSpec{}, fmt.Errorf("read spec file: %w", err)
	}
	os.Remove(path)

	var spec sandbox.ShimSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return sandbox.ShimSpec{}, fmt.Errorf("parse spec file: %w", err)
	}
	return spec, nil
}

// installSignalForwarding makes SIGTERM/SIGINT to muro-shim itself (what
// BwrapIsolator.Stop, bwrap.go, actually signals) correctly tear down
// bwrap's whole process tree, including the part a plain SIGTERM to the
// outer process can't reach: --uid 0 --gid 0 (added for Stage 3's nft)
// deepens that tree to three levels, and the middle one — PID 1 of
// bwrap's new pid namespace — IGNORES SIGTERM entirely by kernel design
// (unhandled signals to a namespace init are dropped, SIGKILL excepted).
// This logic used to live in BwrapIsolator.Stop itself, back when murod
// was bwrap's direct parent; it moved here verbatim because muro-shim is
// bwrap's parent now.
func installSignalForwarding(cmd *exec.Cmd) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	exited := make(chan struct{})

	go func() {
		select {
		case <-sigCh:
		case <-exited:
			return // bwrap already exited on its own; nothing to signal
		}
		if cmd.Process == nil {
			return
		}
		if innerPID, err := sandbox.InnerNamespacePID(cmd.Process.Pid); err == nil {
			if inner, err := os.FindProcess(innerPID); err == nil {
				_ = inner.Signal(syscall.SIGKILL)
			}
		}
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGKILL)
		}
	}()

	// run()'s own cmd.Wait() call (the only one — exec.Cmd.Wait must only
	// ever be invoked once) is what actually unblocks this; it can't
	// close exited itself since it runs concurrently with, not after,
	// that Wait(). Instead: a second, harmless goroutine watches the
	// process's liveness independently via signal-0 polling, purely to
	// close exited promptly once it's gone — the real Wait() result is
	// still always read from run()'s own single cmd.Wait() call.
	go func() {
		defer close(exited)
		if cmd.Process == nil {
			return
		}
		for isAlive(cmd.Process.Pid) {
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

func isAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || (!errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH))
}

// ptyBroadcaster is the single point every byte ptmx ever produces passes
// through: always to logFile (if set), and to whichever client connection
// is currently attached (if any) — see drainToLog. Kept separate from any
// one connection's lifetime so log capture works whether or not anyone is
// attached.
type ptyBroadcaster struct {
	mu      sync.Mutex
	current net.Conn // the one currently-attached connection, or nil
	logFile *os.File // write handle (append mode); nil if file-based log capture disabled
	replay  []byte   // bounded recent-output buffer, ALWAYS maintained regardless of logFile — see snapshot
}

// replayBufferCap bounds ptyBroadcaster.replay — generous enough to catch a
// newly-attaching client up on realistic recent output (a shell prompt,
// the last several lines of a command) without letting memory grow
// unbounded for a long-lived sandbox nobody's had file-based log capture
// configured for.
const replayBufferCap = 64 * 1024

func (b *ptyBroadcaster) setCurrent(c net.Conn) {
	b.mu.Lock()
	b.current = c
	b.mu.Unlock()
}

func (b *ptyBroadcaster) write(p []byte) {
	if b.logFile != nil {
		_, _ = b.logFile.Write(p) // best-effort; a log write failure shouldn't affect the live sandbox
	}
	b.mu.Lock()
	b.replay = append(b.replay, p...)
	if len(b.replay) > replayBufferCap {
		b.replay = b.replay[len(b.replay)-replayBufferCap:]
	}
	c := b.current
	b.mu.Unlock()
	if c != nil {
		_, _ = c.Write(p) // best-effort; a dead connection is handled by acceptLoop's own Read loop noticing it disconnected
	}
}

// snapshot returns a copy of whatever's currently in the replay buffer —
// deliberately NOT sourced from logFile: a direct Isolator caller (e.g.
// test/integration's own tests) may have LogPath unset entirely (file-based
// capture disabled), and replay-on-attach needs to work regardless of
// whether that's configured. Confirmed as a real gap, not hypothetical:
// TestPTY_LaunchProducesUsablePseudoTerminal constructs a LaunchSpec
// directly with no LogPath and failed against a file-snapshot-only version
// of this fix.
func (b *ptyBroadcaster) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, len(b.replay))
	copy(out, b.replay)
	return out
}

// openLogFile opens spec.LogPath for appending, creating its parent
// directory (DESIGN.md §6: <StateDir>/logs/sandbox/) if needed.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// drainToLog is the ONE continuous reader of ptmx for the shim's whole
// life — it runs regardless of whether any client is attached, so output
// produced while unattended (including the entire window while murod is
// down, since this process survives that) still reaches bcast.logFile.
// Returns once ptmx closes (bwrap exited); no stop signal needed, unlike
// the old per-connection relay loop this replaced.
func drainToLog(ptmx *os.File, bcast *ptyBroadcaster) {
	buf := make([]byte, 4096)
	for {
		n, err := ptmx.Read(buf)
		if n > 0 {
			bcast.write(buf[:n])
		}
		if err != nil {
			return // ptmx closed — bwrap exited
		}
	}
}

// acceptLoop serves attach connections one at a time — matching Manager's
// own "exactly one attacher at a time" semantics (DESIGN.md §12) at the
// muro level, this only needs to relay one connection at a time too, so
// handleAttachConn() is called synchronously rather than per-connection
// goroutines.
func acceptLoop(ln net.Listener, ptmx *os.File, bcast *ptyBroadcaster) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (shim shutting down) or a real error either way — stop accepting
		}
		if ptmx == nil {
			conn.Close()
			continue
		}
		handleAttachConn(conn, ptmx, bcast)
	}
}

// handleAttachConn registers conn as bcast's current subscriber (so it
// receives everything drainToLog reads, live), replays whatever's already
// been captured so far (so a client attaching after output has already
// happened still sees it — tmux/screen-style, and the fix for a real
// regression: drainToLog starts consuming ptmx immediately at launch, so a
// fast command's early output could already be gone from the live stream
// by the time a client dials — confirmed via
// TestPTY_LaunchProducesUsablePseudoTerminal), then relays conn's own
// input into ptmx (keystrokes) until conn disconnects or a write to ptmx
// fails.
//
// Registered as current BEFORE reading the replay snapshot, deliberately:
// anything drainToLog writes in the narrow window between registration and
// the snapshot being taken is delivered twice (once via replay, once live)
// rather than potentially not at all — duplication is a far more
// acceptable failure mode here than silently dropping output. Safe to
// clear bcast's current unconditionally on return: acceptLoop only ever
// has one of these active at a time, so there's no newer connection it
// could clobber.
func handleAttachConn(conn net.Conn, ptmx *os.File, bcast *ptyBroadcaster) {
	defer conn.Close()
	bcast.setCurrent(conn)
	defer bcast.setCurrent(nil)

	if snap := bcast.snapshot(); len(snap) > 0 {
		if _, werr := conn.Write(snap); werr != nil {
			return
		}
	}

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, werr := ptmx.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return // client disconnected
		}
	}
}

// writeFinalStatus records bwrap's real exit outcome to statusPath
// (atomically — temp file + rename, same pattern internal/state.Store
// uses) so a Handle reconstructed after a murod restart can recover it
// (bwrap.go's waitReconstructed), and returns the process exit code this
// binary's own main() should exit with, so a murod that's STILL this
// shim's parent recovers the same information the ordinary way, via
// exec.Cmd.Wait's ExitError.
func writeFinalStatus(path string, waitErr error) int {
	var st sandbox.ShimStatus
	code := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			code = exitErr.ExitCode()
			st.ExitCode = code
		} else {
			st.Err = waitErr.Error()
			code = 1
		}
	}
	data, err := json.Marshal(st)
	if err == nil {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, data, 0o600); err == nil {
			_ = os.Rename(tmp, path)
		}
	}
	return code
}

// reportReady / reportErr write muro-shim's one and only readiness
// message to its inherited ready-fd (fd 3, ExtraFiles[0] on the exec.Cmd
// BwrapIsolator.Launch built) — "OK <bwrap-pid>" or "ERR <message>",
// matching the exact format bwrap.go's readShimReady parses
// (shimReadyOKPrefix/shimReadyErrPrefix, shim.go). Writing to fd 3 when
// nothing is actually connected there (e.g. a manual/debugging
// invocation) just errors silently — there's no reasonable fallback
// action to take, and it doesn't affect this process's real job.
func reportReady(bwrapPID int) {
	writeReadyFD(fmt.Sprintf("OK %d\n", bwrapPID))
}

func reportErr(err error) {
	writeReadyFD(fmt.Sprintf("ERR %s\n", err.Error()))
}

func writeReadyFD(line string) {
	fd3 := os.NewFile(3, "ready-fd")
	if fd3 == nil {
		return
	}
	defer fd3.Close()
	_, _ = io.WriteString(fd3, line)
}
