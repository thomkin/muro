package pubsub

import (
	"testing"

	"github.com/thomkin/muro/internal/config"
)

// TestBuildClientOptions covers what NewClient/Connect need right before
// any live network connection is involved: broker address, auth, client
// ID, and the presence-topic LWT (client.go's presenceTopic doc comment
// explains why this is a presence topic rather than a per-claim LWT).
func TestBuildClientOptions(t *testing.T) {
	cfg := config.BrokerConfig{
		Address:  "localhost:1883",
		Username: "murod",
		Password: "secret",
	}
	opts := buildClientOptions(cfg, "muro", "daemon-abc")

	if len(opts.Servers) != 1 {
		t.Fatalf("Servers = %v, want exactly 1 broker", opts.Servers)
	}
	if got := opts.Servers[0].Host; got != "localhost:1883" {
		t.Errorf("broker host = %q, want %q", got, "localhost:1883")
	}
	if opts.ClientID != "murod-daemon-abc" {
		t.Errorf("ClientID = %q, want %q", opts.ClientID, "murod-daemon-abc")
	}
	if opts.Username != "murod" {
		t.Errorf("Username = %q, want %q", opts.Username, "murod")
	}
	if opts.Password != "secret" {
		t.Errorf("Password = %q, want %q", opts.Password, "secret")
	}

	if !opts.WillEnabled {
		t.Fatal("WillEnabled = false, want true (presence-topic LWT must be set)")
	}
	wantWillTopic := "muro/_daemons/daemon-abc/alive"
	if opts.WillTopic != wantWillTopic {
		t.Errorf("WillTopic = %q, want %q", opts.WillTopic, wantWillTopic)
	}
	if string(opts.WillPayload) != "offline" {
		t.Errorf("WillPayload = %q, want %q", opts.WillPayload, "offline")
	}
	if !opts.WillRetained {
		t.Error("WillRetained = false, want true (so a late subscriber sees the last known state)")
	}
}

func TestBuildClientOptions_NoAuthWhenCredentialsEmpty(t *testing.T) {
	cfg := config.BrokerConfig{Address: "localhost:1883"}
	opts := buildClientOptions(cfg, "muro", "daemon-abc")
	if opts.Username != "" || opts.Password != "" {
		t.Errorf("expected no auth set, got Username=%q Password=%q", opts.Username, opts.Password)
	}
}

func TestNewClient_RejectsEmptyTopicRootOrDaemonID(t *testing.T) {
	cfg := config.BrokerConfig{Address: "localhost:1883"}

	if _, err := NewClient(cfg, "", "daemon-abc"); err == nil {
		t.Error("expected error for empty topicRoot, got nil")
	}
	if _, err := NewClient(cfg, "muro", ""); err == nil {
		t.Error("expected error for empty daemonID, got nil")
	}
}

// TestClient_PublishStatus_UsesFakeClient exercises PublishStatus against
// the in-memory fake, confirming the right topic/payload without a real
// broker.
func TestClient_PublishStatus_UsesFakeClient(t *testing.T) {
	fake := &fakeMQTTClient{}
	c := &Client{mqttClient: fake, topicRoot: "muro", daemonID: "daemon-abc"}

	if err := c.PublishStatus("default", "claude-1", "started"); err != nil {
		t.Fatalf("PublishStatus error: %v", err)
	}

	p, ok := fake.lastPublishTo("muro/default/claude-1/status")
	if !ok {
		t.Fatal("no publish recorded to the status topic")
	}
	if string(p.payload) != "started" {
		t.Errorf("payload = %q, want %q", p.payload, "started")
	}
}

func TestClient_PublishDenied_UsesFakeClient(t *testing.T) {
	fake := &fakeMQTTClient{}
	c := &Client{mqttClient: fake, topicRoot: "muro", daemonID: "daemon-abc"}

	if err := c.PublishDenied("default", "claude-1", "evil.example.com"); err != nil {
		t.Fatalf("PublishDenied error: %v", err)
	}

	p, ok := fake.lastPublishTo("muro/default/claude-1/net-denied")
	if !ok {
		t.Fatal("no publish recorded to the net-denied topic")
	}
	if string(p.payload) != "evil.example.com" {
		t.Errorf("payload = %q, want %q", p.payload, "evil.example.com")
	}
}
