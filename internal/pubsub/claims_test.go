package pubsub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func newTestClient(fake *fakeMQTTClient, lookup claimLookup, daemonID string) *Client {
	return &Client{
		mqttClient: fake,
		topicRoot:  "muro",
		daemonID:   daemonID,
		lookup:     lookup,
	}
}

func TestClaimSandbox_NoExistingClaim_Succeeds(t *testing.T) {
	fake := &fakeMQTTClient{}
	c := newTestClient(fake, &fakeClaimLookup{exists: false}, "daemon-a")

	if err := c.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("ClaimSandbox error: %v", err)
	}

	p, ok := fake.lastPublishTo("muro/_claims/default/claude-1")
	if !ok {
		t.Fatal("expected a retained claim publish, got none")
	}
	if !p.retained {
		t.Error("claim publish must be retained")
	}
	var claim claimPayload
	if err := json.Unmarshal(p.payload, &claim); err != nil {
		t.Fatalf("claim payload didn't parse as JSON: %v", err)
	}
	if claim.DaemonID != "daemon-a" {
		t.Errorf("claim.DaemonID = %q, want %q", claim.DaemonID, "daemon-a")
	}
}

func TestClaimSandbox_ExistingClaimSameDaemon_Succeeds(t *testing.T) {
	existing := claimPayload{DaemonID: "daemon-a", ClaimedAt: time.Now().Add(-time.Hour)}
	payload, _ := json.Marshal(existing)

	fake := &fakeMQTTClient{}
	c := newTestClient(fake, &fakeClaimLookup{exists: true, payload: payload}, "daemon-a")

	if err := c.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("re-claiming your own name should succeed, got error: %v", err)
	}
	if fake.publishCount() != 1 {
		t.Errorf("expected exactly one publish (the refresh), got %d", fake.publishCount())
	}
}

func TestClaimSandbox_ExistingClaimDifferentDaemonOnline_Rejected(t *testing.T) {
	claimedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	existing := claimPayload{DaemonID: "daemon-b", ClaimedAt: claimedAt}
	payload, _ := json.Marshal(existing)

	fake := &fakeMQTTClient{}
	lookup := &fakeClaimLookup{
		byTopic: map[string]fakeLookupResult{
			"muro/_claims/default/claude-1": {payload: payload, exists: true},
			"muro/_daemons/daemon-b/alive":  {payload: []byte("online"), exists: true},
		},
	}
	c := newTestClient(fake, lookup, "daemon-a")

	err := c.ClaimSandbox("default", "claude-1")
	if err == nil {
		t.Fatal("expected an error claiming a name held by a different, currently-online daemon, got nil")
	}
	if !strings.Contains(err.Error(), "daemon-b") {
		t.Errorf("error %q should name the claiming daemon", err.Error())
	}
	if fake.publishCount() != 0 {
		t.Errorf("expected no publish on a rejected claim, got %d", fake.publishCount())
	}
}

// TestClaimSandbox_ExistingClaimDifferentDaemonOffline_StaleAllowed covers
// the DESIGN.md §13 recovery path: a crashed daemon's claim must not
// permanently block the name. daemon-b's claim is retained, but daemon-b's
// presence topic reads "offline" (its LWT fired) — the claim must be
// treated as stale and daemon-a's claim allowed through. This exercises
// ClaimSandbox.daemonIsOnline's correlation logic, which a previous
// version of ClaimSandbox didn't implement at all (any claim from a
// different daemon was rejected unconditionally, with no way for a
// crashed daemon's name to ever become claimable again short of manual
// intervention).
func TestClaimSandbox_ExistingClaimDifferentDaemonOffline_StaleAllowed(t *testing.T) {
	existing := claimPayload{DaemonID: "daemon-b", ClaimedAt: time.Now().Add(-time.Hour)}
	payload, _ := json.Marshal(existing)

	fake := &fakeMQTTClient{}
	lookup := &fakeClaimLookup{
		byTopic: map[string]fakeLookupResult{
			"muro/_claims/default/claude-1": {payload: payload, exists: true},
			"muro/_daemons/daemon-b/alive":  {payload: []byte("offline"), exists: true},
		},
	}
	c := newTestClient(fake, lookup, "daemon-a")

	if err := c.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("expected a stale claim from an offline daemon to be claimable, got error: %v", err)
	}
	p, ok := fake.lastPublishTo("muro/_claims/default/claude-1")
	if !ok {
		t.Fatal("expected a retained claim publish overwriting the stale one, got none")
	}
	var claim claimPayload
	if err := json.Unmarshal(p.payload, &claim); err != nil {
		t.Fatalf("claim payload didn't parse as JSON: %v", err)
	}
	if claim.DaemonID != "daemon-a" {
		t.Errorf("claim.DaemonID = %q, want %q", claim.DaemonID, "daemon-a")
	}
}

// TestClaimSandbox_ExistingClaimDifferentDaemonNeverSeen_StaleAllowed
// covers the "never seen" case distinctly from "seen and offline" — a
// daemon whose presence topic has no retained message at all (e.g. it
// crashed before ever completing Connect's online publish) must be
// treated as stale too, not as an error or a false "still online".
func TestClaimSandbox_ExistingClaimDifferentDaemonNeverSeen_StaleAllowed(t *testing.T) {
	existing := claimPayload{DaemonID: "daemon-b", ClaimedAt: time.Now().Add(-time.Hour)}
	payload, _ := json.Marshal(existing)

	fake := &fakeMQTTClient{}
	lookup := &fakeClaimLookup{
		byTopic: map[string]fakeLookupResult{
			"muro/_claims/default/claude-1": {payload: payload, exists: true},
			"muro/_daemons/daemon-b/alive":  {exists: false},
		},
	}
	c := newTestClient(fake, lookup, "daemon-a")

	if err := c.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("expected a claim from a never-seen daemon to be treated as stale, got error: %v", err)
	}
}

func TestClaimSandbox_LookupError_PropagatesAndDoesNotPublish(t *testing.T) {
	fake := &fakeMQTTClient{}
	c := newTestClient(fake, &fakeClaimLookup{err: errBrokerUnreachable}, "daemon-a")

	if err := c.ClaimSandbox("default", "claude-1"); err == nil {
		t.Fatal("expected lookup error to propagate, got nil")
	}
	if fake.publishCount() != 0 {
		t.Errorf("expected no publish when the lookup itself failed, got %d", fake.publishCount())
	}
}

func TestReleaseClaim_PublishesEmptyRetainedPayload(t *testing.T) {
	fake := &fakeMQTTClient{}
	c := newTestClient(fake, &fakeClaimLookup{}, "daemon-a")

	if err := c.ReleaseClaim("default", "claude-1"); err != nil {
		t.Fatalf("ReleaseClaim error: %v", err)
	}

	p, ok := fake.lastPublishTo("muro/_claims/default/claude-1")
	if !ok {
		t.Fatal("expected a publish clearing the claim topic, got none")
	}
	if !p.retained {
		t.Error("clearing publish must be retained so it actually replaces the old retained message")
	}
	if len(p.payload) != 0 {
		t.Errorf("expected an empty payload to clear the claim, got %q", p.payload)
	}
}

var errBrokerUnreachable = &staticErr{"broker unreachable"}

type staticErr struct{ msg string }

func (e *staticErr) Error() string { return e.msg }
