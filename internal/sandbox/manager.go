package sandbox

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/state"
)

// ProxyUpdater is the minimal surface Manager needs from the URL-allowlist
// proxy (internal/proxy.Server implements this). Kept as a local interface
// so this package doesn't depend on internal/proxy, which is implemented
// separately.
type ProxyUpdater interface {
	// SetAllowlist hot-swaps the allowlist ruleset for one sandbox
	// (DESIGN.md §6.3/§9) — always live, no restart ever required.
	SetAllowlist(sandboxID string, allowURLs []string)

	// RegisterSandboxAddr records that a sandbox's bridged network address
	// (Stage 2 networking — internal/sandbox/network.go) belongs to
	// sandboxID, so the proxy can identify inbound connections by source
	// address. Called after every Launch whose Handle exposes one (real
	// bwrap-launched sandboxes always do; a FakeIsolator in tests need
	// not).
	RegisterSandboxAddr(sandboxID, addr string)
}

// registerHandleNetworkAddr tells proxy which address h's sandbox is
// bridged through, if any — h implementing networkAddrProvider is a Go
// "optional interface" check (network.go), not every Isolator provides
// real networking (test fakes don't), so a miss here is expected and
// silently skipped, not an error.
func registerHandleNetworkAddr(proxy ProxyUpdater, sandboxID string, h Handle) {
	if proxy == nil {
		return
	}
	np, ok := h.(networkAddrProvider)
	if !ok {
		return
	}
	if addr := np.NetworkAddr(); addr != "" {
		proxy.RegisterSandboxAddr(sandboxID, addr)
	}
}

// shimRuntimeInfo is an optional capability a Handle may implement to
// expose where its persistent shim process's runtime files live (Stage 3
// lifecycle, shim.go/bwrap.go) — real bwrap-launched sandboxes always do,
// a FakeIsolator's test handles don't need to. Manager persists both
// values into state.Sandbox so a restarted murod can reconstruct a Handle
// (Reattach, below) for an already-running sandbox without needing to
// still be its shim's parent process. Same "optional interface" idiom as
// networkAddrProvider (network.go).
type shimRuntimeInfo interface {
	ShimSocket() string
	SlirpPID() int
}

// captureHandleInfo copies whatever PID.md/networking/shim-runtime info h
// exposes into sb, ready for store.Put — every Launch call site (Run,
// Restart, watchLoop's relaunch) needs exactly this, so it's centralized
// here rather than tripled inline.
func captureHandleInfo(sb *state.Sandbox, h Handle) {
	sb.PID = h.PID()
	if np, ok := h.(networkAddrProvider); ok {
		sb.NetAddr = np.NetworkAddr()
	}
	if si, ok := h.(shimRuntimeInfo); ok {
		sb.ShimSocket = si.ShimSocket()
		sb.SlirpPID = si.SlirpPID()
	}
}

// Reattacher is an optional capability an Isolator may implement to
// reconstruct a Handle for a sandbox process it didn't itself just Launch
// — the case right after a murod restart, once state.Reconcile has
// confirmed the persisted PID is still alive. BwrapIsolator implements
// this (bwrap.go); a test fake has no equivalent concept and simply
// doesn't implement it, which Manager.Reattach reports as a clear error
// rather than requiring every Isolator to support it.
type Reattacher interface {
	Reattach(pid int, shimSocket string, slirpPID int, netAddr string) (Handle, error)
}

// Reattach reconstructs a live Handle for sb, whose process
// state.Reconcile has already confirmed survived from a previous murod
// process, and registers it exactly as Run would (proxy allowlist +
// network address). It deliberately does NOT start a watchLoop —
// resuming restart_policy tracking across a daemon restart is a separate,
// explicitly out-of-scope piece (shim.go's design note); this exists
// solely so `muro sandbox attach`/`stop` keep working against a sandbox
// that outlived the murod process that originally launched it.
func (m *Manager) Reattach(sb *state.Sandbox) error {
	ra, ok := m.isolator.(Reattacher)
	if !ok {
		return fmt.Errorf("isolator does not support reattaching to an already-running sandbox")
	}
	h, err := ra.Reattach(sb.PID, sb.ShimSocket, sb.SlirpPID, sb.NetAddr)
	if err != nil {
		return fmt.Errorf("reattach sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	m.setHandle(mapKey(sb.Namespace, sb.Name), h)
	if m.proxy != nil {
		m.proxy.SetAllowlist(sb.ID, sb.AllowURLs)
		if sb.NetAddr != "" {
			m.proxy.RegisterSandboxAddr(sb.ID, sb.NetAddr)
		}
	}
	return nil
}

// ReattachAll calls Reattach for every sandbox the Store currently has
// marked StateRunning — meant to be called once at murod startup, after
// state.Reconcile has already downgraded anything that didn't actually
// survive. Errors for individual sandboxes are collected, not fatal to
// the whole pass: one unreattachable sandbox (e.g. its shim genuinely
// died in the gap between Reconcile and this call) shouldn't block every
// other one from coming back under murod's management.
func (m *Manager) ReattachAll() []error {
	var errs []error
	for _, sb := range m.store.List("") {
		if sb.State != state.StateRunning {
			continue
		}
		if err := m.Reattach(sb); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// EventPublisher is the minimal surface Manager needs from the pub/sub
// client (internal/pubsub.Client implements this). Kept as a local
// interface so this package doesn't depend on internal/pubsub.
type EventPublisher interface {
	// PublishStatus publishes a lifecycle event for namespace/name.
	// event is one of: started, stopped, reload-pending, restarted,
	// crashed, restarting, restart-exhausted (DESIGN.md §8/§13).
	PublishStatus(namespace, name, event string) error
}

// Selector chooses which running sandbox(es) an Update targets
// (DESIGN.md §11): exactly one of Name, Profile, or All should be set.
type Selector struct {
	Name      string // target one sandbox by name (with Namespace)
	Namespace string // scopes Name, Profile, or All to one namespace; "" with Profile/All means every namespace
	Profile   string // target every running sandbox launched from this profile
	All       bool   // target every running sandbox, regardless of profile
}

// ConfigDelta is the change a Selector's matched sandboxes should receive.
type ConfigDelta struct {
	AddMounts []config.Mount
	AllowURLs []string // URLs to add to the allowlist
	DenyURLs  []string // URLs to remove from the allowlist
}

// UpdateResult reports what happened to one sandbox in a bulk Update.
type UpdateResult struct {
	Namespace string
	Name      string
	Applied   bool // true: fully hot-applied. false: mount change needs Restart (state.StateReloadPending)
}

// Manager orchestrates sandbox lifecycle (DESIGN.md §9/§11/§12/§13) on top
// of an Isolator, a state.Store, and the ProxyUpdater/EventPublisher
// interfaces above. Safe for concurrent use.
type Manager struct {
	mu      sync.Mutex
	handles map[string]Handle // key: namespace+"/"+name — the live process for a running sandbox
	epoch   map[string]int    // bumped whenever handles[key] is replaced or cleared, to invalidate stale watchLoop goroutines

	store     *state.Store
	isolator  Isolator
	proxy     ProxyUpdater
	publisher EventPublisher
	attachReg *attachRegistry

	maxRestartAttempts int
	backoffFunc        func(attempt int) time.Duration
	sleep              func(time.Duration)
}

// NewManager constructs a Manager. proxy and publisher may be nil in
// tests that don't care about those side effects.
func NewManager(store *state.Store, iso Isolator, proxy ProxyUpdater, publisher EventPublisher) *Manager {
	return &Manager{
		handles:            make(map[string]Handle),
		epoch:              make(map[string]int),
		store:              store,
		isolator:           iso,
		proxy:              proxy,
		publisher:          publisher,
		attachReg:          newAttachRegistry(),
		maxRestartAttempts: DefaultMaxRestartAttempts,
		backoffFunc:        backoffDelay,
		sleep:              time.Sleep,
	}
}

func mapKey(namespace, name string) string { return namespace + "/" + name }

func effectiveRestartPolicy(p string) string {
	if p == "" {
		return "never"
	}
	return p
}

// isActive reports whether a sandbox in state s counts as "already
// running" for name-uniqueness (SPEC.md §7) and as a target for bulk
// Update/Attach.
func isActive(s state.SandboxState) bool {
	switch s {
	case state.StateRunning, state.StateReloadPending, state.StateRestarting:
		return true
	default:
		return false
	}
}

func (m *Manager) buildLaunchSpec(sb *state.Sandbox, env map[string]string) LaunchSpec {
	cmd := []string{sb.Agent}
	if sb.Agent == "" {
		cmd = []string{"/bin/sh"} // fallback; real agent command construction is a later (bwrap/cmd) concern
	}
	// LogPath failing to compute (only possible if os.UserHomeDir() fails,
	// e.g. $HOME unset with no XDG override) is not worth failing the
	// whole launch over — logs just won't capture anything for this
	// sandbox, same as if the platform genuinely has no home directory.
	logPath, _ := config.SandboxLogPath(sb.Namespace, sb.Name)
	return LaunchSpec{
		SandboxID: sb.ID,
		Mounts:    sb.Mounts,
		Tools:     sb.Tools,
		Env:       env,
		Cmd:       cmd,
		PTY:       true,
		LogPath:   logPath,
	}
}

// validateSandboxConfig re-runs config.ValidateProfile's tools:/mounts:
// collision check (DESIGN.md §10) against a sandbox's current or
// candidate mounts/tools, by adapting it into the *config.Profile shape
// ValidateProfile expects.
func validateSandboxConfig(sb *state.Sandbox) error {
	p := &config.Profile{
		Name:          sb.Name, // used only for the error message
		Mounts:        sb.Mounts,
		Tools:         sb.Tools,
		RestartPolicy: sb.RestartPolicy,
	}
	return config.ValidateProfile(p)
}

// setHandle registers h as the live handle for key, bumps its epoch, and
// returns the new epoch — callers pass this epoch into watchLoop so it can
// tell whether it's still the current watcher for that sandbox.
func (m *Manager) setHandle(key string, h Handle) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handles[key] = h
	m.epoch[key]++
	return m.epoch[key]
}

// clearHandle removes the live handle for key and bumps its epoch, so any
// in-flight watchLoop for the handle being cleared sees it's been
// superseded and exits without acting.
func (m *Manager) clearHandle(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.handles, key)
	m.epoch[key]++
}

func (m *Manager) getHandle(key string) (Handle, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.handles[key]
	return h, ok
}

// Run launches a new sandbox from profile, rejecting a duplicate
// active name in the same namespace (SPEC.md §7).
func (m *Manager) Run(profile *config.Profile, name, namespace string) (*state.Sandbox, error) {
	if namespace == "" {
		namespace = "default"
	}
	if existing, ok := m.store.Get(namespace, name); ok && isActive(existing.State) {
		return nil, fmt.Errorf("sandbox %s/%s is already active", namespace, name)
	}

	resolvedMounts, err := ResolveMounts(profile)
	if err != nil {
		return nil, err
	}

	id, err := state.NewID()
	if err != nil {
		return nil, err
	}

	sb := &state.Sandbox{
		ID:            id,
		Name:          name,
		Namespace:     namespace,
		Profile:       profile.Name,
		Agent:         profile.Agent,
		Mounts:        resolvedMounts,
		Tools:         profile.Tools,
		AllowURLs:     append([]string(nil), profile.AllowURLs...),
		RestartPolicy: effectiveRestartPolicy(profile.RestartPolicy),
		State:         state.StateRunning,
		StartedAt:     time.Now(),
	}

	handle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(sb, profile.Env))
	if err != nil {
		return nil, fmt.Errorf("launch sandbox %s/%s: %w", namespace, name, err)
	}
	captureHandleInfo(sb, handle)

	if err := m.store.Put(sb); err != nil {
		_ = m.isolator.Stop(handle)
		return nil, err
	}

	key := mapKey(namespace, name)
	epoch := m.setHandle(key, handle)

	if m.proxy != nil {
		m.proxy.SetAllowlist(sb.ID, sb.AllowURLs)
		registerHandleNetworkAddr(m.proxy, sb.ID, handle)
	}
	if m.publisher != nil {
		_ = m.publisher.PublishStatus(namespace, name, "started")
	}

	go m.watchLoop(namespace, name, handle, epoch)

	return sb, nil
}

func (m *Manager) matchSelector(sel Selector) ([]*state.Sandbox, error) {
	switch {
	case sel.Name != "":
		ns := sel.Namespace
		if ns == "" {
			ns = "default"
		}
		sb, ok := m.store.Get(ns, sel.Name)
		if !ok {
			return nil, fmt.Errorf("sandbox %s/%s not found", ns, sel.Name)
		}
		return []*state.Sandbox{sb}, nil

	case sel.All:
		return m.runningSandboxes(sel.Namespace), nil

	case sel.Profile != "":
		var out []*state.Sandbox
		for _, sb := range m.runningSandboxes(sel.Namespace) {
			if sb.Profile == sel.Profile {
				out = append(out, sb)
			}
		}
		return out, nil

	default:
		return nil, fmt.Errorf("update: selector must set Name, Profile, or All")
	}
}

func (m *Manager) runningSandboxes(namespace string) []*state.Sandbox {
	var out []*state.Sandbox
	for _, sb := range m.store.List(namespace) {
		if isActive(sb.State) {
			out = append(out, sb)
		}
	}
	return out
}

// Update applies delta to every sandbox matched by sel, atomically
// (DESIGN.md §11): the delta is validated against every matched sandbox
// before being applied to any of them, so one invalid target rejects the
// whole batch with zero side effects rather than leaving some sandboxes
// changed and others not.
func (m *Manager) Update(sel Selector, delta ConfigDelta) ([]UpdateResult, error) {
	targets, err := m.matchSelector(sel)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("update: no matching running sandboxes")
	}

	// Phase 1: build and validate every candidate before touching anything.
	candidates := make([]*state.Sandbox, len(targets))
	for i, sb := range targets {
		candidate := *sb
		candidate.Mounts = mergeMounts(sb.Mounts, delta.AddMounts)
		candidate.AllowURLs = applyURLDelta(sb.AllowURLs, delta.AllowURLs, delta.DenyURLs)
		if err := validateSandboxConfig(&candidate); err != nil {
			return nil, fmt.Errorf("update rejected, no changes applied to any sandbox: %s/%s: %w", sb.Namespace, sb.Name, err)
		}
		candidates[i] = &candidate
	}

	// Phase 2: every candidate validated — apply to all of them.
	results := make([]UpdateResult, 0, len(candidates))
	for _, candidate := range candidates {
		key := mapKey(candidate.Namespace, candidate.Name)
		applied := true

		if len(delta.AddMounts) > 0 {
			if h, ok := m.getHandle(key); ok {
				a, err := m.isolator.UpdateMounts(h, candidate.Mounts)
				if err != nil {
					return nil, fmt.Errorf("apply mounts to sandbox %s: %w", key, err)
				}
				applied = a
			} else {
				applied = false
			}
		}

		if !applied {
			candidate.State = state.StateReloadPending
		} else if candidate.State == state.StateReloadPending {
			candidate.State = state.StateRunning
		}
		if err := m.store.Put(candidate); err != nil {
			return nil, err
		}
		if m.proxy != nil && (len(delta.AllowURLs) > 0 || len(delta.DenyURLs) > 0) {
			m.proxy.SetAllowlist(candidate.ID, candidate.AllowURLs)
		}

		results = append(results, UpdateResult{Namespace: candidate.Namespace, Name: candidate.Name, Applied: applied})
	}
	return results, nil
}

// Reload re-attempts applying a sandbox's already-stored config live
// (DESIGN.md §6.3/§9) — a no-op, not an error, if nothing is pending.
func (m *Manager) Reload(namespace, name string) error {
	live, ok := m.store.Get(namespace, name)
	if !ok {
		return fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	if live.State != state.StateReloadPending {
		return nil
	}
	// Store.Get returns the live pointer in its map; copy before mutating
	// so we never write fields on an object another goroutine might be
	// reading concurrently (a bare Put back of the same pointer would race
	// against e.g. a concurrent muro status read).
	sb := *live

	h, ok := m.getHandle(mapKey(namespace, name))
	if !ok {
		return fmt.Errorf("sandbox %s/%s has no live process to reload", namespace, name)
	}

	applied, err := m.isolator.UpdateMounts(h, sb.Mounts)
	if err != nil {
		return err
	}
	if applied {
		sb.State = state.StateRunning
	}
	return m.store.Put(&sb)
}

// Restart stops and relaunches a sandbox with its current stored config,
// force-detaching any existing attach session first (DESIGN.md §12): a
// restart re-execs the agent behind a brand-new pty, so an existing
// session is stale the moment restart runs.
func (m *Manager) Restart(namespace, name string) error {
	live, ok := m.store.Get(namespace, name)
	if !ok {
		return fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	sb := *live // copy before mutating — see Reload's comment

	key := mapKey(namespace, name)
	m.attachReg.Detach(key)

	if h, ok := m.getHandle(key); ok {
		m.clearHandle(key) // bump epoch before the kill so the old watchLoop sees itself superseded
		if err := m.isolator.Stop(h); err != nil {
			return err
		}
	}

	handle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(&sb, nil))
	if err != nil {
		sb.State = state.StateCrashed
		_ = m.store.Put(&sb)
		return fmt.Errorf("restart sandbox %s/%s: %w", namespace, name, err)
	}

	captureHandleInfo(&sb, handle)
	sb.State = state.StateRunning
	if err := m.store.Put(&sb); err != nil {
		return err
	}

	// A restart gets a brand-new bwrap process, hence a brand-new Stage 2
	// network bridge and outbound address — re-register it even though the
	// allowlist rule itself (keyed by the stable sb.ID, not the address)
	// doesn't need re-setting.
	registerHandleNetworkAddr(m.proxy, sb.ID, handle)

	epoch := m.setHandle(key, handle)
	if m.publisher != nil {
		_ = m.publisher.PublishStatus(namespace, name, "restarted")
	}

	go m.watchLoop(namespace, name, handle, epoch)
	return nil
}

// Stop stops a sandbox's process (if any) and marks it StateStopped. The
// state transition is written before the process is actually killed so
// the in-flight watchLoop, once it wakes from Wait(), sees the intentional
// stop and doesn't treat it as a crash.
func (m *Manager) Stop(namespace, name string) error {
	live, ok := m.store.Get(namespace, name)
	if !ok {
		return fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	sb := *live // copy before mutating — see Reload's comment

	key := mapKey(namespace, name)
	m.attachReg.Detach(key)

	sb.State = state.StateStopped
	if err := m.store.Put(&sb); err != nil {
		return err
	}

	if h, ok := m.getHandle(key); ok {
		m.clearHandle(key)
		if err := m.isolator.Stop(h); err != nil {
			return err
		}
	}
	if m.publisher != nil {
		_ = m.publisher.PublishStatus(namespace, name, "stopped")
	}
	return nil
}

// Attach claims exclusive interactive access to a running sandbox's pty
// (DESIGN.md §12). The returned detach func must be called when the
// caller is done (or the terminal disconnects) to release the slot.
func (m *Manager) Attach(namespace, name string) (io.ReadWriteCloser, func(), error) {
	sb, ok := m.store.Get(namespace, name)
	if !ok {
		return nil, nil, fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	if !isActive(sb.State) {
		return nil, nil, fmt.Errorf("sandbox %s/%s is not running", namespace, name)
	}

	key := mapKey(namespace, name)
	already, since := m.attachReg.TryAttach(key)
	if already {
		return nil, nil, fmt.Errorf("sandbox %s/%s already attached (since %s)", namespace, name, since.Format(time.RFC3339))
	}

	h, ok := m.getHandle(key)
	if !ok {
		m.attachReg.Detach(key)
		return nil, nil, fmt.Errorf("sandbox %s/%s has no live process to attach to", namespace, name)
	}
	pty, ok := h.Stdio()
	if !ok {
		m.attachReg.Detach(key)
		return nil, nil, fmt.Errorf("sandbox %s/%s was not launched with a pty", namespace, name)
	}

	detach := func() { m.attachReg.Detach(key) }
	return pty, detach, nil
}

// watchLoop waits on h's exit and applies sb's restart_policy
// (DESIGN.md §13). epoch must match Manager's current epoch for this
// sandbox's key at the moment h exits, or this watchLoop has been
// superseded by an explicit Stop/Restart (which already owns whatever
// state transition applies) and it exits without acting.
func (m *Manager) watchLoop(namespace, name string, h Handle, epoch int) {
	exitCode, waitErr := h.Wait()
	cleanExit := waitErr == nil && exitCode == 0

	key := mapKey(namespace, name)
	m.mu.Lock()
	current := m.epoch[key]
	m.mu.Unlock()
	if current != epoch {
		return
	}

	live, ok := m.store.Get(namespace, name)
	if !ok {
		return
	}
	// sb is this goroutine's own private working copy — Store.Get handed
	// back its live map pointer, and we must never mutate that in place
	// (another goroutine, e.g. a concurrent muro status read, could hold
	// the same pointer). Every store.Put below publishes a distinct fresh
	// copy of sb and is never mutated again afterwards, so a pointer
	// handed to Put is effectively immutable from the instant it's handed
	// over.
	sb := *live

	if shouldRestart(sb.RestartPolicy, sb.RestartCount, m.maxRestartAttempts, cleanExit) {
		sb.RestartCount++
		sb.State = state.StateRestarting
		restarting := sb
		_ = m.store.Put(&restarting)
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "restarting")
		}

		m.sleep(m.backoffFunc(sb.RestartCount))

		newHandle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(&sb, nil))
		if err != nil {
			sb.State = state.StateCrashed
			crashed := sb
			_ = m.store.Put(&crashed)
			if m.publisher != nil {
				_ = m.publisher.PublishStatus(namespace, name, "crashed")
			}
			m.clearHandle(key)
			return
		}

		captureHandleInfo(&sb, newHandle)
		sb.State = state.StateRunning
		running := sb
		_ = m.store.Put(&running)
		registerHandleNetworkAddr(m.proxy, sb.ID, newHandle) // new process, new Stage 2 bridge/address
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "restarted")
		}

		newEpoch := m.setHandle(key, newHandle)
		m.watchLoop(namespace, name, newHandle, newEpoch) // one watcher per sandbox lifetime, tail-called rather than a fresh goroutine
		return
	}

	m.clearHandle(key)
	if sb.RestartPolicy == "on-failure" && !cleanExit {
		sb.State = state.StateRestartExhausted
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "restart-exhausted")
		}
	} else {
		sb.State = state.StateCrashed
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "crashed")
		}
	}
	final := sb
	_ = m.store.Put(&final)
}
