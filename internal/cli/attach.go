package cli

import (
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// setRawMode puts f's file descriptor into raw mode (the standard
// "cfmakeraw" flag manipulation, done directly via TCGETS/TCSETS since
// golang.org/x/term is not a project dependency and x/sys/unix already is)
// and returns a func that restores the original mode. f must be a real
// terminal — setRawMode returns an error otherwise (e.g. stdin is piped),
// and the caller should proceed without raw mode in that case rather than
// failing the whole command.
func setRawMode(f *os.File) (restore func(), err error) {
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

// pumpAttach relays bytes bidirectionally between the local terminal
// (in/out) and the attach stream (remoteR/remoteW) until either side
// closes. The server (internal/control/stream.go) already watches for
// sandbox.DetachSequence and ends the stream on its own when it sees it —
// this function just needs to stop cleanly when that happens, not
// interpret the sequence itself.
func pumpAttach(in io.Reader, out io.Writer, remoteR io.Reader, remoteW io.Writer) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(remoteW, in)
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, remoteR)
		done <- err
	}()
	// Either direction ending (EOF on detach/disconnect, or a real error)
	// ends the attach session — don't wait for both, the other goroutine's
	// copy will end on its own once the connection is gone.
	return <-done
}
