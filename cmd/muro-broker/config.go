package main

import (
	"errors"
	"flag"
)

// brokerConfig is muro-broker's entire configuration surface. It is
// deliberately minimal and flag-driven: muro-broker is a generic,
// muro-unaware MQTT broker (DESIGN.md) with no topic-root or muro-specific
// concept, so it doesn't need profile/daemon.yaml-style config loading.
type brokerConfig struct {
	Listen   string // TCP listen address, e.g. ":1883" or "127.0.0.1:1883"
	Username string // optional; if set, Password must also be set
	Password string // optional; if set, Username must also be set
}

// parseFlags parses os.Args[1:]-style arguments into a brokerConfig.
func parseFlags(args []string) (*brokerConfig, error) {
	fs := flag.NewFlagSet("muro-broker", flag.ContinueOnError)
	listen := fs.String("listen", ":1883", "TCP address to listen on for MQTT connections")
	username := fs.String("username", "", "optional: require this username for all connections")
	password := fs.String("password", "", "optional: required password for --username")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &brokerConfig{
		Listen:   *listen,
		Username: *username,
		Password: *password,
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *brokerConfig) validate() error {
	if c.Listen == "" {
		return errors.New("--listen must not be empty")
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("--username and --password must be set together, or not at all")
	}
	return nil
}

// authEnabled reports whether this config requires username/password auth
// rather than allowing all connections.
func (c *brokerConfig) authEnabled() bool {
	return c.Username != "" && c.Password != ""
}
