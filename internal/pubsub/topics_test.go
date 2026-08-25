package pubsub

import "testing"

func TestTopicBuilders(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"status", StatusTopic("muro", "default", "claude-1"), "muro/default/claude-1/status"},
		{"net-denied", NetDeniedTopic("muro", "default", "claude-1"), "muro/default/claude-1/net-denied"},
		{"inbox", InboxTopic("muro", "default", "claude-1"), "muro/default/claude-1/inbox"},
		{"broadcast", BroadcastTopic("muro", "default", "handoff"), "muro/default/broadcast/handoff"},
		{"claim", ClaimTopic("muro", "default", "claude-1"), "muro/_claims/default/claude-1"},
		{"custom root", StatusTopic("muro-home", "work", "gemini-2"), "muro-home/work/gemini-2/status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}
}

func TestClaimTopicNeverCollidesWithNamespace(t *testing.T) {
	// A real namespace can never be named "_claims" in practice (profile/
	// namespace naming isn't validated by this package), but the point of
	// rooting claims under a distinct "_claims" segment is that a claim
	// topic's shape (root/_claims/ns/name) is structurally different from
	// every per-agent topic (root/ns/name/...), so this documents that
	// invariant rather than actually needing enforcement here.
	claim := ClaimTopic("muro", "default", "x")
	status := StatusTopic("muro", "default", "x")
	if claim == status {
		t.Fatalf("claim and status topics collided: %q", claim)
	}
}
