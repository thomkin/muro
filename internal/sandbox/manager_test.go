package sandbox

import (
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

// fakeProxy records every SetAllowlist call instead of touching a real
// internal/proxy.Server (also not wired in yet).
type fakeProxy struct {
	calls map[string][]string // sandboxID -> last allowURLs set
}

func (p *fakeProxy) SetAllowlist(sandboxID string, allowURLs []string) {
	if p.calls == nil {
		p.calls = make(map[string][]string)
	}
	p.calls[sandboxID] = allowURLs
}

func newTestManager(t *testing.T) (*Manager, *fakeIsolator, *fakePublisher) {
	t.Helper()
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

	if err := mgr.Restart("default", "agent-1"); err != nil {
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

	if err := mgr.Restart("default", "agent-1"); err != nil {
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
