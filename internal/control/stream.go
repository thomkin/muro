package control

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"time"

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

// pumpPtyToConn relays sandbox output (pty) to the client (conn) until
// done is closed or the pty/connection errors. It polls via a short read
// deadline so it notices done being closed promptly rather than blocking
// forever in Read — if the underlying file doesn't support deadlines (not
// all *os.File values do), it falls back to a plain blocking Read, which
// still exits correctly once the pty itself closes (sandbox stop/exit),
// just not promptly on a mid-session detach.
func pumpPtyToConn(pty *os.File, conn net.Conn, done <-chan struct{}) {
	buf := make([]byte, 4096)
	supportsDeadline := pty.SetReadDeadline(time.Now().Add(200*time.Millisecond)) == nil
	for {
		select {
		case <-done:
			return
		default:
		}
		if supportsDeadline {
			_ = pty.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
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
