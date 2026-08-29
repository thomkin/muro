package sandbox

import (
	"crypto/rand"
	"fmt"
)

// SessionIDEnvVar is the name of the env var a sandbox's stable per-instance
// session UUID (state.Sandbox.SessionID) is exposed under.
const SessionIDEnvVar = "MURO_SESSION_ID"

// SessionIDTemplateToken is the literal substring buildLaunchSpec replaces
// with the sandbox's SessionID inside each agent_args entry — a plain
// string token, not shell-style $VAR expansion, since agent_args are exec'd
// directly with no shell in between to expand anything.
const SessionIDTemplateToken = "{{SESSION_ID}}"

// NewSessionID generates a fresh RFC 4122 version-4 UUID string.
func NewSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10xx
	return fmt.Sprintf("%x-%x-%x-%x-%x", buf[0:4], buf[4:6], buf[6:8], buf[8:10], buf[10:16]), nil
}
