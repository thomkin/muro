package sandbox

import (
	"net"
	"time"
)

// PubsubSubscriber is the minimal surface Manager needs to receive inbox
// messages for a sandbox and inject them into its pty — the inbound half
// of the agent-to-agent bridge (agentsocket.go is the outbound half).
// Same local-interface decoupling as PubsubPublisher/ProxyUpdater/
// EventPublisher: this package never imports internal/pubsub.
type PubsubSubscriber interface {
	// SubscribeInbox registers handler to be called with the raw message
	// payload for every message delivered to namespace/name's inbox.
	// Subscribing twice for the same namespace/name is the caller's
	// responsibility to avoid (Manager tracks this — see
	// inboxSubscribed).
	SubscribeInbox(namespace, name string, handler func(message []byte)) error
}

// injectMessage delivers payload into the sandbox listening on
// socketPath as if a human had typed it and pressed Enter — dialing the
// shim's dedicated injection socket (shim.go's InjectSocketPath,
// cmd/muro-shim's second accept loop), which writes directly to ptmx and
// never touches the attach path's exclusivity/replay state at all. A
// carriage return ("\r", what a real terminal's Enter key actually sends,
// not "\n") is appended so the message submits as a complete line rather
// than sitting in the sandboxed program's input buffer unsent.
//
// Best-effort: a dial failure (the sandbox already stopped, or was never
// launched with a pty) is not escalated to an error the caller must
// handle — an inbox message arriving for a sandbox that's no longer
// listening has nowhere useful to go, matching this project's established
// "pub/sub delivery failures are non-fatal" philosophy throughout.
func injectMessage(socketPath string, payload []byte) {
	if socketPath == "" {
		return
	}
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write(payload)
	_, _ = conn.Write([]byte("\r"))
}
