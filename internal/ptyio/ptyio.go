// Package ptyio holds the terminal-raw-mode and byte-pumping primitives
// behind `muro sandbox attach` — extracted out of internal/cli so both
// internal/cli (the plain attach command) and internal/tui (the `muro tui`
// live pane) can share exactly one implementation without internal/cli and
// internal/tui importing each other (internal/cli constructs and runs the
// TUI program, so the dependency has to go the other way).
package ptyio

import (
	"io"
	"os"
	"sync"

	"github.com/muesli/cancelreader"
	"golang.org/x/sys/unix"
)

// SetRawMode puts f's file descriptor into raw mode (the standard
// "cfmakeraw" flag manipulation, done directly via TCGETS/TCSETS since
// golang.org/x/term is not a project dependency and x/sys/unix already is)
// and returns a func that restores the original mode. f must be a real
// terminal — SetRawMode returns an error otherwise (e.g. stdin is piped),
// and the caller should proceed without raw mode in that case rather than
// failing the whole command.
func SetRawMode(f *os.File) (restore func(), err error) {
	fd := int(f.Fd())
	orig, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}

	raw := *orig
	raw.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &raw); err != nil {
		return nil, err
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, orig)
		})
	}, nil
}

// Pump relays bytes bidirectionally between the local terminal (in/out)
// and the attach stream (remoteR/remoteW) until either side closes. The
// server (internal/control/stream.go) already watches for
// sandbox.DetachSequence and ends the stream on its own when it sees it —
// this function just needs to stop cleanly when that happens, not
// interpret the sequence itself.
//
// Returns as soon as EITHER direction ends (EOF on detach/disconnect, or a
// real error) — it does not wait for both. When remoteR ends first (the
// normal detach case), the OTHER goroutine is still blocked on in.Read()
// at that point with nothing to unblock it; in is wrapped in a
// cancelreader.CancelReader specifically so that blocked Read can be force-
// canceled before Pump returns, rather than leaking a goroutine holding a
// live read against in indefinitely. This was harmless for the original
// caller (`muro sandbox attach`, whose process exits immediately after
// Pump returns, reaping the leak along with it) but is load-bearing for a
// caller that keeps running afterward and needs `in` back for its own
// purposes — confirmed by direct reproduction: `muro tui`'s attach/detach
// cycle silently stopped accepting any further keyboard input at all,
// because the leaked goroutine kept consuming (and discarding, since its
// write side was already dead) every keystroke race-won away from Bubble
// Tea's own newly re-initialized stdin reader.
func Pump(in io.Reader, out io.Writer, remoteR io.Reader, remoteW io.Writer) error {
	cr, crErr := cancelreader.NewReader(in)
	inReader := in
	if crErr == nil {
		inReader = cr
	}

	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(remoteW, inReader)
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, remoteR)
		done <- err
	}()
	err := <-done
	if crErr == nil {
		cr.Cancel()
	}
	return err
}
