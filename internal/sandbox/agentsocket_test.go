package sandbox

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakePubsubPublisher is an in-memory PubsubPublisher recording every call, so
// tests can assert exactly what agentSocketServer.publish decided to
// forward (or, for the scoping-rejection cases, that it never called this
// at all).
type fakePubsubPublisher struct {
	mu         sync.Mutex
	inbox      []string // "namespace/name: message"
	broadcasts []string // "namespace/topic: message"
	failInbox  error
}

func (f *fakePubsubPublisher) PublishInbox(namespace, name, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInbox != nil {
		return f.failInbox
	}
	f.inbox = append(f.inbox, namespace+"/"+name+": "+message)
	return nil
}

func (f *fakePubsubPublisher) PublishBroadcast(namespace, topic, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcasts = append(f.broadcasts, namespace+"/"+topic+": "+message)
	return nil
}

func (f *fakePubsubPublisher) calls() (inbox, broadcasts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.inbox...), append([]string(nil), f.broadcasts...)
}

func TestAgentSocketServer_Publish_SameNamespaceInboxAllowed(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	resp := s.publish(AgentPublishRequest{To: "teamA/bob", Message: "hi bob"})
	if !resp.OK || resp.Error != "" {
		t.Fatalf("expected OK, got %+v", resp)
	}
	inbox, _ := pub.calls()
	if len(inbox) != 1 || inbox[0] != "teamA/bob: hi bob" {
		t.Fatalf("unexpected inbox calls: %v", inbox)
	}
}

func TestAgentSocketServer_Publish_BareNameDefaultsToOwnNamespace(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	resp := s.publish(AgentPublishRequest{To: "bob", Message: "hi"})
	if !resp.OK {
		t.Fatalf("expected OK, got %+v", resp)
	}
	inbox, _ := pub.calls()
	if len(inbox) != 1 || inbox[0] != "teamA/bob: hi" {
		t.Fatalf("bare name should default to own namespace, got %v", inbox)
	}
}

// TestAgentSocketServer_Publish_CrossNamespaceRejected is the directive's
// core requirement: a sandbox may only inbox-message another agent within
// its own namespace (SPEC.md §8's v1 scoping default). This must be
// rejected BEFORE ever reaching the publisher.
func TestAgentSocketServer_Publish_CrossNamespaceRejected(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	resp := s.publish(AgentPublishRequest{To: "teamB/bob", Message: "hi"})
	if resp.OK {
		t.Fatalf("expected cross-namespace publish to be rejected, got OK")
	}
	if resp.Error == "" {
		t.Fatalf("expected a non-empty error message")
	}
	inbox, _ := pub.calls()
	if len(inbox) != 0 {
		t.Fatalf("publisher must never be called for a rejected cross-namespace request, got %v", inbox)
	}
}

func TestAgentSocketServer_Publish_BroadcastAlwaysOwnNamespace(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	resp := s.publish(AgentPublishRequest{Broadcast: "standup", Message: "starting now"})
	if !resp.OK {
		t.Fatalf("expected OK, got %+v", resp)
	}
	_, broadcasts := pub.calls()
	if len(broadcasts) != 1 || broadcasts[0] != "teamA/standup: starting now" {
		t.Fatalf("unexpected broadcast calls: %v", broadcasts)
	}
}

func TestAgentSocketServer_Publish_RejectsBothOrNeitherTarget(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	if resp := s.publish(AgentPublishRequest{Message: "hi"}); resp.OK {
		t.Fatalf("expected rejection when neither to/broadcast is set")
	}
	if resp := s.publish(AgentPublishRequest{To: "teamA/bob", Broadcast: "x", Message: "hi"}); resp.OK {
		t.Fatalf("expected rejection when both to/broadcast are set")
	}
	inbox, broadcasts := pub.calls()
	if len(inbox) != 0 || len(broadcasts) != 0 {
		t.Fatalf("publisher must never be called for a malformed request, got inbox=%v broadcasts=%v", inbox, broadcasts)
	}
}

func TestAgentSocketServer_Publish_NilPublisherClearError(t *testing.T) {
	s := &agentSocketServer{namespace: "teamA", publisher: nil}

	resp := s.publish(AgentPublishRequest{To: "teamA/bob", Message: "hi"})
	if resp.OK {
		t.Fatalf("expected rejection with a nil publisher")
	}
	if resp.Error != "pub/sub broker not connected" {
		t.Fatalf("unexpected error message: %q", resp.Error)
	}
}

func TestAgentSocketServer_Publish_InvalidNameRejected(t *testing.T) {
	pub := &fakePubsubPublisher{}
	s := &agentSocketServer{namespace: "teamA", publisher: pub}

	resp := s.publish(AgentPublishRequest{To: "teamA/../etc", Message: "hi"})
	if resp.OK {
		t.Fatalf("expected an invalid sandbox name to be rejected")
	}
	inbox, _ := pub.calls()
	if len(inbox) != 0 {
		t.Fatalf("publisher must never be called for an invalid name, got %v", inbox)
	}
}

// TestAgentSocketServer_EndToEnd exercises the real Unix socket + newline-
// JSON wire protocol (startAgentSocket, handleConn) end to end, matching
// exactly what `muro pubsub publish` (internal/cli/pubsub.go) does over the
// wire: dial, write one JSON line, read one JSON line back.
func TestAgentSocketServer_EndToEnd(t *testing.T) {
	pub := &fakePubsubPublisher{}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")

	srv, err := startAgentSocket(path, "teamA", pub)
	if err != nil {
		t.Fatalf("startAgentSocket: %v", err)
	}
	defer srv.stop()

	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("expected socket at %s with mode 0600, got err=%v mode=%v", path, err, fi)
	}

	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := AgentPublishRequest{To: "teamA/bob", Message: "hello over the wire"}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp AgentPublishResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if !resp.OK {
		t.Fatalf("expected OK response, got %+v", resp)
	}

	inbox, _ := pub.calls()
	if len(inbox) != 1 || inbox[0] != "teamA/bob: hello over the wire" {
		t.Fatalf("unexpected inbox calls: %v", inbox)
	}
}

// TestAgentSocketServer_EndToEnd_CrossNamespaceOverWire is the same
// end-to-end wire path as above, but confirms the scoping rejection
// survives the real socket round trip too, not just a direct in-process
// call to publish().
func TestAgentSocketServer_EndToEnd_CrossNamespaceOverWire(t *testing.T) {
	pub := &fakePubsubPublisher{}
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.sock")

	srv, err := startAgentSocket(path, "teamA", pub)
	if err != nil {
		t.Fatalf("startAgentSocket: %v", err)
	}
	defer srv.stop()

	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	req := AgentPublishRequest{To: "teamB/eve", Message: "leak attempt"}
	data, _ := json.Marshal(req)
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}

	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp AgentPublishResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.OK {
		t.Fatalf("expected cross-namespace publish over the wire to be rejected")
	}
	inbox, _ := pub.calls()
	if len(inbox) != 0 {
		t.Fatalf("publisher must never be called, got %v", inbox)
	}
}

func TestSplitToTarget(t *testing.T) {
	cases := []struct {
		in       string
		wantNS   string
		wantName string
	}{
		{"teamA/bob", "teamA", "bob"},
		{"bob", "", "bob"},
		{"", "", ""},
	}
	for _, c := range cases {
		ns, name := splitToTarget(c.in)
		if ns != c.wantNS || name != c.wantName {
			t.Errorf("splitToTarget(%q) = (%q, %q), want (%q, %q)", c.in, ns, name, c.wantNS, c.wantName)
		}
	}
}
