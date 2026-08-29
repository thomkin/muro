package sandbox

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/state"
)

// fakePublisher records every PublishStatus call for assertions, instead
// of actually talking to MQTT (internal/pubsub isn't wired in yet).
type fakePublisher struct {
	events []string // "namespace/name:event"
}

func (p *fakePublisher) PublishStatus(namespace, name, event string) error {
	p.events = append(p.events, namespace+"/"+name+":"+event)
	return nil
}

// fakeProxy records every SetAllowlist/RegisterSandboxAddr call instead of
// touching a real internal/proxy.Server (also not wired in yet).
type fakeProxy struct {
	calls map[string][]string // sandboxID -> last allowURLs set
	addrs map[string]string   // sandboxID -> last registered network address
}

func (p *fakeProxy) SetAllowlist(sandboxID string, allowURLs []string) {
	if p.calls == nil {
		p.calls = make(map[string][]string)
	}
	p.calls[sandboxID] = allowURLs
}

func (p *fakeProxy) RegisterSandboxAddr(sandboxID, addr string) {
	if p.addrs == nil {
		p.addrs = make(map[string]string)
	}
	p.addrs[sandboxID] = addr
}

func newTestManager(t *testing.T) (*Manager, *fakeIsolator, *fakePublisher) {
	t.Helper()
	// resolveSandboxFieldsFromProfile calls BundleDocsMounts, which stats
	// config.ProfileBundleDir(profile.Name) unconditionally — without this,
	// every manager test would touch the real host's ~/muro/profiles/<name>
	// (harmless read, but nondeterministic if a real profile of the same
	// name happens to exist there).
	t.Setenv("MURO_PROFILES_DIR", t.TempDir())
	store := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	iso := newFakeIsolator()
	pub := &fakePublisher{}
	mgr := NewManager(store, iso, &fakeProxy{}, pub)
	// No real sleeping in tests: backoff still "runs" (so shouldRestart's
	// logic is exercised) but instantly.
	mgr.sleep = func(time.Duration) {}
	return mgr, iso, pub
}

func testProfile(name string) *config.Profile {
	return &config.Profile{
		Name:          name,
		Agent:         "claude",
		AllowURLs:     []string{"https://api.anthropic.com"},
		RestartPolicy: "never",
	}
}

// TestRun_AgentArgsFlowIntoLaunchCmd proves a profile's agent_args (e.g.
// Claude Code's own --dangerously-skip-permissions) actually reach the
// launched process's argv, not just get stored — buildLaunchSpec's cmd
// used to be a single-element []string{sb.Agent} with no way to add
// arguments at all.
func TestRun_AgentArgsFlowIntoLaunchCmd(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	p.Agent = "/usr/bin/claude"
	p.AgentArgs = []string{"--dangerously-skip-permissions", "--add-dir", "/workspace"}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	got := iso.launched[len(iso.launched)-1].Cmd
	want := []string{"/usr/bin/claude", "--dangerously-skip-permissions", "--add-dir", "/workspace"}
	if len(got) != len(want) {
		t.Fatalf("Cmd = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Cmd[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestRun_CreatesSandbox(t *testing.T) {
	mgr, iso, pub := newTestManager(t)

	sb, err := mgr.Run(testProfile("p1"), "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if sb.State != state.StateRunning {
		t.Errorf("State = %q, want running", sb.State)
	}
	if iso.handleCount() != 1 {
		t.Errorf("handleCount = %d, want 1", iso.handleCount())
	}
	found := false
	for _, e := range pub.events {
		if e == "default/agent-1:started" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a started event, got %v", pub.events)
	}
}

func TestRun_RejectsDuplicateActiveName(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("first Run error: %v", err)
	}
	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err == nil {
		t.Fatal("expected an error launching a duplicate active name, got nil")
	}
}

func TestRun_AllowsSameNameInDifferentNamespace(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "ns-a"); err != nil {
		t.Fatalf("Run ns-a error: %v", err)
	}
	if _, err := mgr.Run(testProfile("p1"), "agent-1", "ns-b"); err != nil {
		t.Fatalf("Run ns-b error: %v", err)
	}
}

func TestUpdate_SingleNameSelector(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	results, err := mgr.Update(Selector{Name: "agent-1", Namespace: "default"}, ConfigDelta{
		AllowURLs: []string{"https://github.com"},
	})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if len(results) != 1 || !results[0].Applied {
		t.Fatalf("results = %+v, want one applied result", results)
	}

	sb, _ := mgr.store.Get("default", "agent-1")
	if len(sb.AllowURLs) != 2 {
		t.Errorf("AllowURLs = %v, want 2 entries", sb.AllowURLs)
	}
}

func TestUpdate_ProfileSelectorFansOutAtomically(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("shared"), "agent-1", "default"); err != nil {
		t.Fatalf("Run agent-1 error: %v", err)
	}
	if _, err := mgr.Run(testProfile("shared"), "agent-2", "default"); err != nil {
		t.Fatalf("Run agent-2 error: %v", err)
	}

	results, err := mgr.Update(Selector{Profile: "shared"}, ConfigDelta{
		AllowURLs: []string{"https://github.com"},
	})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}

	for _, name := range []string{"agent-1", "agent-2"} {
		sb, _ := mgr.store.Get("default", name)
		if len(sb.AllowURLs) != 2 {
			t.Errorf("%s AllowURLs = %v, want 2 entries", name, sb.AllowURLs)
		}
	}
}

func TestUpdate_ProfileSelectorRejectsWholeBatchOnOneCollision(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("shared"), "agent-1", "default"); err != nil {
		t.Fatalf("Run agent-1 error: %v", err)
	}
	if _, err := mgr.Run(testProfile("shared"), "agent-2", "default"); err != nil {
		t.Fatalf("Run agent-2 error: %v", err)
	}

	// Give agent-2 a pre-existing tool at /usr/local/bin/git so that adding
	// a mount at the same sandbox path collides only for agent-2, not
	// agent-1 — the whole batch must still be rejected.
	live, _ := mgr.store.Get("default", "agent-2")
	sb2 := *live // copy before mutating, same discipline as production code (see manager.go's Reload comment)
	sb2.Tools = []config.Tool{{Host: "/usr/bin/git", As: "git"}}
	if err := mgr.store.Put(&sb2); err != nil {
		t.Fatalf("Put error: %v", err)
	}

	_, err := mgr.Update(Selector{Profile: "shared"}, ConfigDelta{
		AddMounts: []config.Mount{{Host: "/opt/custom-git", SandboxPath: "/usr/local/bin/git", Mode: "ro"}},
	})
	if err == nil {
		t.Fatal("expected the batch to be rejected due to agent-2's collision, got nil error")
	}

	// Neither sandbox should have the new mount — zero side effects.
	sb1, _ := mgr.store.Get("default", "agent-1")
	for _, m := range sb1.Mounts {
		if m.SandboxPath == "/usr/local/bin/git" {
			t.Errorf("agent-1 unexpectedly got the colliding mount applied: %+v", sb1.Mounts)
		}
	}
	sb2Again, _ := mgr.store.Get("default", "agent-2")
	for _, m := range sb2Again.Mounts {
		if m.SandboxPath == "/usr/local/bin/git" {
			t.Errorf("agent-2 unexpectedly got the colliding mount applied: %+v", sb2Again.Mounts)
		}
	}
}

func TestUpdate_AllSelector(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run agent-1 error: %v", err)
	}
	if _, err := mgr.Run(testProfile("p2"), "agent-2", "default"); err != nil {
		t.Fatalf("Run agent-2 error: %v", err)
	}

	results, err := mgr.Update(Selector{All: true}, ConfigDelta{DenyURLs: []string{"https://api.anthropic.com"}})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %+v, want 2", results)
	}
	for _, name := range []string{"agent-1", "agent-2"} {
		sb, _ := mgr.store.Get("default", name)
		if len(sb.AllowURLs) != 0 {
			t.Errorf("%s AllowURLs = %v, want empty after deny", name, sb.AllowURLs)
		}
	}
}

func TestUpdate_MountNotHotApplicableMarksReloadPending(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	iso.lastHandle().updateApplies = false

	results, err := mgr.Update(Selector{Name: "agent-1", Namespace: "default"}, ConfigDelta{
		AddMounts: []config.Mount{{Host: "/opt/extra", SandboxPath: "/workspace/extra", Mode: "ro"}},
	})
	if err != nil {
		t.Fatalf("Update error: %v", err)
	}
	if results[0].Applied {
		t.Error("Applied = true, want false (isolator reported not hot-applicable)")
	}
	sb, _ := mgr.store.Get("default", "agent-1")
	if sb.State != state.StateReloadPending {
		t.Errorf("State = %q, want reload-pending", sb.State)
	}
}

func TestRestartPolicy_NeverEndsInCrashed(t *testing.T) {
	mgr, iso, pub := newTestManager(t)

	p := testProfile("p1")
	p.RestartPolicy = "never"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	iso.lastHandle().finish(1, nil) // non-clean exit
	waitForState(t, mgr, "default", "agent-1", state.StateCrashed)

	if iso.handleCount() != 1 {
		t.Errorf("handleCount = %d, want 1 (never policy must not relaunch)", iso.handleCount())
	}
	assertHasEvent(t, pub, "default/agent-1:crashed")
}

// TestRestartPolicy_NeverEndsInStoppedOnCleanExit closes a real bug:
// watchLoop's final branch used to set StateCrashed unconditionally,
// regardless of exit code — so a sandbox that exited cleanly on its own
// (e.g. the agent/shell ran `exit`) was misreported as crashed, exactly
// the same way `muro sandbox stop` would report it, even though nothing
// went wrong.
func TestRestartPolicy_NeverEndsInStoppedOnCleanExit(t *testing.T) {
	mgr, iso, pub := newTestManager(t)

	p := testProfile("p1")
	p.RestartPolicy = "never"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	iso.lastHandle().finish(0, nil) // clean exit
	waitForState(t, mgr, "default", "agent-1", state.StateStopped)

	if iso.handleCount() != 1 {
		t.Errorf("handleCount = %d, want 1 (never policy must not relaunch)", iso.handleCount())
	}
	assertHasEvent(t, pub, "default/agent-1:stopped")
}

func TestRestartPolicy_OnFailureRetriesThenExhausts(t *testing.T) {
	mgr, iso, pub := newTestManager(t)
	mgr.maxRestartAttempts = 2

	p := testProfile("p1")
	p.RestartPolicy = "on-failure"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	iso.lastHandle().finish(1, nil) // crash #1 -> should relaunch (attempt 1 <= 2)
	waitForHandleCount(t, iso, 2)
	iso.lastHandle().finish(1, nil) // crash #2 -> should relaunch (attempt 2 <= 2)
	waitForHandleCount(t, iso, 3)
	iso.lastHandle().finish(1, nil) // crash #3 -> exhausted (attempt 3 > 2)

	waitForState(t, mgr, "default", "agent-1", state.StateRestartExhausted)
	if iso.handleCount() != 3 {
		t.Errorf("handleCount = %d, want 3 (2 restarts + original launch)", iso.handleCount())
	}
	assertHasEvent(t, pub, "default/agent-1:restart-exhausted")
}

// TestRestart_FromProfilePicksUpMountChange proves the actual point of
// `muro sandbox restart --from-profile`: editing a profile's mounts on
// disk and then restarting a sandbox already running from it picks up the
// new mount, unlike a plain restart (which only reapplies whatever the
// sandbox already had stored, ignoring the profile file entirely).
func TestRestart_FromProfilePicksUpMountChange(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	sb, _ := mgr.store.Get("default", "agent-1")
	if len(sb.Mounts) != 0 {
		t.Fatalf("precondition: expected no mounts yet, got %+v", sb.Mounts)
	}

	// Edit the profile ON DISK, the same way `muro profile mount add` does
	// — mgr.Run's own in-memory p is deliberately not touched, since the
	// whole point is proving Restart re-reads the file.
	p.Mounts = append(p.Mounts, config.Mount{Host: "/usr/bin", SandboxPath: "/usr/bin", Mode: "ro"})
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Restart("default", "agent-1", true); err != nil {
		t.Fatalf("Restart(fromProfile=true) error: %v", err)
	}

	sb, _ = mgr.store.Get("default", "agent-1")
	if len(sb.Mounts) != 1 || sb.Mounts[0].SandboxPath != "/usr/bin" {
		t.Errorf("Mounts after restart --from-profile = %+v, want the newly added /usr/bin mount", sb.Mounts)
	}
	if iso.handleCount() != 2 {
		t.Errorf("handleCount = %d, want 2 (original launch + restart relaunch)", iso.handleCount())
	}
}

// TestRestart_FromProfilePicksUpAgentArgsChange is TestRestart_FromProfilePicksUpMountChange's
// sibling for agent_args specifically, since AgentArgs is a separate field
// resolveSandboxFieldsFromProfile/Restart has to thread through on its own.
func TestRestart_FromProfilePicksUpAgentArgsChange(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	p.AgentArgs = []string{"--dangerously-skip-permissions"}
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Restart("default", "agent-1", true); err != nil {
		t.Fatalf("Restart(fromProfile=true) error: %v", err)
	}

	got := iso.launched[len(iso.launched)-1].Cmd
	if len(got) != 2 || got[1] != "--dangerously-skip-permissions" {
		t.Errorf("Cmd after restart --from-profile = %v, want agent followed by --dangerously-skip-permissions", got)
	}
}

// TestRestart_WithoutFromProfileIgnoresProfileChange proves the default
// (no flag) behavior is unchanged: a plain restart must NOT pick up a
// profile edit, only whatever the sandbox already has stored.
func TestRestart_WithoutFromProfileIgnoresProfileChange(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	p := testProfile("p1")
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	p.Mounts = append(p.Mounts, config.Mount{Host: "/usr/bin", SandboxPath: "/usr/bin", Mode: "ro"})
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Restart("default", "agent-1", false); err != nil {
		t.Fatalf("Restart error: %v", err)
	}

	sb, _ := mgr.store.Get("default", "agent-1")
	if len(sb.Mounts) != 0 {
		t.Errorf("Mounts after plain restart = %+v, want unchanged (empty) — restart without --from-profile must not read the profile file", sb.Mounts)
	}
}

func TestRun_GeneratesUniqueSessionIDPerSandbox(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	sb1, err := mgr.Run(testProfile("p1"), "agent-1", "default")
	if err != nil {
		t.Fatalf("Run agent-1 error: %v", err)
	}
	sb2, err := mgr.Run(testProfile("p1"), "agent-2", "default")
	if err != nil {
		t.Fatalf("Run agent-2 error: %v", err)
	}
	if sb1.SessionID == "" || sb2.SessionID == "" {
		t.Fatalf("expected non-empty SessionID on both, got %q and %q", sb1.SessionID, sb2.SessionID)
	}
	if sb1.SessionID == sb2.SessionID {
		t.Errorf("two different sandboxes got the same SessionID: %q", sb1.SessionID)
	}
}

func TestRestart_FromProfilePreservesSessionID(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	p := testProfile("p1")
	if err := config.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	original := sb.SessionID
	if original == "" {
		t.Fatal("expected a non-empty SessionID after Run")
	}

	if err := mgr.Restart("default", "agent-1", true); err != nil {
		t.Fatalf("Restart(fromProfile=true) error: %v", err)
	}

	after, _ := mgr.store.Get("default", "agent-1")
	if after.SessionID != original {
		t.Errorf("SessionID changed across restart --from-profile: %q -> %q, want unchanged", original, after.SessionID)
	}
}

func TestRun_AgentArgsSessionIDTemplateSubstitution(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	p.Agent = "/usr/bin/claude"
	p.AgentArgs = []string{"--session-id", SessionIDTemplateToken}
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	got := iso.launched[len(iso.launched)-1].Cmd
	want := []string{"/usr/bin/claude", "--session-id", sb.SessionID}
	if len(got) != len(want) {
		t.Fatalf("Cmd = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Cmd[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestRun_InstructionsAndSkillsProduceExpectedMounts(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	dir := t.TempDir()
	instructionsFile := dir + "/AGENT.md"
	if err := os.WriteFile(instructionsFile, []byte("# expert\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillFile := dir + "/deploy.md"
	if err := os.WriteFile(skillFile, []byte("# deploy\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hostHome, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no host home dir available in this environment: %v", err)
	}

	p := testProfile("p1")
	p.Instructions = instructionsFile
	p.Skills = []string{skillFile}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	mounts := iso.launched[len(iso.launched)-1].Mounts
	wantInstr := hostHome + "/.claude/CLAUDE.md"
	wantSkill := hostHome + "/.claude/skills/deploy/SKILL.md"
	var gotInstr, gotSkill bool
	for _, m := range mounts {
		if m.SandboxPath == wantInstr && m.Host == instructionsFile && m.Mode == "ro" {
			gotInstr = true
		}
		if m.SandboxPath == wantSkill && m.Host == skillFile && m.Mode == "ro" {
			gotSkill = true
		}
	}
	if !gotInstr {
		t.Errorf("expected an instructions mount at %q, got %+v", wantInstr, mounts)
	}
	if !gotSkill {
		t.Errorf("expected a skill mount at %q, got %+v", wantSkill, mounts)
	}
}

func TestRun_PrivateDirsProduceIsolatedRWMounts(t *testing.T) {
	mgr, iso, _ := newTestManager(t)
	mgr.SetStateDir(t.TempDir())

	p := testProfile("p1")
	p.PrivateDirs = []string{"/home/agent/.claude/projects"}
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if len(sb.PrivateDirs) != 1 || sb.PrivateDirs[0] != "/home/agent/.claude/projects" {
		t.Errorf("sb.PrivateDirs = %v, want the one configured path", sb.PrivateDirs)
	}

	found := false
	for _, m := range iso.launched[len(iso.launched)-1].Mounts {
		if m.SandboxPath == "/home/agent/.claude/projects" {
			found = true
			if m.Mode != "rw" {
				t.Errorf("private dir mount mode = %q, want rw", m.Mode)
			}
		}
	}
	if !found {
		t.Errorf("expected a mount for the configured private dir, got %+v", iso.launched[len(iso.launched)-1].Mounts)
	}
}

func TestDelete_RejectsActiveSandbox(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if err := mgr.Delete("default", "agent-1", nil); err == nil {
		t.Fatal("expected Delete to reject a still-running sandbox, got nil error")
	}
	if _, ok := mgr.store.Get("default", "agent-1"); !ok {
		t.Error("sandbox record should still exist after a rejected Delete")
	}
}

func TestDelete_RemovesStoppedSandboxAndItsPrivateDirs(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	stateDir := t.TempDir()
	mgr.SetStateDir(stateDir)

	p := testProfile("p1")
	p.PrivateDirs = []string{"/data"}
	sb, err := mgr.Run(p, "agent-1", "default")
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	privateHostDir := filepath.Join(stateDir, "sandboxes", sb.ID, "private", "data")
	if _, err := os.Stat(privateHostDir); err != nil {
		t.Fatalf("precondition: expected private dir to exist: %v", err)
	}

	if err := mgr.Stop("default", "agent-1"); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if err := mgr.Delete("default", "agent-1", nil); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	if _, ok := mgr.store.Get("default", "agent-1"); ok {
		t.Error("sandbox record should be gone after Delete")
	}
	if _, err := os.Stat(privateHostDir); !os.IsNotExist(err) {
		t.Errorf("expected private dir to be removed after Delete, stat err = %v", err)
	}
}

func TestDelete_UnknownSandboxIsAnError(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	if err := mgr.Delete("default", "never-existed", nil); err == nil {
		t.Fatal("expected an error deleting a sandbox that was never created, got nil")
	}
}

// TestRestart_PlainRestartPreservesEnvFromOriginalRun closes a real bug: a
// plain `muro sandbox restart` (no --from-profile) used to always launch
// with an EMPTY environment — buildLaunchSpec took Env as a parameter that
// Run passed profile.Env into, but Restart (without fromProfile) and
// watchLoop's crash-relaunch always passed nil, silently losing every
// profile env var on every restart after the first. Confirmed live: a
// profile env var a launch depended on for correctness
// (CLAUDE_CODE_BUBBLEWRAP) was present on the original `muro run` but
// vanished on the very next plain `muro sandbox restart`.
func TestRestart_PlainRestartPreservesEnvFromOriginalRun(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	p.Env = map[string]string{"SOME_VAR": "some-value"}
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if err := mgr.Restart("default", "agent-1", false); err != nil {
		t.Fatalf("Restart error: %v", err)
	}

	got := iso.launched[len(iso.launched)-1].Env
	if got["SOME_VAR"] != "some-value" {
		t.Errorf("Env after plain restart = %+v, want SOME_VAR=some-value preserved from the original Run", got)
	}
}

func TestRestartPolicy_AlwaysRetriesAfterCleanExit(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	p.RestartPolicy = "always"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	iso.lastHandle().finish(0, nil) // clean exit -> "always" still relaunches
	waitForHandleCount(t, iso, 2)

	sb, _ := mgr.store.Get("default", "agent-1")
	if sb.State != state.StateRunning {
		t.Errorf("State = %q, want running after always-policy relaunch", sb.State)
	}
}

// TestReattach_ResumesRestartPolicyWithContinuousCount closes a real gap:
// a sandbox that survives a murod restart (via muro-shim + Reattach) used
// to NOT get its restart_policy re-applied if it crashed again afterward,
// since Reattach never started a watchLoop. Worse than just "stops
// watching" would be silently resetting RestartCount to 0 on top of that —
// this test's real point is proving BOTH that watching resumes AND that
// it resumes counting from where it left off, not from scratch, so a
// sandbox can't get more total restart attempts than its policy allows
// just because murod happened to restart in the middle of its life.
func TestReattach_ResumesRestartPolicyWithContinuousCount(t *testing.T) {
	mgr, iso, pub := newTestManager(t)
	mgr.maxRestartAttempts = 4

	p := testProfile("p1")
	p.RestartPolicy = "on-failure"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Consume 2 of 4 allowed attempts before the "restart".
	iso.lastHandle().finish(1, nil) // crash #1 -> relaunch (attempt 1 <= 4)
	waitForHandleCount(t, iso, 2)
	iso.lastHandle().finish(1, nil) // crash #2 -> relaunch (attempt 2 <= 4)
	waitForHandleCount(t, iso, 3)
	waitForState(t, mgr, "default", "agent-1", state.StateRunning)

	sb, ok := mgr.store.Get("default", "agent-1")
	if !ok {
		t.Fatal("sandbox not found in store before simulated restart")
	}
	if sb.RestartCount != 2 {
		t.Fatalf("RestartCount before simulated murod restart = %d, want 2", sb.RestartCount)
	}

	// mgr's own watchLoop is still genuinely in-flight here, blocked in
	// Wait() on the current handle — a real murod restart doesn't leave
	// any equivalent goroutine behind at all (the whole process, and every
	// goroutine in it, is gone). clearHandle bumps mgr's epoch for this
	// key, so when that in-flight watchLoop eventually wakes it sees
	// itself superseded and returns without acting — the same mechanism
	// Restart/Stop already rely on to invalidate a stale watcher, reused
	// here to correctly simulate "the old Manager's era is over" instead
	// of leaving two watchers racing over the same handle (confirmed as a
	// real test-construction bug, not a production one: production never
	// has two Managers sharing one handle at the same time).
	mgr.clearHandle(mapKey("default", "agent-1"))

	// Simulate a murod restart: a FRESH Manager over the SAME store and
	// isolator (the underlying fakeHandle — standing in for a real
	// still-running shim process — persists across this, exactly like a
	// real sandbox surviving because muro-shim isn't murod's child).
	freshMgr := NewManager(mgr.store, iso, &fakeProxy{}, pub)
	freshMgr.maxRestartAttempts = 4
	freshMgr.sleep = func(time.Duration) {}

	if errs := freshMgr.ReattachAll(); len(errs) != 0 {
		t.Fatalf("ReattachAll errors: %v", errs)
	}

	// Consume the remaining 2 of 4 attempts through the NEW Manager,
	// against the SAME underlying handle.
	iso.lastHandle().finish(1, nil) // crash #3 -> relaunch (attempt 3 <= 4)
	waitForHandleCount(t, iso, 4)
	iso.lastHandle().finish(1, nil) // crash #4 -> relaunch (attempt 4 <= 4)
	waitForHandleCount(t, iso, 5)
	iso.lastHandle().finish(1, nil) // crash #5 -> exhausted (attempt 5 > 4)

	waitForState(t, freshMgr, "default", "agent-1", state.StateRestartExhausted)
	if iso.handleCount() != 5 {
		t.Errorf("handleCount = %d, want 5 (1 original launch + 4 restarts total, not more just because of the restart in between)", iso.handleCount())
	}
	assertHasEvent(t, pub, "default/agent-1:restart-exhausted")
}

// TestReattach_SkipsSandboxesReconcileAlreadyMarkedDead confirms
// ReattachAll only starts a watchLoop for sandboxes state.Reconcile still
// considers genuinely live (StateRunning) — a sandbox Reconcile already
// downgraded to StateStopped (dead PID) has no live process left to Wait()
// on, and must not be handed to Reattach at all.
func TestReattach_SkipsSandboxesReconcileAlreadyMarkedDead(t *testing.T) {
	mgr, iso, pub := newTestManager(t)

	p := testProfile("p1")
	p.RestartPolicy = "on-failure"
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	// Simulate Reconcile already having found this sandbox's PID dead at
	// startup and downgraded it, before ReattachAll ever runs.
	sb, _ := mgr.store.Get("default", "agent-1")
	sb.State = state.StateStopped
	if err := mgr.store.Put(sb); err != nil {
		t.Fatalf("Put: %v", err)
	}

	freshMgr := NewManager(mgr.store, iso, &fakeProxy{}, pub)
	if errs := freshMgr.ReattachAll(); len(errs) != 0 {
		t.Fatalf("ReattachAll errors: %v", errs)
	}

	if _, ok := freshMgr.getHandle(mapKey("default", "agent-1")); ok {
		t.Error("ReattachAll registered a live handle for a sandbox Reconcile already marked stopped")
	}
}

func TestAttach_ExclusiveThenReleasedByDetach(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	_, detach, err := mgr.Attach("default", "agent-1")
	if err != nil {
		t.Fatalf("first Attach error: %v", err)
	}

	if _, _, err := mgr.Attach("default", "agent-1"); err == nil {
		t.Fatal("expected the second concurrent Attach to be rejected, got nil error")
	}

	detach()

	if _, _, err := mgr.Attach("default", "agent-1"); err != nil {
		t.Fatalf("Attach after detach should succeed, got: %v", err)
	}
}

func TestRestart_ForceDetachesExistingAttach(t *testing.T) {
	mgr, iso, _ := newTestManager(t)

	if _, err := mgr.Run(testProfile("p1"), "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if _, _, err := mgr.Attach("default", "agent-1"); err != nil {
		t.Fatalf("Attach error: %v", err)
	}

	if err := mgr.Restart("default", "agent-1", false); err != nil {
		t.Fatalf("Restart error: %v", err)
	}
	if iso.handleCount() != 2 {
		t.Fatalf("handleCount = %d, want 2 after Restart", iso.handleCount())
	}

	// The old attach session must have been force-detached by Restart, so
	// a fresh Attach against the new process succeeds immediately.
	if _, _, err := mgr.Attach("default", "agent-1"); err != nil {
		t.Fatalf("Attach after Restart should succeed (force-detached), got: %v", err)
	}
}

func TestRestart_DoesNotDoubleRestartViaStaleWatcher(t *testing.T) {
	// Regression test for the epoch mechanism: killing the old handle as
	// part of an explicit Restart must not cause the *old* watchLoop
	// (still waiting on that now-dead handle) to also treat the exit as a
	// crash and independently apply restart_policy — Restart already owns
	// that transition.
	mgr, iso, _ := newTestManager(t)

	p := testProfile("p1")
	p.RestartPolicy = "always" // worst case: a stale watcher applying policy would definitely relaunch again
	if _, err := mgr.Run(p, "agent-1", "default"); err != nil {
		t.Fatalf("Run error: %v", err)
	}

	if err := mgr.Restart("default", "agent-1", false); err != nil {
		t.Fatalf("Restart error: %v", err)
	}

	// Give any (incorrect) stale-watcher goroutine a moment to misbehave.
	waitForHandleCount(t, iso, 2)
	time.Sleep(20 * time.Millisecond)
	if iso.handleCount() != 2 {
		t.Errorf("handleCount = %d, want exactly 2 (stale watcher must not have relaunched again)", iso.handleCount())
	}
}

// waitForState polls (bounded) until namespace/name reaches want, since
// watchLoop's restart-policy handling runs on its own goroutine.
func waitForState(t *testing.T, mgr *Manager, namespace, name string, want state.SandboxState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sb, ok := mgr.store.Get(namespace, name)
		if ok && sb.State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	sb, _ := mgr.store.Get(namespace, name)
	t.Fatalf("timed out waiting for %s/%s to reach state %q, last seen %+v", namespace, name, want, sb)
}

func waitForHandleCount(t *testing.T, iso *fakeIsolator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if iso.handleCount() >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for handleCount >= %d, got %d", want, iso.handleCount())
}

func assertHasEvent(t *testing.T, pub *fakePublisher, want string) {
	t.Helper()
	for _, e := range pub.events {
		if e == want {
			return
		}
	}
	t.Errorf("expected event %q, got %v", want, pub.events)
}
