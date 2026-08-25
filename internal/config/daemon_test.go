package config

import (
	"os"
	"path/filepath"
	"testing"
)

const exampleDaemonYAML = `
control_socket_path: ~/.local/state/muro/control.sock
broker:
  address: localhost:1883
  username: ""
  password: ""
mqtt:
  topic_root: muro
log_level: info
event_log_cap: 200
restart_backoff_cap: 5
`

func TestLoadDaemonConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(path, []byte(exampleDaemonYAML), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonConfig() error: %v", err)
	}

	if cfg.ControlSocketPath != "~/.local/state/muro/control.sock" {
		t.Errorf("ControlSocketPath = %q", cfg.ControlSocketPath)
	}
	if cfg.Broker.Address != "localhost:1883" {
		t.Errorf("Broker.Address = %q", cfg.Broker.Address)
	}
	if cfg.MQTT.TopicRoot != "muro" {
		t.Errorf("MQTT.TopicRoot = %q, want %q", cfg.MQTT.TopicRoot, "muro")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q", cfg.LogLevel)
	}
	if cfg.EventLogCap != 200 {
		t.Errorf("EventLogCap = %d, want 200", cfg.EventLogCap)
	}
	if cfg.RestartBackoffCap != 5 {
		t.Errorf("RestartBackoffCap = %d, want 5", cfg.RestartBackoffCap)
	}
}

func TestLoadDaemonConfig_DefaultsFillMissingFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.yaml")
	if err := os.WriteFile(path, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	cfg, err := LoadDaemonConfig(path)
	if err != nil {
		t.Fatalf("LoadDaemonConfig() error: %v", err)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q (explicit value should survive)", cfg.LogLevel, "debug")
	}
	if cfg.EventLogCap != 200 {
		t.Errorf("EventLogCap default = %d, want 200", cfg.EventLogCap)
	}
	if cfg.RestartBackoffCap != 5 {
		t.Errorf("RestartBackoffCap default = %d, want 5", cfg.RestartBackoffCap)
	}
	if cfg.MQTT.TopicRoot != "muro" {
		t.Errorf("MQTT.TopicRoot default = %q, want %q", cfg.MQTT.TopicRoot, "muro")
	}
}

func TestLoadDaemonConfig_MissingFile(t *testing.T) {
	if _, err := LoadDaemonConfig("/nonexistent/daemon.yaml"); err == nil {
		t.Error("expected an error for a missing file, got nil")
	}
}

func TestDefaultDaemonConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdgstate")
	cfg := DefaultDaemonConfig()
	wantSocket := "/tmp/xdgstate/muro/control.sock"
	if cfg.ControlSocketPath != wantSocket {
		t.Errorf("ControlSocketPath = %q, want %q", cfg.ControlSocketPath, wantSocket)
	}
	if cfg.Broker.Address != "localhost:1883" {
		t.Errorf("Broker.Address = %q", cfg.Broker.Address)
	}
}
