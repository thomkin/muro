// Command muro-broker is a standalone MQTT broker for muro's pub/sub bus
// (DESIGN.md §4/§8). It is a thin, muro-unaware wrapper around
// mochi-mqtt/server/v2, always run as its own separate process — murod
// connects to it exactly like any other MQTT broker, whether it's running
// on localhost for local development or self-hosted remotely for
// production. muro-broker itself has no concept of muro's topic root or
// any other muro-specific behavior.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("muro-broker exited with error", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := parseFlags(args)
	if err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	server := mqtt.New(nil)

	if err := addAuthHook(server, cfg); err != nil {
		return fmt.Errorf("configure auth: %w", err)
	}

	tcp := listeners.NewTCP(listeners.Config{
		ID:      "muro-broker-tcp",
		Address: cfg.Listen,
	})
	if err := server.AddListener(tcp); err != nil {
		return fmt.Errorf("add TCP listener on %s: %w", cfg.Listen, err)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// server.Serve() is NOT blocking in mochi-mqtt/server/v2: it starts the
	// configured listeners' accept loops in their own goroutines and returns
	// nil immediately once they're up. serveErr therefore only ever carries
	// a genuine startup error, never a normal "done serving" signal — the
	// select below waits on the OS signal channel for the actual shutdown
	// trigger, exactly like the library's own examples do.
	serveErr := make(chan error, 1)
	go func() {
		if err := server.Serve(); err != nil {
			serveErr <- err
		}
	}()

	slog.Info("muro-broker listening", "address", cfg.Listen, "auth", cfg.authEnabled())

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case sig := <-sigs:
		slog.Info("muro-broker caught signal, shutting down", "signal", sig.String())
		if err := server.Close(); err != nil {
			return fmt.Errorf("close server: %w", err)
		}
		slog.Info("muro-broker stopped")
		return nil
	}
}

// addAuthHook wires up mochi-mqtt's hook-based auth. With no
// username/password configured, all connections are allowed (matching the
// examples' AllowHook default); with credentials configured, only clients
// presenting that exact username/password are allowed, and everyone else is
// denied — mochi's auth.Ledger denies by default unless a rule explicitly
// allows.
func addAuthHook(server *mqtt.Server, cfg *brokerConfig) error {
	if !cfg.authEnabled() {
		return server.AddHook(new(auth.AllowHook), nil)
	}

	ledger := &auth.Ledger{
		Auth: auth.AuthRules{
			{Username: auth.RString(cfg.Username), Password: auth.RString(cfg.Password), Allow: true},
		},
		ACL: auth.ACLRules{
			{
				Username: auth.RString(cfg.Username),
				Filters: auth.Filters{
					"#": auth.ReadWrite,
				},
			},
		},
	}

	return server.AddHook(new(auth.Hook), &auth.Options{
		Ledger: ledger,
	})
}
