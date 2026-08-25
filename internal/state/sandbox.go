package state

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/thomkin/muro/internal/config"
)

// SandboxState is the lifecycle state of a sandbox.
type SandboxState string

const (
	StateRunning          SandboxState = "running"
	StateStopped          SandboxState = "stopped"
	StateReloadPending    SandboxState = "reload-pending"
	StateCrashed          SandboxState = "crashed"
	StateRestarting       SandboxState = "restarting"
	StateRestartExhausted SandboxState = "restart-exhausted"
)

// Sandbox is the daemon's live record of one running (or previously running)
// sandbox. ID is an internal storage key only — CLI-facing addressing uses
// Namespace/Name (DESIGN.md §9).
type Sandbox struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Namespace     string         `json:"namespace"`
	Profile       string         `json:"profile"`
	Agent         string         `json:"agent"`
	PID           int            `json:"pid"`
	State         SandboxState   `json:"state"`
	StartedAt     time.Time      `json:"started_at"`
	Mounts        []config.Mount `json:"mounts"`
	Tools         []config.Tool  `json:"tools"`
	AllowURLs     []string       `json:"allow_urls"`
	RestartPolicy string         `json:"restart_policy"`
	RestartCount  int            `json:"restart_count"`
}

// NewID generates a unique internal sandbox id, e.g. "sb_8f2a1c9d".
func NewID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate sandbox id: %w", err)
	}
	return "sb_" + hex.EncodeToString(buf), nil
}

// key is the Store's internal map key for a sandbox: "namespace/name".
func key(namespace, name string) string {
	return namespace + "/" + name
}
