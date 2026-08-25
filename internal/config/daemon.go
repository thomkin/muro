package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

// BrokerConfig describes how murod connects to the MQTT broker (local
// muro-broker or a self-hosted remote broker — same code path either way).
type BrokerConfig struct {
	Address  string `yaml:"address"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// MQTTConfig holds pub/sub settings that aren't about the broker
// connection itself.
type MQTTConfig struct {
	// TopicRoot roots every topic muro publishes/subscribes to, so it
	// can't collide with anything else on a shared broker (SPEC.md §8).
	TopicRoot string `yaml:"topic_root"`
}

// DaemonConfig is the on-disk shape of ~/.config/muro/daemon.yaml
// (DESIGN.md §7, IMPLEMENTATION.md §8).
type DaemonConfig struct {
	ControlSocketPath string       `yaml:"control_socket_path"`
	Broker            BrokerConfig `yaml:"broker"`
	MQTT              MQTTConfig   `yaml:"mqtt"`
	LogLevel          string       `yaml:"log_level"`
	EventLogCap       int          `yaml:"event_log_cap"`
	RestartBackoffCap int          `yaml:"restart_backoff_cap"`
}

// DefaultDaemonConfig returns the built-in defaults used when
// ~/.config/muro/daemon.yaml doesn't exist yet or omits a field.
func DefaultDaemonConfig() *DaemonConfig {
	socket := ""
	if stateDir, err := StateDir(); err == nil {
		socket = stateDir + "/control.sock"
	}
	return &DaemonConfig{
		ControlSocketPath: socket,
		Broker: BrokerConfig{
			Address: "localhost:1883",
		},
		MQTT: MQTTConfig{
			TopicRoot: "muro",
		},
		LogLevel:          "info",
		EventLogCap:       200,
		RestartBackoffCap: 5,
	}
}

// LoadDaemonConfig reads and parses daemon.yaml at path, filling in
// defaults for any field the file omits.
func LoadDaemonConfig(path string) (*DaemonConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg := DefaultDaemonConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, err
	}

	if cfg.EventLogCap <= 0 {
		cfg.EventLogCap = 200
	}
	if cfg.RestartBackoffCap <= 0 {
		cfg.RestartBackoffCap = 5
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.MQTT.TopicRoot == "" {
		cfg.MQTT.TopicRoot = "muro"
	}

	return cfg, nil
}
