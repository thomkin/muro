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
