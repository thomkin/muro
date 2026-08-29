package gitproxy

import (
	"testing"

	"github.com/thomkin/muro/internal/config"
)

func testMounts() []config.Mount {
	return []config.Mount{
		{Host: "/home/user/projects/foo", SandboxPath: "/workspace", Mode: "rw"},
	}
}

func testProfilePolicy() config.GitPolicy {
	return config.GitPolicy{
		Repos: []config.GitRepoPolicy{
			{
				Host:            "/home/user/projects/foo",
				AllowedBranches: []string{"ai", "ai/*"},
				AllowedRemotes:  []string{"origin"},
			},
		},
	}
}

func defaultSubcommands() []string {
	return []string{"status", "diff", "log", "show", "add", "commit", "push", "fetch", "pull"}
}

func TestTranslateCwd(t *testing.T) {
	mounts := testMounts()

	host, ok := TranslateCwd(mounts, "/workspace")
	if !ok || host != "/home/user/projects/foo" {
		t.Fatalf("got %q, %v", host, ok)
	}

	host, ok = TranslateCwd(mounts, "/workspace/src")
	if !ok || host != "/home/user/projects/foo/src" {
		t.Fatalf("got %q, %v", host, ok)
	}

	_, ok = TranslateCwd(mounts, "/etc")
	if ok {
		t.Fatalf("expected no match for /etc")
	}

	// longest-prefix-wins
	overlap := []config.Mount{
		{Host: "/host/wide", SandboxPath: "/workspace", Mode: "rw"},
		{Host: "/host/narrow", SandboxPath: "/workspace/sub", Mode: "rw"},
	}
	host, ok = TranslateCwd(overlap, "/workspace/sub/x")
	if !ok || host != "/host/narrow/x" {
		t.Fatalf("expected longest-prefix match, got %q, %v", host, ok)
	}
}

func TestValidate_SubcommandNotInDaemonCeiling(t *testing.T) {
	_, _, err := Validate(Request{Argv: []string{"clone", "x"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection for a subcommand outside the daemon ceiling")
	}
}

func TestValidate_CwdNotUnderAnyMount(t *testing.T) {
	_, _, err := Validate(Request{Argv: []string{"status"}, Cwd: "/somewhere/else"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection for cwd outside any mount")
	}
}

func TestValidate_RepoNotConfigured(t *testing.T) {
	mounts := []config.Mount{{Host: "/home/user/other", SandboxPath: "/workspace", Mode: "rw"}}
	_, _, err := Validate(Request{Argv: []string{"status"}, Cwd: "/workspace"}, mounts, testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection when no repo policy covers the resolved host path")
	}
}

func TestValidate_StatusOK(t *testing.T) {
	hostRepo, sub, err := Validate(Request{Argv: []string{"status"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hostRepo != "/home/user/projects/foo" || sub != "status" {
		t.Fatalf("got %q %q", hostRepo, sub)
	}
}

func TestValidate_StatusRejectsArgs(t *testing.T) {
	_, _, err := Validate(Request{Argv: []string{"status", "--short"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection for status with arguments")
	}
}

func TestValidate_DiffVariants(t *testing.T) {
	for _, argv := range [][]string{{"diff"}, {"diff", "--staged"}} {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
			t.Fatalf("argv %v: unexpected error: %v", argv, err)
		}
	}
	if _, _, err := Validate(Request{Argv: []string{"diff", "HEAD~1"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
		t.Fatal("expected rejection for diff with an unsupported argument")
	}
}

func TestValidate_AddRejectsFlagsAndAbsolutePaths(t *testing.T) {
	cases := [][]string{
		{"add"},                // no paths at all
		{"add", "-A"},          // flag
		{"add", "/etc/passwd"}, // absolute path
	}
	for _, argv := range cases {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
			t.Fatalf("argv %v: expected rejection", argv)
		}
	}
	if _, _, err := Validate(Request{Argv: []string{"add", "src/main.go"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
		t.Fatalf("unexpected error for a relative add: %v", err)
	}
}

func TestValidate_CommitOnlyAllowsDashM(t *testing.T) {
	cases := [][]string{
		{"commit"},
		{"commit", "--amend"},
		{"commit", "-am", "msg"},
		{"commit", "-m", "one", "-m", "two"},
	}
	for _, argv := range cases {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
			t.Fatalf("argv %v: expected rejection", argv)
		}
	}
	if _, _, err := Validate(Request{Argv: []string{"commit", "-m", "a message"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidate_PushRequiresExplicitRemoteAndBranch(t *testing.T) {
	cases := [][]string{
		{"push"},
		{"push", "origin"},
		{"push", "origin", "ai", "extra"},
	}
	for _, argv := range cases {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
			t.Fatalf("argv %v: expected rejection", argv)
		}
	}
}

func TestValidate_PushRemoteNotAllowed(t *testing.T) {
	_, _, err := Validate(Request{Argv: []string{"push", "upstream", "ai"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection for a remote not in AllowedRemotes")
	}
}

func TestValidate_PushBranchNotAllowed(t *testing.T) {
	_, _, err := Validate(Request{Argv: []string{"push", "origin", "main"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands())
	if err == nil {
		t.Fatal("expected rejection for a branch not matching any allowed pattern")
	}
}

func TestValidate_PushBranchGlobMatch(t *testing.T) {
	if _, _, err := Validate(Request{Argv: []string{"push", "origin", "ai/feature-x"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
		t.Fatalf("unexpected error for a glob-matched branch: %v", err)
	}
}

func TestValidate_PushLocalColonRemoteForm(t *testing.T) {
	if _, _, err := Validate(Request{Argv: []string{"push", "origin", "local-branch:ai"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, _, err := Validate(Request{Argv: []string{"push", "origin", "ai:main"}, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
		t.Fatal("expected rejection when the destination side of local:remote is not allowed")
	}
}

func TestValidate_PushFlagInjectionRejected(t *testing.T) {
	cases := [][]string{
		{"push", "--upload-pack=evil", "ai"},
		{"push", "origin", "--force"},
		{"push", "-f", "ai"},
	}
	for _, argv := range cases {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
			t.Fatalf("argv %v: expected rejection", argv)
		}
	}
}

func TestValidate_FetchPull(t *testing.T) {
	ok := [][]string{
		{"fetch"}, {"fetch", "origin"},
		{"pull"}, {"pull", "origin"}, {"pull", "origin", "ai"},
	}
	for _, argv := range ok {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err != nil {
			t.Fatalf("argv %v: unexpected error: %v", argv, err)
		}
	}
	bad := [][]string{
		{"fetch", "upstream"},
		{"pull", "origin", "-x"},
	}
	for _, argv := range bad {
		if _, _, err := Validate(Request{Argv: argv, Cwd: "/workspace"}, testMounts(), testProfilePolicy(), defaultSubcommands()); err == nil {
			t.Fatalf("argv %v: expected rejection", argv)
		}
	}
}
