package sandbox

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// listenUnix creates a real Unix domain socket at path, closing it when the
// test ends — AudioMounts requires a genuine socket (os.ModeSocket), not
// just a file at the expected name, so a plain os.Create wouldn't exercise
// the check this test is meant to cover.
func listenUnix(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen unix %s: %v", path, err)
	}
	t.Cleanup(func() { ln.Close() })
}

func TestAudioMounts_NoRuntimeDir(t *testing.T) {
	if _, err := AudioMounts(""); err == nil {
		t.Fatal("expected an error when runtimeDir is empty")
	}
}

func TestAudioMounts_NeitherSocketPresent(t *testing.T) {
	dir := t.TempDir()
	if _, err := AudioMounts(dir); err == nil {
		t.Fatal("expected an error when neither pipewire-0 nor pulse/native exists")
	}
}

func TestAudioMounts_PipewireOnly(t *testing.T) {
	dir := t.TempDir()
	listenUnix(t, filepath.Join(dir, "pipewire-0"))

	mounts, err := AudioMounts(dir)
	if err != nil {
		t.Fatalf("AudioMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 mount, got %d: %+v", len(mounts), mounts)
	}
	want := filepath.Join(dir, "pipewire-0")
	if mounts[0].Host != want || mounts[0].SandboxPath != want || mounts[0].Mode != "rw" {
		t.Fatalf("unexpected mount: %+v", mounts[0])
	}
}

func TestAudioMounts_PulseOnly(t *testing.T) {
	dir := t.TempDir()
	pulseDir := filepath.Join(dir, "pulse")
	if err := os.MkdirAll(pulseDir, 0o700); err != nil {
		t.Fatalf("mkdir pulse dir: %v", err)
	}
	listenUnix(t, filepath.Join(pulseDir, "native"))

	mounts, err := AudioMounts(dir)
	if err != nil {
		t.Fatalf("AudioMounts: %v", err)
	}
	if len(mounts) != 1 {
		t.Fatalf("expected exactly 1 mount, got %d: %+v", len(mounts), mounts)
	}
	want := filepath.Join(dir, "pulse", "native")
	if mounts[0].Host != want || mounts[0].SandboxPath != want {
		t.Fatalf("unexpected mount: %+v", mounts[0])
	}
}

func TestAudioMounts_Both(t *testing.T) {
	dir := t.TempDir()
	listenUnix(t, filepath.Join(dir, "pipewire-0"))
	pulseDir := filepath.Join(dir, "pulse")
	if err := os.MkdirAll(pulseDir, 0o700); err != nil {
		t.Fatalf("mkdir pulse dir: %v", err)
	}
	listenUnix(t, filepath.Join(pulseDir, "native"))

	mounts, err := AudioMounts(dir)
	if err != nil {
		t.Fatalf("AudioMounts: %v", err)
	}
	if len(mounts) != 2 {
		t.Fatalf("expected exactly 2 mounts, got %d: %+v", len(mounts), mounts)
	}
}

func TestAudioMounts_StaleRegularFileIgnored(t *testing.T) {
	dir := t.TempDir()
	// A plain file at the expected socket path (e.g. left over from a crash)
	// must not be treated as a working socket.
	path := filepath.Join(dir, "pipewire-0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	if _, err := AudioMounts(dir); err == nil {
		t.Fatal("expected an error — the only candidate present is a stale regular file, not a socket")
	}
}
