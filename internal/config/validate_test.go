package config

import "testing"

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
