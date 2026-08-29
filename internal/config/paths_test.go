package config

import (
	"path/filepath"
	"testing"
)

func TestConfigDir_XDGOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgcfg")
	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}
	want := "/tmp/xdgcfg/muro"
	if got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
}

func TestConfigDir_HomeFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	got, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir() error: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".config", "muro")
	if got != want {
		t.Errorf("ConfigDir() = %q, want %q", got, want)
	}
}

func TestStateDir_XDGOverride(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	got, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir() error: %v", err)
	}
	want := "/tmp/xdgstate/muro"
	if got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

func TestStateDir_HomeFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	got, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir() error: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".local", "state", "muro")
	if got != want {
		t.Errorf("StateDir() = %q, want %q", got, want)
	}
}

func TestProfilesDir_UnderConfigDirByDefault(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdgcfg")
	got, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir() error: %v", err)
	}
	want := filepath.Join("/tmp/xdgcfg", "muro", "profiles")
	if got != want {
		t.Errorf("ProfilesDir() = %q, want %q", got, want)
	}
}

func TestProfilesDir_HomeFallbackWhenNoXDG(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	got, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir() error: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".config", "muro", "profiles")
	if got != want {
		t.Errorf("ProfilesDir() = %q, want %q", got, want)
	}
}

func TestProfilesDir_EnvOverride(t *testing.T) {
	t.Setenv("MURO_PROFILES_DIR", "/tmp/custom-profiles")
	got, err := ProfilesDir()
	if err != nil {
		t.Fatalf("ProfilesDir() error: %v", err)
	}
	if got != "/tmp/custom-profiles" {
		t.Errorf("ProfilesDir() = %q, want /tmp/custom-profiles", got)
	}
}
