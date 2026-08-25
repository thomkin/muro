// Command murod is muro's daemon: it owns sandbox lifecycle, the
// URL-allowlist proxy, and the pub/sub connection, and serves the CLI's
// control API over a Unix socket (DESIGN.md §5, IMPLEMENTATION.md §6).
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/control"
	"github.com/thomkin/muro/internal/proxy"
	"github.com/thomkin/muro/internal/pubsub"
	"github.com/thomkin/muro/internal/sandbox"
	"github.com/thomkin/muro/internal/state"
)

// proxyListenAddr is murod's URL-allowlist proxy listen address. Fixed
// (not :0/OS-assigned) so that Stage 2's slirp4netns per-sandbox network
// bridge has a consistent, known destination to route toward — an
// OS-assigned port would be a moving target no bridge setup could rely on.
const proxyListenAddr = "127.0.0.1:18080"

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.Default()

	cfg := loadDaemonConfig(logger)

	store, err := openStateStore(logger)
	if err != nil {
		logger.Error("open state store", "error", err)
		return 1
	}

	isolator, err := sandbox.NewBwrapIsolator()
	if err != nil {
		logger.Error("sandbox isolation unavailable", "error", err)
		return 1
	}

	proxySrv := proxy.NewServer(store)
	go func() {
		if err := proxySrv.ListenAndServe(proxyListenAddr); err != nil {
			logger.Error("proxy listener stopped", "error", err)
		}
	}()
	logger.Info("proxy listening", "addr", proxyListenAddr)

	pubsubClient, brokerChecker := connectPubSub(cfg, logger)

	var publisher sandbox.EventPublisher
	if pubsubClient != nil {
		publisher = pubsubClient
	}

	mgr := sandbox.NewManager(store, isolator, proxySrv, publisher)

	controlSrv := control.NewServer(mgr, store, brokerChecker)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, os.Interrupt)
	go func() {
		sig := <-sigCh
		logger.Info("shutting down", "signal", sig.String())
		// Deliberately does NOT stop running sandboxes (IMPLEMENTATION.md
		// §6): only `muro sandbox stop` should do that. A daemon restart
		// must not kill an agent mid-session.
		if pubsubClient != nil {
			pubsubClient.Disconnect()
		}
		_ = controlSrv.Close()
	}()

	logger.Info("murod starting", "control_socket", cfg.ControlSocketPath)
	if err := controlSrv.ListenAndServe(cfg.ControlSocketPath); err != nil {
		logger.Error("control server stopped", "error", err)
		return 1
	}
	logger.Info("murod stopped cleanly")
	return 0
}

// loadDaemonConfig reads daemon.yaml, falling back to built-in defaults on
// any error (missing file, unreadable, malformed) — a daemon that can't
// find its own config should still start with sane defaults rather than
// refuse to run, matching DESIGN.md §7's config being "input, not the
// source of truth" framing.
func loadDaemonConfig(logger *slog.Logger) *config.DaemonConfig {
	cfgDir, err := config.ConfigDir()
	if err != nil {
		logger.Warn("resolve config dir, using built-in defaults", "error", err)
		return config.DefaultDaemonConfig()
	}
	path := filepath.Join(cfgDir, "daemon.yaml")
	cfg, err := config.LoadDaemonConfig(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logger.Warn("load daemon.yaml, using built-in defaults", "path", path, "error", err)
		}
		return config.DefaultDaemonConfig()
	}
	return cfg
}

func openStateStore(logger *slog.Logger) (*state.Store, error) {
	stateDir, err := config.StateDir()
	if err != nil {
		return nil, fmt.Errorf("resolve state dir: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	store := state.NewStore(filepath.Join(stateDir, "state.json"))
	if err := store.Load(); err != nil {
		return nil, fmt.Errorf("load state.json: %w", err)
	}
	if err := state.Reconcile(store); err != nil {
		logger.Warn("startup reconciliation had errors", "error", err)
	}
	return store, nil
}

// connectPubSub tries to connect to the configured MQTT broker. A failed
// connection is not fatal — sandbox management works without pub/sub — but
// the returned publisher is nil in that case, so Manager doesn't spend
// every operation waiting out a 5s publish timeout against a broker that
// isn't there.
func connectPubSub(cfg *config.DaemonConfig, logger *slog.Logger) (*pubsub.Client, control.BrokerStatusChecker) {
	daemonID, err := newDaemonID()
	if err != nil {
		logger.Warn("generate daemon id, pub/sub disabled this run", "error", err)
		return nil, nil
	}

	client, err := pubsub.NewClient(cfg.Broker, cfg.MQTT.TopicRoot, daemonID)
	if err != nil {
		logger.Warn("construct pub/sub client, disabled this run", "error", err)
		return nil, nil
	}

	checker := &brokerStatus{address: cfg.Broker.Address}
	if err := client.Connect(); err != nil {
		logger.Warn("connect to MQTT broker, pub/sub disabled this run (sandboxes still work)", "broker", cfg.Broker.Address, "error", err)
		checker.lastErr = err
		return nil, checker
	}

	logger.Info("connected to MQTT broker", "broker", cfg.Broker.Address, "topic_root", cfg.MQTT.TopicRoot, "daemon_id", daemonID)
	checker.connected = true
	return client, checker
}

func newDaemonID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "murod-" + hex.EncodeToString(buf), nil
}

// brokerStatus implements control.BrokerStatusChecker. It's a static
// snapshot taken at connect time, not live — internal/pubsub.Client doesn't
// currently expose a live reconnect/health signal (that wasn't part of its
// original task scope), so `muro broker status` reports "as of daemon
// startup" rather than continuously monitoring the connection. Good enough
// for v1; a live health check is a reasonable future addition to
// internal/pubsub.Client rather than something to fake here.
type brokerStatus struct {
	address   string
	connected bool
	lastErr   error
}

func (b *brokerStatus) Status() (connected bool, address string, lastErr error) {
	return b.connected, b.address, b.lastErr
}
