package sandbox

import (
	"context"
	"os"

	"github.com/thomkin/muro/internal/config"
)

// LaunchSpec is everything an Isolator needs to start one sandboxed
// process.
type LaunchSpec struct {
	Mounts []config.Mount
	Tools  []config.Tool
	Env    map[string]string
	Cmd    []string
	PTY    bool
}

// Isolator is the sandboxing backend (DESIGN.md §6.1). v1 wraps the bwrap
// binary (bwrap.go, a later milestone); a native Go namespace
// implementation can be swapped in later without touching Manager, the
// proxy, or pub/sub, since nothing outside this package depends on which
// Isolator is in use.
type Isolator interface {
	Launch(ctx context.Context, spec LaunchSpec) (Handle, error)
	UpdateMounts(h Handle, mounts []config.Mount) (applied bool, err error)
	Stop(h Handle) error
}

// Handle is a live reference to one launched sandbox process.
type Handle interface {
	PID() int
	Wait() (exitCode int, err error)
	Stdio() (pty *os.File, ok bool)
}
