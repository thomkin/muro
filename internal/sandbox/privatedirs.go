package sandbox

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thomkin/muro/internal/config"
)

// privateDirsBase returns the host directory under which every one of
// sandboxID's private directories live — also what a `muro sandbox delete`
// removes wholesale, so no individual private-dir bookkeeping is needed
// beyond state.Sandbox.PrivateDirs recording which sandbox paths exist
// under it.
func privateDirsBase(stateDir, sandboxID string) string {
	return filepath.Join(stateDir, "sandboxes", sandboxID, "private")
}

// PrivateDirMounts creates (if not already present) and returns bind mounts
// for each of sandboxPaths, each backed by a fresh directory under
// privateDirsBase(stateDir, sandboxID) — isolated per sandbox instance,
// never shared with any real host location at that path or with any other
// sandbox. Idempotent by construction: restarting the same sandbox (same
// sandboxID) resolves to the identical host directories, so whatever an
// agent wrote there persists across restarts; a fresh `muro run` (a new
// sandboxID) always starts with empty directories.
func PrivateDirMounts(stateDir, sandboxID string, sandboxPaths []string) ([]config.Mount, error) {
	var mounts []config.Mount
	for _, sp := range sandboxPaths {
		hostDir := filepath.Join(privateDirsBase(stateDir, sandboxID), strings.TrimPrefix(sp, "/"))
		if err := os.MkdirAll(hostDir, 0o700); err != nil {
			return nil, fmt.Errorf("create private dir for %q: %w", sp, err)
		}
		mounts = append(mounts, config.Mount{Host: hostDir, SandboxPath: sp, Mode: "rw"})
	}
	return mounts, nil
}

// RemovePrivateDirs deletes every private directory ever created for
// sandboxID (privateDirsBase's whole subtree) — called by `muro sandbox
// delete`. A missing directory (nothing was ever private for this sandbox)
// is not an error.
func RemovePrivateDirs(stateDir, sandboxID string) error {
	return os.RemoveAll(privateDirsBase(stateDir, sandboxID))
}
