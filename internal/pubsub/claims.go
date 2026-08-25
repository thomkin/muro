package pubsub

import (
	"encoding/json"
	"fmt"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// claimPayload is the retained-message shape published to a ClaimTopic.
type claimPayload struct {
	DaemonID  string    `json:"daemon_id"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// claimLookup fetches the currently retained payload at a topic, if any.
// It exists so ClaimSandbox's allow/reject decision logic can be tested
// without a real broker round-trip — Client's real implementation
// (mqttClaimLookup) does an actual subscribe/wait/unsubscribe against the
// broker; tests substitute a fake.
type claimLookup interface {
	currentRetained(topic string) (payload []byte, exists bool, err error)
}

// mqttClaimLookup is claimLookup's real implementation. MQTT has no
// direct "read the current retained value" API, so this uses the
// standard trick: subscribing to a topic delivers its retained message
// (if any) immediately via the subscription callback, so briefly
// subscribing and waiting is equivalent to a read.
type mqttClaimLookup struct {
	client mqtt.Client
}

func (m *mqttClaimLookup) currentRetained(topic string) ([]byte, bool, error) {
	result := make(chan []byte, 1)
	token := m.client.Subscribe(topic, 1, func(_ mqtt.Client, msg mqtt.Message) {
		select {
		case result <- msg.Payload():
		default:
		}
	})
	if !token.WaitTimeout(5 * time.Second) {
		return nil, false, fmt.Errorf("pubsub: claim lookup subscribe timed out")
	}
	if err := token.Error(); err != nil {
		return nil, false, err
	}
	defer m.client.Unsubscribe(topic)

	select {
	case payload := <-result:
		if len(payload) == 0 {
			// ReleaseClaim publishes an empty retained payload to clear a
			// claim; an empty payload means "no live claim", same as no
			// retained message at all.
			return nil, false, nil
		}
		return payload, true, nil
	case <-time.After(500 * time.Millisecond):
		// No retained message arrived in the window a broker would have
		// delivered one immediately on subscribe, so there isn't one.
		return nil, false, nil
	}
}

// ClaimSandbox registers this daemon as the owner of namespace/name via a
// retained MQTT message at ClaimTopic(root, namespace, name)
// (DESIGN.md §13). If a live claim from a *different* daemon already
// exists there, the claim is rejected and nothing is published — callers
// must not launch the sandbox in that case. Re-claiming a name this same
// daemon already holds (e.g. across a reload/restart) succeeds and
// refreshes the timestamp.
//
// Automatically detecting that a *different* daemon's claim is stale
// because that daemon crashed (rather than genuinely still owning the
// name) requires correlating the claim against that daemon's presence
// topic (see client.go's presenceTopic doc comment for why a single
// per-claim LWT isn't mechanically possible) — that correlation is not
// implemented here.
// TODO(integration): exercise real stale-claim correlation against a
// live muro-broker once one exists (IMPLEMENTATION.md Phase 11); for now
// a stale claim from a crashed daemon must be cleared via ReleaseClaim by
// an operator, or will simply expire once that daemon's own process
// eventually calls ReleaseClaim/ClaimSandbox again.
func (c *Client) ClaimSandbox(namespace, name string) error {
	topic := ClaimTopic(c.topicRoot, namespace, name)

	payload, exists, err := c.lookup.currentRetained(topic)
	if err != nil {
		return fmt.Errorf("pubsub: checking existing claim for %s/%s: %w", namespace, name, err)
	}
	if exists {
		var existing claimPayload
		if err := json.Unmarshal(payload, &existing); err != nil {
			return fmt.Errorf("pubsub: parsing existing claim for %s/%s: %w", namespace, name, err)
		}
		if existing.DaemonID != c.daemonID {
			return fmt.Errorf("namespace/name already claimed by daemon %s since %s",
				existing.DaemonID, existing.ClaimedAt.Format(time.RFC3339))
		}
		// Same daemon re-claiming its own name — allowed, refresh below.
	}

	claim := claimPayload{DaemonID: c.daemonID, ClaimedAt: time.Now()}
	data, err := json.Marshal(claim)
	if err != nil {
		return fmt.Errorf("pubsub: marshal claim for %s/%s: %w", namespace, name, err)
	}
	token := c.mqttClient.Publish(topic, 1, true, data)
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: publish claim for %s/%s timed out", namespace, name)
	}
	return token.Error()
}

// ReleaseClaim clears this daemon's claim on namespace/name by publishing
// an empty retained payload to its ClaimTopic (DESIGN.md §13) — callers
// should call this on a clean sandbox stop.
func (c *Client) ReleaseClaim(namespace, name string) error {
	topic := ClaimTopic(c.topicRoot, namespace, name)
	token := c.mqttClient.Publish(topic, 1, true, []byte{})
	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("pubsub: release claim for %s/%s timed out", namespace, name)
	}
	return token.Error()
}
