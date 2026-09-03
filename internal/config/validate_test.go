package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateProfile_OK(t *testing.T) {
	p := &Profile{
		Name:  "ok-profile",
		Agent: "claude",
		Mounts: []Mount{
			{Host: "~/projects/myrepo", SandboxPath: "/workspace", Mode: "rw"},
		},
		Tools: []Tool{
			{Host: "/usr/bin/git", As: "git"},
			{Host: "/usr/bin/node", As: "node"},
		},
		RestartPolicy: "never",
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile() = %v, want nil", err)
	}
}

func TestValidateProfile_EmptyRestartPolicyOK(t *testing.T) {
	p := &Profile{Name: "no-policy", Agent: "claude"}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile() with empty restart_policy = %v, want nil", err)
	}
}

func TestValidateProfile_InvalidRestartPolicy(t *testing.T) {
	p := &Profile{Name: "bad-policy", Agent: "claude", RestartPolicy: "sometimes"}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for an invalid restart_policy")
	}
}

func TestValidateProfile_EmptyWorkDirOK(t *testing.T) {
	p := &Profile{Name: "no-workdir", Agent: "claude"}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile() with empty workdir = %v, want nil", err)
	}
}

func TestValidateProfile_AbsoluteWorkDirOK(t *testing.T) {
	p := &Profile{Name: "workdir-profile", Agent: "claude", WorkDir: "/workspace"}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("ValidateProfile() with absolute workdir = %v, want nil", err)
	}
}

func TestValidateProfile_RelativeWorkDirRejected(t *testing.T) {
	p := &Profile{Name: "bad-workdir", Agent: "claude", WorkDir: "workspace"}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a non-absolute workdir")
	}
}

func TestValidateProfile_RequiresName(t *testing.T) {
	if err := ValidateProfile(&Profile{}); err == nil {
		t.Error("expected an error for a profile with no name")
	}
}

func TestValidateProfile_ToolMountCollision(t *testing.T) {
	p := &Profile{
		Name:  "colliding-profile",
		Agent: "claude",
		Mounts: []Mount{
			{Host: "/opt/custom-git", SandboxPath: "/usr/local/bin/git", Mode: "ro"},
		},
		Tools: []Tool{
			{Host: "/usr/bin/git", As: "git"},
		},
	}
	err := ValidateProfile(p)
	if err == nil {
		t.Fatal("expected an error for a tools:/mounts: path collision")
	}
	t.Logf("got expected error: %v", err)
}

func TestValidateProfile_ToolToolCollision(t *testing.T) {
	p := &Profile{
		Name:  "dup-tool-profile",
		Agent: "claude",
		Tools: []Tool{
			{Host: "/usr/bin/git", As: "git"},
			{Host: "/opt/other/git", As: "git"},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for two tools: entries resolving to the same sandbox path")
	}
}

func TestValidSandboxName_ValidAccepted(t *testing.T) {
	for _, s := range []string{"claude-1", "default", "my_agent", "a1b2c3", "team.prod"} {
		if err := ValidSandboxName("name", s); err != nil {
			t.Errorf("ValidSandboxName(%q) = %v, want nil", s, err)
		}
	}
}

func TestValidSandboxName_EmptyRejected(t *testing.T) {
	if err := ValidSandboxName("name", ""); err == nil {
		t.Error("expected an error for an empty name")
	}
}

func TestValidSandboxName_PathTraversalRejected(t *testing.T) {
	for _, s := range []string{"../etc", "..", "foo/../../bar", "a/../../../etc/passwd"} {
		if err := ValidSandboxName("name", s); err == nil {
			t.Errorf("ValidSandboxName(%q) should have been rejected (path traversal)", s)
		}
	}
}

func TestValidSandboxName_PathSeparatorRejected(t *testing.T) {
	for _, s := range []string{"foo/bar", "foo\\bar", "/etc/passwd"} {
		if err := ValidSandboxName("name", s); err == nil {
			t.Errorf("ValidSandboxName(%q) should have been rejected (path separator)", s)
		}
	}
}

func TestValidSandboxName_LeadingDotRejected(t *testing.T) {
	if err := ValidSandboxName("name", ".hidden"); err == nil {
		t.Error("expected an error for a name starting with \".\"")
	}
}

func TestValidateProfile_RootMountRWRejected(t *testing.T) {
	p := &Profile{
		Name:   "root-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/", SandboxPath: "/host", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting / read-write")
	}
}

func TestValidateProfile_MuroStateDirRWRejected(t *testing.T) {
	stateDir, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir(): %v", err)
	}
	p := &Profile{
		Name:   "state-dir-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: stateDir, SandboxPath: "/muro-state", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting muro's own state dir read-write")
	}
}

// TestValidateProfile_FileInsideStateDirRWRejected covers the exact
// scenario this validation exists to prevent (per its own doc comment):
// a sandboxed agent modifying muro's own state.json out from under murod.
// A mount targeting state.json specifically — narrower than StateDir, not
// StateDir itself or something broader containing it — is a distinct code
// path from TestValidateProfile_MuroStateDirRWRejected above (which only
// covers mounting StateDir directly) and caught a real bug during review:
// an earlier version of this check only rejected mounts that covered a
// dangerous root, not ones narrower than it, letting exactly this through.
func TestValidateProfile_FileInsideStateDirRWRejected(t *testing.T) {
	stateDir, err := StateDir()
	if err != nil {
		t.Fatalf("StateDir(): %v", err)
	}
	p := &Profile{
		Name:  "state-json-rw",
		Agent: "claude",
		Mounts: []Mount{{
			Host:        filepath.Join(stateDir, "state.json"),
			SandboxPath: "/muro-state.json",
			Mode:        "rw",
		}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting a file inside muro's own state dir read-write")
	}
}

// TestValidateProfile_SiblingOfRootRWAccepted confirms the "/" fix didn't
// overcorrect into rejecting every mount: a mount that has nothing to do
// with any dangerous root must still validate successfully. This is the
// regression test for a real bug caught during review — a symmetric
// overlap check against "/" (pathCoversOrEquals("/", hostAbs)) is true for
// every absolute path by definition, which briefly rejected all mounts,
// including this one, before "/" was given one-directional-only treatment.
func TestValidateProfile_SiblingOfRootRWAccepted(t *testing.T) {
	p := &Profile{
		Name:   "unrelated-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/opt/some-safe-tool", SandboxPath: "/workspace", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("expected a mount unrelated to any dangerous root to validate, got: %v", err)
	}
}

func TestValidateProfile_MuroConfigDirRWRejected(t *testing.T) {
	cfgDir, err := ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir(): %v", err)
	}
	p := &Profile{
		Name:   "config-dir-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: cfgDir, SandboxPath: "/muro-config", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting muro's own config dir read-write")
	}
}

func TestValidateProfile_HomeSubdirRWAccepted(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir(): %v", err)
	}
	p := &Profile{
		Name:   "home-subdir-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: home + "/projects/myrepo", SandboxPath: "/workspace", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("mounting a subdirectory of home rw should be accepted, got: %v", err)
	}
}

func TestValidateProfile_HomeRootRWRejected(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir(): %v", err)
	}
	p := &Profile{
		Name:   "home-root-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: home, SandboxPath: "/home-copy", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting the home directory root read-write")
	}
}

func TestValidateProfile_TildeHomeRootRWRejected(t *testing.T) {
	p := &Profile{
		Name:   "tilde-home-root-rw",
		Agent:  "claude",
		Mounts: []Mount{{Host: "~", SandboxPath: "/home-copy", Mode: "rw"}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for mounting \"~\" (home root) read-write")
	}
}

func TestValidateProfile_EtcReadOnlyAccepted(t *testing.T) {
	p := &Profile{
		Name:   "etc-ro",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/etc", SandboxPath: "/etc", Mode: "ro"}},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("mounting /etc read-only should be accepted, got: %v", err)
	}
}

func TestValidateProfile_ShellMountsReadOnlyAccepted(t *testing.T) {
	// Matches test/integration/bwrap_test.go's shellMounts() pattern: giving
	// a sandbox a working shell via read-only system mounts is legitimate
	// and must keep validating successfully.
	p := &Profile{
		Name:  "shell-mounts-ro",
		Agent: "claude",
		Mounts: []Mount{
			{Host: "/usr", SandboxPath: "/usr", Mode: "ro"},
			{Host: "/bin", SandboxPath: "/bin", Mode: "ro"},
			{Host: "/lib", SandboxPath: "/lib", Mode: "ro"},
			{Host: "/lib64", SandboxPath: "/lib64", Mode: "ro"},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("legitimate read-only shell mounts should be accepted, got: %v", err)
	}
}

func TestValidateProfile_SandboxPathOverridingProcRejected(t *testing.T) {
	for _, sandboxPath := range []string{"/proc", "/dev", "/tmp"} {
		p := &Profile{
			Name:   "scaffold-override",
			Agent:  "claude",
			Mounts: []Mount{{Host: "/some/host/dir", SandboxPath: sandboxPath, Mode: "ro"}},
		}
		if err := ValidateProfile(p); err == nil {
			t.Errorf("expected an error for a mount overriding sandbox scaffolding path %q (mode ro)", sandboxPath)
		}

		p.Mounts[0].Mode = "rw"
		if err := ValidateProfile(p); err == nil {
			t.Errorf("expected an error for a mount overriding sandbox scaffolding path %q (mode rw)", sandboxPath)
		}
	}
}

func TestValidateProfile_WildcardToolNeverCollides(t *testing.T) {
	p := &Profile{
		Name:  "wildcard-profile",
		Agent: "claude",
		Mounts: []Mount{
			{Host: "/some/dir", SandboxPath: "/usr/local/bin", Mode: "ro"},
		},
		Tools: []Tool{
			{Host: "~/.muro/toolchains/claude-default/bin", As: "*"},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("wildcard tool should never collide with a mount, got: %v", err)
	}
}

func TestValidateProfile_PrivateDirsAccepted(t *testing.T) {
	p := &Profile{
		Name:        "private-dirs-ok",
		Agent:       "claude",
		Mounts:      []Mount{{Host: "/tmp", SandboxPath: "/workspace", Mode: "rw"}},
		PrivateDirs: []string{"/home/agent/.claude/projects"},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("a private_dirs entry with no collision should be accepted, got: %v", err)
	}
}

func TestValidateProfile_DuplicatePrivateDirRejected(t *testing.T) {
	p := &Profile{
		Name:        "dup-private-dir",
		Agent:       "claude",
		PrivateDirs: []string{"/data", "/data"},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected a duplicate private_dirs entry to be rejected")
	}
}

func TestValidateProfile_PrivateDirCollidesWithMountRejected(t *testing.T) {
	p := &Profile{
		Name:        "collide-mount",
		Agent:       "claude",
		Mounts:      []Mount{{Host: "/tmp", SandboxPath: "/data", Mode: "ro"}},
		PrivateDirs: []string{"/data"},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected a private_dirs entry colliding with a mounts: target to be rejected")
	}
}

func TestValidateProfile_PrivateDirCollidesWithToolRejected(t *testing.T) {
	p := &Profile{
		Name:        "collide-tool",
		Agent:       "claude",
		Tools:       []Tool{{Host: "/usr/bin/git", As: "git"}},
		PrivateDirs: []string{"/usr/local/bin/git"},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected a private_dirs entry colliding with a tools: target to be rejected")
	}
}

func TestValidateProfile_GitPolicyRepoNotCoveredByMountsRejected(t *testing.T) {
	p := &Profile{
		Name:   "git-uncovered",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/home/user/other", SandboxPath: "/workspace", Mode: "rw"}},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", AllowedBranches: []string{"ai"}, AllowedRemotes: []string{"origin"}},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a git policy repo not covered by any mounts: entry")
	}
}

func TestValidateProfile_GitPolicyRepoCoveredByMountsAccepted(t *testing.T) {
	p := &Profile{
		Name:   "git-covered",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/home/user/projects/foo", SandboxPath: "/workspace", Mode: "rw"}},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", AllowedBranches: []string{"ai"}, AllowedRemotes: []string{"origin"}},
			},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("expected a mount-covered git policy repo to be accepted, got: %v", err)
	}
}

func TestValidateProfile_GitPolicyEmptyBranchesOrRemotesRejected(t *testing.T) {
	base := Profile{
		Name:   "git-empty",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/home/user/projects/foo", SandboxPath: "/workspace", Mode: "rw"}},
	}

	noBranches := base
	noBranches.Git = GitPolicy{Repos: []GitRepoPolicy{
		{Host: "/home/user/projects/foo", AllowedRemotes: []string{"origin"}},
	}}
	if err := ValidateProfile(&noBranches); err == nil {
		t.Error("expected an error for a repo policy with no allowed_branches")
	}

	noRemotes := base
	noRemotes.Git = GitPolicy{Repos: []GitRepoPolicy{
		{Host: "/home/user/projects/foo", AllowedBranches: []string{"ai"}},
	}}
	if err := ValidateProfile(&noRemotes); err == nil {
		t.Error("expected an error for a repo policy with no allowed_remotes")
	}
}

func TestValidateProfile_WorktreeRepoAccepted(t *testing.T) {
	p := &Profile{
		Name:  "worktree-ok",
		Agent: "claude",
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("expected a worktree: true repo with a mount_path to be accepted, got: %v", err)
	}
}

func TestValidateProfile_WorktreeRepoNotRequiredToBeCoveredByMounts(t *testing.T) {
	// The whole point of worktree: true is that the sandbox never sees
	// Host directly — muro generates the mount itself, so unlike a
	// non-worktree entry, Host must NOT need a matching mounts: entry.
	p := &Profile{
		Name:  "worktree-uncovered-ok",
		Agent: "claude",
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/nowhere-mounted", Worktree: true, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("expected a worktree repo to be accepted without any covering mounts: entry, got: %v", err)
	}
}

func TestValidateProfile_WorktreeRepoEmptyAllowedRemotesAccepted(t *testing.T) {
	// Unlike a non-worktree entry, empty allowed_remotes is legitimate for
	// a worktree — merging back is a host-side muro operation, not a
	// sandbox push, so "no remote access at all" is a normal, valid
	// configuration, not a pointless empty declaration.
	p := &Profile{
		Name:  "worktree-no-remotes-ok",
		Agent: "claude",
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err != nil {
		t.Errorf("expected empty allowed_remotes on a worktree repo to be accepted, got: %v", err)
	}
}

func TestValidateProfile_WorktreeRepoMissingMountPathRejected(t *testing.T) {
	p := &Profile{
		Name:  "worktree-no-mountpath",
		Agent: "claude",
		Git:   GitPolicy{Repos: []GitRepoPolicy{{Host: "/home/user/projects/foo", Worktree: true}}},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a worktree: true repo with no mount_path")
	}
}

func TestValidateProfile_WorktreeRepoWithAllowedBranchesRejected(t *testing.T) {
	// allowed_branches is computed automatically from the sandbox's own
	// generated branch (DESIGN.md §15 refinement 1) — a profile author
	// declaring it themselves can't know that name in advance, so it's
	// rejected rather than silently ignored.
	p := &Profile{
		Name:  "worktree-with-branches",
		Agent: "claude",
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/foo", AllowedBranches: []string{"main"}},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a worktree: true repo with allowed_branches set")
	}
}

func TestValidateProfile_NonWorktreeRepoWithMountPathRejected(t *testing.T) {
	p := &Profile{
		Name:   "non-worktree-with-mountpath",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/home/user/projects/foo", SandboxPath: "/workspace", Mode: "rw"}},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", AllowedBranches: []string{"ai"}, AllowedRemotes: []string{"origin"}, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error for a non-worktree repo that also sets mount_path")
	}
}

func TestValidateProfile_WorktreeMountPathCollidesWithMountRejected(t *testing.T) {
	p := &Profile{
		Name:   "worktree-mount-collision",
		Agent:  "claude",
		Mounts: []Mount{{Host: "/tmp", SandboxPath: "/workspace/foo", Mode: "rw"}},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error when a worktree's mount_path collides with a mounts: entry")
	}
}

func TestValidateProfile_WorktreeMountPathCollidesWithToolRejected(t *testing.T) {
	p := &Profile{
		Name:  "worktree-tool-collision",
		Agent: "claude",
		Tools: []Tool{{Host: "/usr/bin/node", As: "node"}},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: toolRoot + "/node"},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error when a worktree's mount_path collides with a tools: entry")
	}
}

func TestValidateProfile_WorktreeMountPathCollidesWithPrivateDirRejected(t *testing.T) {
	p := &Profile{
		Name:        "worktree-privatedir-collision",
		Agent:       "claude",
		PrivateDirs: []string{"/workspace/foo"},
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/foo"},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error when a worktree's mount_path collides with a private_dirs entry")
	}
}

func TestValidateProfile_TwoWorktreeReposSameMountPathRejected(t *testing.T) {
	p := &Profile{
		Name:  "worktree-worktree-collision",
		Agent: "claude",
		Git: GitPolicy{
			Repos: []GitRepoPolicy{
				{Host: "/home/user/projects/foo", Worktree: true, MountPath: "/workspace/shared"},
				{Host: "/home/user/projects/bar", Worktree: true, MountPath: "/workspace/shared"},
			},
		},
	}
	if err := ValidateProfile(p); err == nil {
		t.Error("expected an error when two worktree repos both set the same mount_path")
	}
}
