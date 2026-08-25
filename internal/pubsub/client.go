package pubsub

import (
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/thomkin/muro/internal/config"
)

// presenceTopic is a single per-daemon liveness topic. MQTT's Last Will
// and Testament mechanism supports exactly one will message per
// connection, so a daemon running multiple sandboxes cannot register a
// separate LWT per claim topic (DESIGN.md §13 describes clearing "all of
// its own claim topics" via LWT, which isn't mechanically possible for
// more than one topic on a single connection). Instead this client uses
// the standard MQTT presence idiom: one retained "online"/"offline"
// topic per daemon, with the LWT publishing "offline" to it on an
// unclean disconnect. Correlating a stale claim (DESIGN.md §13's "a
// crashed daemon's claims don't permanently block the name") against
// this presence topic is left for the caller/muro-broker-integration
// layer — see the doc comment on ClaimSandbox.
func presenceTopic(root, daemonID string) string {
	return fmt.Sprintf("%s/_daemons/%s/alive", root, daemonID)
}

// Client wraps a paho MQTT connection with muro's topic-root scoping and
// the DESIGN.md §13 claim mechanism. The underlying paho client is held
// as the mqtt.Client interface (not the concrete implementation) so
// Client can be exercised in tests against a fake.
type Client struct {
	mqttClient mqtt.Client
	topicRoot  string
	daemonID   string
	lookup     claimLookup
}

// buildClientOptions constructs paho's ClientOptions from a BrokerConfig,
// topic root, and daemon id, including the presence-topic LWT. Split out
// from NewClient so the option-building logic (broker address, auth,
// will topic/payload) can be unit-tested without a live connection.
func buildClientOptions(cfg config.BrokerConfig, topicRoot, daemonID string) *mqtt.ClientOptions {
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.Address)
	opts.SetClientID("murod-" + daemonID)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
	}
	if cfg.Password != "" {
		opts.SetPassword(cfg.Password)
	}
	opts.SetCleanSession(true)
	opts.SetBinaryWill(presenceTopic(topicRoot, daemonID), []byte("offline"), 1, true)
	return opts
}

// NewClient constructs a Client for the given broker configuration. It
// does not connect — call Connect to actually establish the connection.
func NewClient(cfg config.BrokerConfig, topicRoot, daemonID string) (*Client, error) {
	if topicRoot == "" {
		return nil, fmt.Errorf("pubsub: topicRoot must not be empty")
	}
	if daemonID == "" {
		return nil, fmt.Errorf("pubsub: daemonID must not be empty")
	}
	opts := buildClientOptions(cfg, topicRoot, daemonID)
	mc := mqtt.NewClient(opts)
	c := &Client{
		mqttClient: mc,
		topicRoot:  topicRoot,
		daemonID:   daemonID,
	}
	c.lookup = &mqttClaimLookup{client: mc}
	return c, nil
}

// Connect establishes the broker connection (with the presence-topic LWT
// already registered by NewClient, DESIGN.md §13) and then publishes this
// daemon's own "online" presence so other daemons can tell it's up.
func (c *Client) Connect() error {
	token := c.mqttClient.Connect()
	if !token.WaitTimeout(10 * time.Second) {
		return fmt.Errorf("pubsub: connect timed out")
	}
	if err := token.Error(); err != nil {
		return fmt.Errorf("pubsub: connect: %w", err)
	}

	onlineToken := c.mqttClient.Publish(presenceTopic(c.topicRoot, c.daemonID), 1, true, []byte("online"))
	if !onlineToken.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: publishing presence timed out")
	}
	return onlineToken.Error()
}

// Disconnect cleanly closes the connection. Because this is a clean
// disconnect, the LWT does NOT fire — callers that want the presence
// topic to flip to "offline" on purpose (e.g. `muro daemon stop`) should
// publish that themselves before calling Disconnect, rather than relying
// on the LWT, which is only for unclean/crash disconnects.
func (c *Client) Disconnect() {
	c.mqttClient.Disconnect(250)
}

// PublishStatus publishes a lifecycle event for a sandbox to its status
// topic (DESIGN.md §8/§13's event set: started, stopped, reload-pending,
// restarted, crashed, restarting, restart-exhausted).
func (c *Client) PublishStatus(namespace, name, event string) error {
	topic := StatusTopic(c.topicRoot, namespace, name)
	token := c.mqttClient.Publish(topic, 1, false, []byte(event))
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: publish status timed out")
	}
	return token.Error()
}

// PublishDenied publishes a denied-network event for a sandbox (SPEC.md
// §8's sandbox.network.denied-equivalent net-denied topic).
func (c *Client) PublishDenied(namespace, name, host string) error {
	topic := NetDeniedTopic(c.topicRoot, namespace, name)
	token := c.mqttClient.Publish(topic, 1, false, []byte(host))
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: publish denied timed out")
	}
	return token.Error()
}

// Subscribe registers handler to be called for every message published to
// topic.
func (c *Client) Subscribe(topic string, handler func(topic string, payload []byte)) error {
	token := c.mqttClient.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		handler(msg.Topic(), msg.Payload())
	})
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: subscribe timed out")
	}
	return token.Error()
}
