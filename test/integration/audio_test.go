//go:build integration

package integration

import (
	"fmt"
	"os"
	"testing"

	"github.com/thomkin/muro/internal/sandbox"
)

// TestAudioPassthrough_SocketsReachableInsideSandbox proves the
// audio-passthrough bind mounts (internal/sandbox/audio.go's AudioMounts)
// are genuinely reachable from inside a real bwrap sandbox, at the
// identical path as the host — not just that AudioMounts computes the
// right Go values (audio_test.go's unit tests already cover that against
// fake sockets). This only proves the bind mount is a real, connectable
// socket from inside the sandbox; it does not prove actual audio I/O,
// which needs real hardware and a real speech-to-text tool, out of scope
// for this codebase.
func TestAudioPassthrough_SocketsReachableInsideSandbox(t *testing.T) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		t.Skip("XDG_RUNTIME_DIR not set on this host, skipping")
	}

	audioMounts, err := sandbox.AudioMounts(runtimeDir)
	if err != nil {
		t.Skipf("no real PipeWire/PulseAudio socket available on this host, skipping: %v", err)
	}

	iso := newIsolator(t)

	// Assert every socket AudioMounts found is reachable, by exact host
	// path, from inside the sandbox — proving both the mount and the
	// identical-path assumption the whole feature depends on.
	var checks string
	for _, m := range audioMounts {
		checks += fmt.Sprintf("test -S %q && ", m.SandboxPath)
	}
	checks += "true"

	mounts := append(shellMounts(), audioMounts...)
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts:          mounts,
		AudioRuntimeDir: runtimeDir,
		Cmd:             []string{"/bin/sh", "-c", checks},
	})
	if code != 0 {
		t.Errorf("expected every audio socket to be reachable as a real socket file inside the sandbox, got exit %d", code)
	}
}

// TestAudioPassthrough_DenyByDefaultWithoutOptIn confirms the same paths
// are NOT visible inside a sandbox that never opted into audio passthrough
// — audio access must be opt-in, not incidentally available.
func TestAudioPassthrough_DenyByDefaultWithoutOptIn(t *testing.T) {
	runtimeDir := os.Getenv("XDG_RUNTIME_DIR")
	if runtimeDir == "" {
		t.Skip("XDG_RUNTIME_DIR not set on this host, skipping")
	}

	audioMounts, err := sandbox.AudioMounts(runtimeDir)
	if err != nil {
		t.Skipf("no real PipeWire/PulseAudio socket available on this host, skipping: %v", err)
	}

	iso := newIsolator(t)

	// Deliberately do NOT include audioMounts or AudioRuntimeDir.
	code := runAndWait(t, iso, sandbox.LaunchSpec{
		Mounts: shellMounts(),
		Cmd:    []string{"/bin/sh", "-c", fmt.Sprintf("test -e %q", audioMounts[0].SandboxPath)},
	})
	if code == 0 {
		t.Errorf("expected the audio socket to be invisible without opting in, but it was visible")
	}
}
