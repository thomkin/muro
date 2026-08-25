package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/sandbox"
)

// handleAttach implements DESIGN.md §12's sandbox.attach: after the JSON
// handshake Response below, this connection stops being a
// request/response protocol and becomes a raw bidirectional byte stream —
// the sandbox's pty — until the client sends sandbox.DetachSequence
// (Ctrl-P Ctrl-Q) or either side disconnects. r is the SAME bufio.Reader
// handleConn already used to read the sandbox.attach request line, so any
// bytes it may have buffered ahead of that line's newline (e.g. the very
// first keystrokes of an eager client) are not lost.
func (s *Server) handleAttach(conn net.Conn, r *bufio.Reader, req Request) {
	var areq SandboxAttachRequest
	if err := json.Unmarshal(req.Payload, &areq); err != nil {
		_ = writeResponse(conn, errResp(err))
		return
	}

	pty, detach, err := s.mgr.Attach(defaultNS(areq.Namespace), areq.Name)
	if err != nil {
		_ = writeResponse(conn, errResp(err))
		return
	}
	defer detach()

	if err := writeResponse(conn, okResp(SandboxAttachResponse{OK: true})); err != nil {
		return
	}

	done := make(chan struct{})
	defer close(done)
	go pumpPtyToConn(pty, conn, done)

	scanner := &detachScanner{}
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if scanner.feed(chunk) {
				// The detach sequence arrived — in practice a terminal in
				// raw mode flushes Ctrl-P Ctrl-Q as its own read with
				// nothing else mixed in, so treating "found anywhere in
				// this chunk" as "stop forwarding this whole chunk and
				// detach" is a deliberate, documented simplification
				// rather than a byte-exact splice around the sequence.
				return
			}
			if _, werr := pty.Write(chunk); werr != nil {
				return
			}
		}
		if err != nil {
			return // client disconnected, or a real read error
		}
	}
}

// handleLogs implements `muro logs`: after the JSON handshake Response
// below, this connection becomes a one-directional raw byte stream —
// muro-shim's continuously-captured pty output for this sandbox
// (DESIGN.md §6) — starting with whatever's already on disk, then (if
// Follow) newly-appended content as it happens, until the client
// disconnects. Unlike sandbox.attach, nothing the client sends is ever
// read as input to the sandbox; logs is read-only.
func (s *Server) handleLogs(conn net.Conn, req Request) {
	var lreq LogsRequest
	if err := json.Unmarshal(req.Payload, &lreq); err != nil {
		_ = writeResponse(conn, errResp(err))
		return
	}
	ns := defaultNS(lreq.Namespace)

	// Validate the sandbox is real before ever touching a log path, so a
	// typo'd name gets a clear "no such sandbox" rather than a confusing
	// empty stream (DESIGN.md — same reasoning handleSandboxShow already
	// applies for `muro sandbox show`).
	if _, ok := s.store.Get(ns, lreq.Name); !ok {
		_ = writeResponse(conn, errResp(fmt.Errorf("sandbox %s/%s not found", ns, lreq.Name)))
		return
	}

	logPath, err := config.SandboxLogPath(ns, lreq.Name)
	if err != nil {
		_ = writeResponse(conn, errResp(fmt.Errorf("compute log path: %w", err)))
		return
	}

	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			// A real sandbox that just hasn't produced (or had log
			// capture for) any output yet — not an error, an empty
			// stream. --follow still needs somewhere to poll, so treat
			// this the same as an empty file rather than bailing out.
			if err := writeResponse(conn, okResp(LogsResponse{OK: true})); err != nil {
				return
			}
			if lreq.Follow {
				followMissingThenMaybeCreated(conn, logPath, watchClientDisconnect(conn))
			}
			return
		}
		_ = writeResponse(conn, errResp(fmt.Errorf("open log file: %w", err)))
		return
	}
	defer f.Close()

	if err := writeResponse(conn, okResp(LogsResponse{OK: true})); err != nil {
		return
	}

	if _, err := io.Copy(conn, f); err != nil {
		return // client disconnected mid-copy, or a real read/write error either way
	}

	if lreq.Follow {
		followAppendedContent(conn, f, watchClientDisconnect(conn))
	}
}

// followMissingThenMaybeCreated handles --follow against a sandbox whose
// log file doesn't exist yet (e.g. muro logs --follow run an instant after
// muro run, before the shim's first write) — polls for the file to appear,
// then falls through to followAppendedContent once it does. done is shared
// across both phases: it's created once by handleLogs's caller and simply
// threaded through here, since watchClientDisconnect's underlying
// goroutine (and so done itself) is only ever meant to exist once per
// connection.
func followMissingThenMaybeCreated(conn net.Conn, path string, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			followAppendedContent(conn, f, done)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// followAppendedContent polls f for newly-appended data (muro-shim's
// continuous writer, cmd/muro-shim's drainToLog) and streams it to conn as
// it arrives, until done closes (the client disconnected). Polling, not
// inotify/fsnotify, matches pumpPtyToConn's existing idiom in this same
// file — no new dependency for what's already a short, harmless poll
// interval.
func followAppendedContent(conn net.Conn, f *os.File, done <-chan struct{}) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			return
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
			continue // more may already be waiting; don't sleep after a real read
		}
		if err != nil && err != io.EOF {
			return // a real read error, not just "caught up to EOF"
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// watchClientDisconnect returns a channel that's closed once conn's peer
// disconnects (detected via a blocking Read in its own goroutine, which
// returns EOF/an error only then, since `muro logs` never sends anything
// on this connection). The goroutine — and so the channel — only ever
// closes once, naturally, when the connection actually goes away
// (including the ordinary case: handleConn's own `defer conn.Close()`
// firing once handleLogs returns) — callers just select on it and never
// close it themselves, avoiding any close-of-closed-channel race.
func watchClientDisconnect(conn net.Conn) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1)
		_, _ = conn.Read(buf) // blocks until the peer disconnects, sends data (unexpected here), or conn.Close() unblocks it
	}()
	return done
}

// deadlineSetter is the optional capability pty (an io.ReadWriteCloser —
// Handle.Stdio() no longer promises a real *os.File now that it dials a
// shim process's Unix socket rather than holding an in-process pty fd,
// internal/sandbox/isolator.go) may additionally support. net.Conn always
// does; pumpPtyToConn checks for it via a type assertion rather than
// requiring it, the same "optional interface" idiom
// internal/sandbox/network.go uses for networkAddrProvider.
type deadlineSetter interface {
	SetReadDeadline(time.Time) error
}

// pumpPtyToConn relays sandbox output (pty) to the client (conn) until
// done is closed or the pty/connection errors. It polls via a short read
// deadline so it notices done being closed promptly rather than blocking
// forever in Read — if pty doesn't support deadlines, it falls back to a
// plain blocking Read, which still exits correctly once the pty itself
// closes (sandbox stop/exit), just not promptly on a mid-session detach.
func pumpPtyToConn(pty io.ReadWriteCloser, conn net.Conn, done <-chan struct{}) {
	buf := make([]byte, 4096)
	deadliner, supportsDeadline := pty.(deadlineSetter)
	if supportsDeadline {
		supportsDeadline = deadliner.SetReadDeadline(time.Now().Add(200*time.Millisecond)) == nil
	}
	for {
		select {
		case <-done:
			return
		default:
		}
		if supportsDeadline {
			_ = deadliner.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		}
		n, err := pty.Read(buf)
		if n > 0 {
			if _, werr := conn.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if supportsDeadline && os.IsTimeout(err) {
				continue
			}
			return
		}
	}
}

// detachScanner watches a stream of client input for sandbox.DetachSequence
// (Ctrl-P Ctrl-Q), split across arbitrarily-sized reads. This is a local
// re-implementation of the same logic internal/sandbox keeps privately for
// its own package's use (unexported there, so it can't be imported) —
// both watch for the same exported sandbox.DetachSequence constant, so
// the two stay in sync by construction even though the code is
// duplicated.
type detachScanner struct {
	matched int
}

func (s *detachScanner) feed(buf []byte) bool {
	seq := sandbox.DetachSequence
	for _, b := range buf {
		switch {
		case b == seq[s.matched]:
			s.matched++
			if s.matched == len(seq) {
				s.matched = 0
				return true
			}
		case b == seq[0]:
			s.matched = 1
		default:
			s.matched = 0
		}
	}
	return false
}
