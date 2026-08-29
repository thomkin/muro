package sandbox

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/thomkin/muro/internal/config"
)

// AudioMounts resolves the host's PipeWire/PulseAudio sockets under
// runtimeDir (the host's $XDG_RUNTIME_DIR) into the bind mounts a sandbox
// needs to reach them — at the IDENTICAL sandbox-side path as the host, not
// a translated one, since both client libraries default to
// $XDG_RUNTIME_DIR/pipewire-0 and $XDG_RUNTIME_DIR/pulse/native
// respectively; matching paths means the caller only needs to also set
// XDG_RUNTIME_DIR to the same value inside the sandbox (buildLaunchSpec)
// and nothing else. Raw ALSA device nodes (/dev/snd/*) are deliberately not
// used here — they're typically already claimed exclusively by the host's
// running sound server and would just conflict with it; bind-mounting the
// server's own socket is what desktop container runtimes (Flatpak, etc.)
// actually do. Peer-credential auth on these sockets resolves to the real
// host UID from the server's point of view regardless of the sandboxed
// process's own user-namespace-internal identity, the same mechanism that
// already makes AgentSocketPath/ToolSocketPath work correctly from inside a
// bwrap sandbox, so no special credential handling is needed here either.
//
// Unlike pub/sub (where a nil/unreachable broker is tolerated — the bridge
// is a nice-to-have with a clear error path if actually used later), a
// profile that opts into audio: true and gets nothing back is a launch
// failure, not silent degradation: audio access is the entire reason that
// profile field was set, so finding out at launch is far better than an STT
// tool silently getting nothing mid-session.
func AudioMounts(runtimeDir string) ([]config.Mount, error) {
	if runtimeDir == "" {
		return nil, fmt.Errorf("XDG_RUNTIME_DIR not set on the host — cannot resolve audio sockets")
	}

	candidates := []string{
		filepath.Join(runtimeDir, "pipewire-0"),
		filepath.Join(runtimeDir, "pulse", "native"),
	}

	var mounts []config.Mount
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSocket == 0 {
			// A stale regular file (or directory) at this path isn't a
			// working audio socket — skip it rather than bind-mounting
			// something that would just fail to connect from inside the
			// sandbox.
			continue
		}
		mounts = append(mounts, config.Mount{Host: path, SandboxPath: path, Mode: "rw"})
	}

	if len(mounts) == 0 {
		return nil, fmt.Errorf("no PipeWire or PulseAudio socket found under %s — is a sound server running on the host?", runtimeDir)
	}
	return mounts, nil
}
