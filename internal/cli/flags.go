package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/control"
)

// splitMountFlag parses one --mount host:sandbox_path:mode flag.
func splitMountFlag(f string) (host, sandboxPath, mode string, err error) {
	parts := strings.Split(f, ":")
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("--mount %q: expected host:sandbox_path:mode", f)
	}
	mode = parts[2]
	if mode != "ro" && mode != "rw" {
		return "", "", "", fmt.Errorf("--mount %q: mode must be \"ro\" or \"rw\", got %q", f, mode)
	}
	return parts[0], parts[1], mode, nil
}

// splitToolFlag parses one --tool <host-path>[:<as>] flag (DESIGN.md
// §9/§10) — <as> defaults to the host path's basename if omitted.
func splitToolFlag(f string) (host, as string, err error) {
	host, as, found := strings.Cut(f, ":")
	if host == "" {
		return "", "", fmt.Errorf("--tool %q: missing host path", f)
	}
	if !found || as == "" {
		as = filepath.Base(host)
	}
	return host, as, nil
}

// parseMountFlags parses repeated --mount flags for a control API request
// (sandbox update).
func parseMountFlags(flags []string) ([]control.MountView, error) {
	out := make([]control.MountView, 0, len(flags))
	for _, f := range flags {
		host, sandboxPath, mode, err := splitMountFlag(f)
		if err != nil {
			return nil, err
		}
		out = append(out, control.MountView{Host: host, SandboxPath: sandboxPath, Mode: mode})
	}
	return out, nil
}

// parseMountFlagsConfig parses repeated --mount flags into config.Mount,
// for writing directly into a profile file (profile create).
func parseMountFlagsConfig(flags []string) ([]config.Mount, error) {
	out := make([]config.Mount, 0, len(flags))
	for _, f := range flags {
		host, sandboxPath, mode, err := splitMountFlag(f)
		if err != nil {
			return nil, err
		}
		out = append(out, config.Mount{Host: host, SandboxPath: sandboxPath, Mode: mode})
	}
	return out, nil
}

// parseToolFlagsConfig parses repeated --tool flags into config.Tool, for
// writing directly into a profile file (profile create).
func parseToolFlagsConfig(flags []string) ([]config.Tool, error) {
	out := make([]config.Tool, 0, len(flags))
	for _, f := range flags {
		host, as, err := splitToolFlag(f)
		if err != nil {
			return nil, err
		}
		out = append(out, config.Tool{Host: host, As: as})
	}
	return out, nil
}
