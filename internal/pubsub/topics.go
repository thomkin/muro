// Package pubsub implements muro's MQTT pub/sub client: topic naming
// (SPEC.md §8), the daemon's broker connection wrapper, and the
// cross-daemon name-collision claim mechanism (DESIGN.md §13).
package pubsub

import "fmt"

// StatusTopic is where lifecycle events for a sandbox are published
// (started, stopped, reload-pending, restarted, crashed, restarting,
// restart-exhausted — DESIGN.md §8/§13).
func StatusTopic(root, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/status", root, namespace, name)
}

// NetDeniedTopic is where denied-network events for a sandbox are
// published (SPEC.md §8).
func NetDeniedTopic(root, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/net-denied", root, namespace, name)
}

// InboxTopic is where direct messages addressed to a specific agent are
// published (SPEC.md §8, §8.4).
func InboxTopic(root, namespace, name string) string {
	return fmt.Sprintf("%s/%s/%s/inbox", root, namespace, name)
}

// BroadcastTopic is a free-form, namespace-scoped topic agents
// publish/subscribe to for their own coordination (SPEC.md §8).
func BroadcastTopic(root, namespace, topic string) string {
	return fmt.Sprintf("%s/%s/broadcast/%s", root, namespace, topic)
}

// ClaimTopic holds the retained cross-daemon name-collision claim for a
// namespace/name pair (DESIGN.md §13). It is deliberately rooted under
// "_claims" rather than under the namespace itself, so a claim can never
// collide with a real per-agent topic (agent names can't contain "/", so
// "_claims" can never be mistaken for a namespace either).
func ClaimTopic(root, namespace, name string) string {
	return fmt.Sprintf("%s/_claims/%s/%s", root, namespace, name)
}
