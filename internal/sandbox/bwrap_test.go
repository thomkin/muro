package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thomkin/muro/internal/config"
)

func containsSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j, s := range seq {
			if args[i+j] != s {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestBuildArgs_ReadOnlyMount(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{
		Mounts: []config.Mount{{Host: "/host/ro", SandboxPath: "/sbx/ro", Mode: "ro"}},
		Cmd:    []string{"/bin/true"},
	})
	if !containsSeq(args, "--ro-bind", "/host/ro", "/sbx/ro") {
		t.Errorf("expected --ro-bind /host/ro /sbx/ro in args, got %v", args)
	}
	if containsSeq(args, "--bind", "/host/ro", "/sbx/ro") {
		t.Errorf("ro mount must not also appear as --bind, got %v", args)
	}
}

func TestBuildArgs_ReadWriteMount(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{
		Mounts: []config.Mount{{Host: "/host/rw", SandboxPath: "/sbx/rw", Mode: "rw"}},
		Cmd:    []string{"/bin/true"},
	})
	if !containsSeq(args, "--bind", "/host/rw", "/sbx/rw") {
		t.Errorf("expected --bind /host/rw /sbx/rw in args, got %v", args)
	}
}

func TestBuildArgs_UnsetModeDefaultsToReadOnly(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{
		Mounts: []config.Mount{{Host: "/host/x", SandboxPath: "/sbx/x"}}, // Mode unset
		Cmd:    []string{"/bin/true"},
	})
	if !containsSeq(args, "--ro-bind", "/host/x", "/sbx/x") {
		t.Errorf("unset Mode must default to read-only, got %v", args)
	}
}

func TestBuildArgs_EnvSortedDeterministic(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{
		Env: map[string]string{"ZEBRA": "1", "APPLE": "2"},
		Cmd: []string{"/bin/true"},
	})
	var order []string
	for i, a := range args {
		if a == "--setenv" && i+1 < len(args) {
			order = append(order, args[i+1])
		}
	}
	if len(order) != 2 || order[0] != "APPLE" || order[1] != "ZEBRA" {
		t.Errorf("expected sorted env keys [APPLE ZEBRA], got %v", order)
	}
}

func TestBuildArgs_UnshareFlagsAndScaffolding(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{Cmd: []string{"/bin/true"}})
	for _, want := range []string{
		"--unshare-user", "--unshare-pid", "--unshare-net", "--die-with-parent",
	} {
		found := false
		for _, a := range args {
			if a == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %s in args, got %v", want, args)
		}
	}
	if !containsSeq(args, "--proc", "/proc") || !containsSeq(args, "--dev", "/dev") || !containsSeq(args, "--tmpfs", "/tmp") {
		t.Errorf("expected minimal scaffolding (--proc /proc --dev /dev --tmpfs /tmp), got %v", args)
	}
}

func TestBuildArgs_CmdAfterDoubleDash(t *testing.T) {
	b := &BwrapIsolator{}
	args := b.buildArgs(LaunchSpec{Cmd: []string{"/bin/sh", "-c", "echo hi"}})
	if len(args) < 4 {
		t.Fatalf("args too short: %v", args)
	}
	last4 := args[len(args)-4:]
	want := []string{"--", "/bin/sh", "-c", "echo hi"}
	for i := range want {
		if last4[i] != want[i] {
			t.Errorf("expected trailing args %v, got %v", want, last4)
		}
	}
}

func TestNewBwrapIsolator_MissingFromPATH(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir) // a PATH with nothing on it

	_, err := NewBwrapIsolator()
	if err == nil {
		t.Fatal("expected an error when bwrap is not on PATH")
	}
	if !strings.Contains(err.Error(), "bwrap not found") {
		t.Errorf("expected a clear 'bwrap not found' error, got: %v", err)
	}
}

func TestNewBwrapIsolator_FoundButUserNamespacesUnavailable(t *testing.T) {
	// A fake "bwrap" that always fails, to exercise the smoke-test failure
	// path without depending on this host's actual userns support.
	dir := t.TempDir()
	fake := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := NewBwrapIsolator()
	if err == nil {
		t.Fatal("expected an error when the bwrap smoke test fails")
	}
	if !strings.Contains(err.Error(), "smoke test") && !strings.Contains(err.Error(), "unprivileged") {
		t.Errorf("expected a smoke-test/unprivileged-userns error, got: %v", err)
	}
}
