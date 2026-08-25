package config

import (
	"os"
	"path/filepath"
)

// ConfigDir returns the muro user configuration directory:
// $XDG_CONFIG_HOME/muro, or ~/.config/muro if XDG_CONFIG_HOME is unset.
func ConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "muro"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "muro"), nil
}

// StateDir returns the muro daemon-owned runtime state directory:
// $XDG_STATE_HOME/muro, or ~/.local/state/muro if XDG_STATE_HOME is unset.
func StateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "muro"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "muro"), nil
}

// ProfilesDir returns the directory holding reusable sandbox profiles:
// <ConfigDir>/profiles.
func ProfilesDir() (string, error) {
	cfgDir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfgDir, "profiles"), nil
}

// SandboxLogPath returns the per-sandbox captured-output log file path,
// DESIGN.md §6's convention: <StateDir>/logs/sandbox/<namespace>__<name>.log.
// Deliberately derived purely from namespace/name rather than stored
// anywhere in state.json — muro-shim (the writer) and the control server's
// logs handler (the reader) can each compute the same path independently,
// with no extra state to keep in sync.
func SandboxLogPath(namespace, name string) (string, error) {
	stateDir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "logs", "sandbox", namespace+"__"+name+".log"), nil
}
