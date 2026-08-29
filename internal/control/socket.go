package control

import (
	"path/filepath"

	"github.com/thomkin/muro/internal/config"
)

// ResolveSocketPath returns the control socket path murod actually listens
// on: daemon.yaml's control_socket_path if the file exists and sets one,
// otherwise the built-in default (~/.local/state/muro/control.sock). Both
// cmd/muro (internal/cli) and `muro tui` (internal/tui) need this same
// resolution — kept here, in the one package both already depend on for
// Client/Dial, rather than in internal/cli, which internal/tui must not
// import (internal/cli constructs and runs the tui program, so the
// dependency only goes one way).
func ResolveSocketPath() string {
	if cfgDir, err := config.ConfigDir(); err == nil {
		if cfg, err := config.LoadDaemonConfig(filepath.Join(cfgDir, "daemon.yaml")); err == nil && cfg.ControlSocketPath != "" {
			return cfg.ControlSocketPath
		}
	}
	return config.DefaultDaemonConfig().ControlSocketPath
}
