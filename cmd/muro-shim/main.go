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
		go acceptLoop(ln, ptmx)
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

// acceptLoop serves attach connections one at a time — matching Manager's
// own "exactly one attacher at a time" semantics (DESIGN.md §12) at the
// muro level, this only needs to relay one connection at a time too, so
// relay() is called synchronously rather than per-connection goroutines,
// which also sidesteps any need to coordinate concurrent access to ptmx.
func acceptLoop(ln net.Listener, ptmx *os.File) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (shim shutting down) or a real error either way — stop accepting
		}
		if ptmx == nil {
			conn.Close()
			continue
		}
		relay(conn, ptmx)
	}
}

// relay pumps bytes bidirectionally between conn and ptmx until conn
// disconnects or ptmx closes (bwrap exited). It polls ptmx via a short
// read deadline, the same pattern internal/control/stream.go's
// pumpPtyToConn already uses, so a client disconnect stops the
// output-relay goroutine within ~200ms instead of leaking it until ptmx
// itself next produces output.
func relay(conn net.Conn, ptmx *os.File) {
	defer conn.Close()
	stop := make(chan struct{})

	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = ptmx.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := ptmx.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				if os.IsTimeout(err) {
					continue
				}
				return // ptmx closed — bwrap exited
			}
		}
	}()
	defer close(stop)

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
