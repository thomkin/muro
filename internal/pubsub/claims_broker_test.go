//go:build integration

package pubsub

import (
	"net"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	mqttlib "github.com/eclipse/paho.mqtt.golang"
	mqttserver "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/hooks/auth"
	"github.com/mochi-mqtt/server/v2/listeners"

	"github.com/thomkin/muro/internal/config"
)

// startTestBroker starts a real, in-process mochi-mqtt broker (the same
// library cmd/muro-broker wraps) on a free localhost port, for tests that
// need genuine MQTT retained-message/LWT semantics a fake claimLookup
// can't provide. Returns the broker's address; registers cleanup via t.
func startTestBroker(t *testing.T) string {
	t.Helper()

	// Reserve a free port the standard way, then hand that exact address to
	// mochi rather than relying on any library-specific "what port did you
	// actually bind" accessor.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a free port: %v", err)
	}
	addr := probe.Addr().String()
	probe.Close()

	server := mqttserver.New(nil)
	if err := server.AddHook(new(auth.AllowHook), nil); err != nil {
		t.Fatalf("add auth hook: %v", err)
	}
	tcp := listeners.NewTCP(listeners.Config{ID: "test-tcp", Address: addr})
	if err := server.AddListener(tcp); err != nil {
		t.Fatalf("add TCP listener on %s: %v", addr, err)
	}
	if err := server.Serve(); err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	return addr
}

// capturingDialer wraps paho's connection establishment so a test can grab
// the raw net.Conn a Client is using and close it directly, bypassing
// Client.Disconnect entirely — the only way to make that client's MQTT
// Last Will actually fire, since a clean MQTT DISCONNECT (what
// Client.Disconnect always sends) explicitly suppresses the will by
// protocol design. This is what makes daemon-crash simulation genuine
// rather than a same-package fake standing in for it.
type capturingDialer struct {
	mu   sync.Mutex
	conn net.Conn
}

func (d *capturingDialer) open(uri *url.URL, _ mqttlib.ClientOptions) (net.Conn, error) {
	c, err := net.Dial("tcp", uri.Host)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conn = c
	d.mu.Unlock()
	return c, nil
}

func (d *capturingDialer) killUncleanly() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn != nil {
		_ = d.conn.Close()
	}
}

// connectRealClient constructs and connects a real, broker-backed Client
// (not the fakeMQTTClient/fakeClaimLookup unit-test doubles elsewhere in
// this package) for daemonID against a real broker at addr. Returns the
// Client and the dialer that can force an unclean disconnect for it.
func connectRealClient(t *testing.T, addr, daemonID string) (*Client, *capturingDialer) {
	t.Helper()

	dialer := &capturingDialer{}
	opts := buildClientOptions(config.BrokerConfig{Address: "tcp://" + addr}, "muro-test", daemonID)
	opts.SetCustomOpenConnectionFn(dialer.open)
	opts.SetAutoReconnect(false) // a killed connection must stay down for the test to observe staleness
	mc := mqttlib.NewClient(opts)

	c := &Client{
		mqttClient: mc,
		topicRoot:  "muro-test",
		daemonID:   daemonID,
	}
	c.lookup = &mqttClaimLookup{client: mc}

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect(%s): %v", daemonID, err)
	}
	t.Cleanup(func() {
		if mc.IsConnected() {
			mc.Disconnect(250)
		}
	})

	return c, dialer
}

// waitUntil polls cond every 100ms up to timeout, failing the test if it
// never becomes true — used for the LWT-delivery window, which is real
// network/broker latency, not instantaneous.
func waitUntil(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("timed out after %s waiting for: %s", timeout, msg)
	}
}

// TestRealBroker_CrossDaemonClaim_OnlineRejectedThenStaleRecovered is the
// real, end-to-end proof of DESIGN.md §13's cross-daemon collision
// protection — internal/pubsub's own unit tests exercise ClaimSandbox's
// decision logic against a fake claimLookup, which by construction cannot
// validate that real MQTT retained messages and a real Last Will actually
// behave the way that logic assumes. This test uses two real Clients
// (simulating two independent murod processes, different daemon IDs) against
// one real broker.
func TestRealBroker_CrossDaemonClaim_OnlineRejectedThenStaleRecovered(t *testing.T) {
	addr := startTestBroker(t)

	daemonA, dialerA := connectRealClient(t, addr, "daemon-a")
	daemonB, _ := connectRealClient(t, addr, "daemon-b")

	const namespace, name = "default", "claude-1"

	// 1. daemon-a claims cleanly.
	if err := daemonA.ClaimSandbox(namespace, name); err != nil {
		t.Fatalf("daemon-a ClaimSandbox: %v", err)
	}

	// 2. daemon-b tries the same name while daemon-a is still genuinely
	// online (real presence topic, real retained claim) -- must be
	// rejected.
	err := daemonB.ClaimSandbox(namespace, name)
	if err == nil {
		t.Fatal("daemon-b claimed a name daemon-a genuinely still holds and is online for")
	}
	t.Logf("daemon-b correctly rejected: %v", err)

	// 3. Simulate daemon-a crashing: close its raw TCP connection directly,
	// bypassing Client.Disconnect (which sends a clean MQTT DISCONNECT and
	// would explicitly suppress the Will). The broker must observe this as
	// an abrupt loss and fire daemon-a's LWT, publishing "offline" to its
	// presence topic.
	dialerA.killUncleanly()

	// 4. Once that propagates, daemon-a's claim is stale and daemon-b's
	// claim for the same name must now succeed. This is the real proof:
	// it only passes if the broker actually delivered daemon-a's LWT
	// ("offline" on its presence topic) and daemon-b's daemonIsOnline
	// check actually saw it -- no fake stands in for either half here.
	waitUntil(t, 10*time.Second, "daemon-b successfully claims the name after daemon-a's LWT fires", func() bool {
		return daemonB.ClaimSandbox(namespace, name) == nil
	})

	// 5. Confirm the claim really did transfer to daemon-b, not just that
	// SOME claim attempt eventually stopped erroring: read the retained
	// claim back directly and check its DaemonID. (Not checking
	// daemonA.mqttClient.IsConnected() here deliberately -- paho only
	// updates that flag once it notices the connection is gone via a
	// failed read/write, which is a separate, unbounded timing window from
	// the broker-side LWT delivery this test actually cares about, and
	// asserting on it made this test flaky without strengthening it.)
	payload, exists, err := daemonB.lookup.currentRetained(ClaimTopic(daemonB.topicRoot, namespace, name))
	if err != nil {
		t.Fatalf("read back final claim: %v", err)
	}
	if !exists {
		t.Fatal("expected a retained claim after daemon-b's successful claim, found none")
	}
	if !strings.Contains(string(payload), "daemon-b") {
		t.Errorf("final claim payload should name daemon-b, got: %s", payload)
	}
}

// TestRealBroker_SameDaemonReclaim_Succeeds is the one case from the
// fake-backed unit tests (TestClaimSandbox_ExistingClaimSameDaemon_Succeeds)
// worth re-confirming against a real broker too, cheaply, since it's
// exercised as a side effect of the test above's setup pattern: a daemon
// re-claiming its own already-held name (e.g. across a reload) must
// succeed, not be treated as a collision with itself.
func TestRealBroker_SameDaemonReclaim_Succeeds(t *testing.T) {
	addr := startTestBroker(t)
	daemonA, _ := connectRealClient(t, addr, "daemon-a")

	if err := daemonA.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := daemonA.ClaimSandbox("default", "claude-1"); err != nil {
		t.Fatalf("re-claiming your own held name should succeed, got: %v", err)
	}
}
