package sandbox

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/state"
	"github.com/thomkin/muro/internal/worktree"
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

	// InjectSocket exposes the pty-injection socket path (shim.go) a
	// freshly-Launched Handle's shim listens on — see
	// state.Sandbox.InjectSocket's doc comment for why this is persisted
	// but, unlike ShimSocket/SlirpPID, deliberately NOT threaded through
	// Reattacher: a Reattach-reconstructed Handle returns "" here, and
	// captureHandleInfo is never called on that path, so the value from
	// the sandbox's original Launch survives untouched across a murod
	// restart.
	InjectSocket() string
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
		sb.InjectSocket = si.InjectSocket()
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
// process, and registers it exactly as Run would (proxy allowlist,
// network address, and — since a previously out-of-scope gap was closed
// here — a watchLoop resuming restart_policy tracking from sb's current,
// already-persisted RestartCount). This is what lets `muro sandbox
// attach`/`stop` keep working against a sandbox that outlived the murod
// process that originally launched it, AND what makes a crash after the
// restart still get restart_policy applied instead of silently going
// unwatched. watchLoop reads RestartCount fresh from the Store at the
// moment the process actually exits (not from sb here), so it's already
// continuous across a restart with no extra propagation needed — sb's
// RestartCount was itself loaded from state.json at startup, which
// watchLoop's own Store.Put calls kept correctly persisted all along.
func (m *Manager) Reattach(sb *state.Sandbox) error {
	ra, ok := m.isolator.(Reattacher)
	if !ok {
		return fmt.Errorf("isolator does not support reattaching to an already-running sandbox")
	}
	// This process's own agent-socket listener for sb doesn't exist yet —
	// the previous murod's died along with that process, even though the
	// sandbox (and its bwrap-side mount at the same host path) survived.
	m.startAgentBridge(sb)
	m.startToolBridge(sb)
	h, err := ra.Reattach(sb.PID, sb.ShimSocket, sb.SlirpPID, sb.NetAddr)
	if err != nil {
		return fmt.Errorf("reattach sandbox %s/%s: %w", sb.Namespace, sb.Name, err)
	}
	epoch := m.setHandle(mapKey(sb.Namespace, sb.Name), h)
	if m.proxy != nil {
		m.proxy.SetAllowlist(sb.ID, sb.AllowURLs)
		if sb.NetAddr != "" {
			m.proxy.RegisterSandboxAddr(sb.ID, sb.NetAddr)
		}
	}
	go m.watchLoop(sb.Namespace, sb.Name, h, epoch)
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

	// pubStateDir/pubPublisher/pubSubscriber configure the MQTT
	// agent-to-agent bridge (DESIGN.md §8) — all zero/nil until
	// EnablePubsub is called, which most callers (nearly the whole
	// existing test suite) simply never do, leaving the bridge fully
	// disabled with no behavior change from before it existed.
	pubStateDir   string
	pubPublisher  PubsubPublisher
	pubSubscriber PubsubSubscriber

	pubMu           sync.Mutex
	agentSockets    map[string]*agentSocketServer // key: sandboxID (state.Sandbox.ID)
	inboxSubscribed map[string]bool               // key: mapKey(namespace, name)

	// toolSockets holds the git tool-proxy's per-sandbox listeners
	// (toolsocket.go) — unlike agentSockets, there is no "enabled" gate at
	// the Manager level: a listener is started per-sandbox purely based on
	// whether that sandbox's own GitPolicy has any repos configured
	// (startToolBridge), since (unlike pub/sub) there's no broker
	// connection this depends on.
	toolMu                      sync.Mutex
	toolSockets                 map[string]*toolSocketServer // key: sandboxID
	daemonGitAllowedSubcommands []string
	// stateDir is murod's XDG state directory — used to compute
	// ToolSocketPath (independent of pubStateDir/EnablePubsub, since the
	// git tool-proxy has nothing to do with pub/sub and must not silently
	// stop working just because MQTT isn't configured) and, more generally
	// now, anywhere a sandbox needs its own private on-disk area (private
	// directories, PrivateDirMounts). Set via SetStateDir.
	stateDir string

	maxRestartAttempts int
	backoffFunc        func(attempt int) time.Duration
	sleep              func(time.Duration)
}

// NewManager constructs a Manager. proxy and publisher may be nil in
// tests that don't care about those side effects.
func NewManager(store *state.Store, iso Isolator, proxy ProxyUpdater, publisher EventPublisher) *Manager {
	return &Manager{
		handles:         make(map[string]Handle),
		epoch:           make(map[string]int),
		store:           store,
		isolator:        iso,
		proxy:           proxy,
		publisher:       publisher,
		attachReg:       newAttachRegistry(),
		agentSockets:    make(map[string]*agentSocketServer),
		inboxSubscribed: make(map[string]bool),
		toolSockets:     make(map[string]*toolSocketServer),
		// Defaulted here (not left nil) so tests that exercise a git
		// policy without ever calling SetGitPolicy still behave like
		// cmd/murod would with an unconfigured daemon.yaml — mirrors
		// config.DefaultDaemonConfig's own default exactly, via that same
		// function, so the two can't drift apart.
		daemonGitAllowedSubcommands: config.DefaultDaemonConfig().GitPolicy.AllowedSubcommands,
		maxRestartAttempts:          DefaultMaxRestartAttempts,
		backoffFunc:                 backoffDelay,
		sleep:                       time.Sleep,
	}
}

// EnablePubsub turns on the MQTT agent-to-agent bridge (DESIGN.md §8) for
// every sandbox this Manager subsequently launches or reattaches — optional
// because most callers (nearly the entire existing test suite) have no
// broker to connect to and never call this, leaving the whole bridge inert.
// pub and sub may independently be nil (e.g. pub/sub configured but the
// broker is currently unreachable): each sandbox's agent socket still
// starts and its inbox subscription is still skipped/attempted the same
// way, just with `muro pubsub publish` getting a clear "broker not
// connected" response instead of the mount silently not existing. Must be
// called once, before Run/ReattachAll — stateDir can't sensibly change
// mid-flight since it's already baked into every currently-running
// sandbox's already-mounted AgentSocketPath. cmd/murod is the only real
// caller.
func (m *Manager) EnablePubsub(stateDir string, pub PubsubPublisher, sub PubsubSubscriber) {
	m.pubStateDir = stateDir
	m.pubPublisher = pub
	m.pubSubscriber = sub
}

func (m *Manager) pubsubEnabled() bool { return m.pubStateDir != "" }

// startAgentBridge brings up sb's half of the MQTT bridge: its outbound
// agent-socket listener (idempotent per sb.ID) and, once per namespace/name
// for this Manager's lifetime, an inbox subscription that injects arriving
// messages into whatever sandbox is currently live at that address (looked
// up fresh from the Store at delivery time, not captured here — see
// state.Sandbox.InjectSocket's doc comment for why that's safe across
// Restart/a murod restart). A no-op if EnablePubsub was never called.
// Called from Run (before Launch, so the listener is guaranteed up before
// the sandboxed process could possibly connect — the exact race
// AgentSocketPath's doc comment already establishes for buildLaunchSpec)
// and from Reattach (where the sandbox is already running, but this
// process's own listener for it does not exist yet — the previous murod's
// died with that process).
func (m *Manager) startAgentBridge(sb *state.Sandbox) {
	if !m.pubsubEnabled() {
		return
	}
	m.pubMu.Lock()
	defer m.pubMu.Unlock()

	if _, ok := m.agentSockets[sb.ID]; !ok {
		path := AgentSocketPath(m.pubStateDir, sb.ID)
		if srv, err := startAgentSocket(path, sb.Namespace, m.pubPublisher); err == nil {
			m.agentSockets[sb.ID] = srv
		}
		// A listen failure here is not escalated to a launch failure —
		// matches this codebase's established tolerance for a
		// partially-degraded optional subsystem (nil proxy/publisher
		// elsewhere): the sandbox still runs, just without a working
		// outbound bridge.
	}

	key := mapKey(sb.Namespace, sb.Name)
	if m.pubSubscriber != nil && !m.inboxSubscribed[key] {
		namespace, name := sb.Namespace, sb.Name
		err := m.pubSubscriber.SubscribeInbox(namespace, name, func(message []byte) {
			if live, ok := m.store.Get(namespace, name); ok {
				injectMessage(live.InjectSocket, message)
			}
		})
		if err == nil {
			m.inboxSubscribed[key] = true
		}
	}
}

// stopAgentBridge tears down sandboxID's agent-socket listener (Stop's
// final-teardown path only — Restart and watchLoop's crash-relaunch reuse
// the same sandboxID and therefore the same AgentSocketPath, so the
// existing listener keeps serving the freshly-Launched sandbox's new mount
// unchanged and must NOT be stopped there). The inbox MQTT subscription is
// deliberately left in place: it's keyed by namespace/name rather than
// sandboxID, and a later Run reusing that namespace/name (a fresh sb.ID)
// still wants messages delivered — startAgentBridge's handler always
// re-reads InjectSocket from the Store at delivery time, so it naturally
// picks up whichever sandbox is current.
func (m *Manager) stopAgentBridge(sandboxID string) {
	m.pubMu.Lock()
	defer m.pubMu.Unlock()
	if srv, ok := m.agentSockets[sandboxID]; ok {
		srv.stop()
		delete(m.agentSockets, sandboxID)
	}
}

// SetGitPolicy configures the daemon-global ceiling for the git tool-proxy
// (DaemonConfig.GitPolicy.AllowedSubcommands, config.go). cmd/murod is the
// only real caller; tests that don't use the git tool-proxy never need to
// call this — NewManager already seeds a sensible built-in default.
func (m *Manager) SetGitPolicy(daemonAllowedSubcommands []string) {
	m.toolMu.Lock()
	defer m.toolMu.Unlock()
	m.daemonGitAllowedSubcommands = daemonAllowedSubcommands
}

// SetStateDir records murod's XDG state directory for computing
// ToolSocketPath (bwrap.go) — deliberately independent of EnablePubsub's
// stateDir parameter, even though in practice cmd/murod passes the same
// directory to both: the git tool-proxy has nothing to do with pub/sub and
// must keep working whether or not an MQTT broker is configured.
// cmd/murod is the only real caller; a sandbox with no git policy never
// needs this to have been called.
func (m *Manager) SetStateDir(stateDir string) {
	m.toolMu.Lock()
	defer m.toolMu.Unlock()
	m.stateDir = stateDir
}

// startToolBridge brings up sb's git tool-proxy listener (toolsocket.go),
// idempotent per sb.ID — a no-op if sb.GitPolicy has no repos configured,
// matching bwrap.go's "absence of policy means the stub isn't even
// mounted" convention: no listener, nothing to dial, nothing to shadow.
// Called from Run (before Launch — same "listener up before the mount
// could possibly be dialed" reasoning as startAgentBridge) and from
// Reattach (this process's own listener for sb doesn't exist yet).
func (m *Manager) startToolBridge(sb *state.Sandbox) {
	if len(sb.GitPolicy.Repos) == 0 {
		return
	}
	m.toolMu.Lock()
	defer m.toolMu.Unlock()
	if _, ok := m.toolSockets[sb.ID]; ok {
		return
	}
	path := ToolSocketPath(m.stateDir, sb.ID)
	if srv, err := startToolSocket(path, sb.Mounts, sb.GitPolicy, m.daemonGitAllowedSubcommands); err == nil {
		m.toolSockets[sb.ID] = srv
	}
	// A listen failure here is not escalated to a launch failure, matching
	// this codebase's established tolerance for a partially-degraded
	// optional subsystem — but note Launch will still fail separately if
	// spec.ToolSocketPath ends up empty-but-expected is never the case
	// here: buildLaunchSpec sets ToolSocketPath from the same len(Repos)>0
	// condition regardless of whether the listener actually started, so a
	// listen failure here means the sandbox launches with a mounted socket
	// nothing is serving — a clear "connection refused" from the stub
	// rather than a silent hang.
}

// stopToolBridge tears down sandboxID's tool-socket listener (Stop's final
// teardown path only — Restart/watchLoop's crash-relaunch reuse the same
// sandboxID and therefore the same ToolSocketPath, so the existing
// listener keeps serving the freshly-Launched sandbox's new mount
// unchanged and must NOT be stopped there — identical reasoning to
// stopAgentBridge).
func (m *Manager) stopToolBridge(sandboxID string) {
	m.toolMu.Lock()
	defer m.toolMu.Unlock()
	if srv, ok := m.toolSockets[sandboxID]; ok {
		srv.stop()
		delete(m.toolSockets, sandboxID)
	}
}

func mapKey(namespace, name string) string { return namespace + "/" + name }

func effectiveRestartPolicy(p string) string {
	if p == "" {
		return "never"
	}
	return p
}

// resolvedProfileFields is everything a state.Sandbox needs derived from a
// config.Profile — shared by Run (a brand-new sandbox) and
// Restart(fromProfile=true) (an existing one picking up a profile edit),
// so the two can never drift on how a profile turns into a running
// sandbox's config.
type resolvedProfileFields struct {
	AgentArgs     []string
	Env           map[string]string
	Mounts        []config.Mount
	Tools         []config.Tool
	AllowURLs     []string
	RestartPolicy string
	GitPolicy     config.GitPolicy
	Audio         bool
	PrivateDirs   []string
	Worktrees     []state.WorktreeInfo
	WorkDir       string
	QuietMode     bool
}

// resolveSandboxFieldsFromProfile resolves profile's mounts (including
// audio-passthrough sockets, if enabled) and copies its other
// sandbox-relevant fields. Audio failing to resolve fails this outright
// rather than silently degrading — unlike pub/sub (a nice-to-have bridge),
// the entire reason a profile sets Audio is "I need this to actually
// work," so finding out now beats an STT tool silently getting nothing.
func resolveSandboxFieldsFromProfile(profile *config.Profile, namespace, name, stateDir, sandboxID string) (resolvedProfileFields, error) {
	mounts, err := ResolveMounts(profile)
	if err != nil {
		return resolvedProfileFields{}, err
	}
	bundleMounts, berr := BundleDocsMounts(profile.Name)
	if berr != nil {
		return resolvedProfileFields{}, fmt.Errorf("resolve bundle docs for sandbox %s/%s: %w", namespace, name, berr)
	}
	mounts = append(mounts, bundleMounts...)
	if profile.Audio {
		audioMounts, aerr := AudioMounts(os.Getenv("XDG_RUNTIME_DIR"))
		if aerr != nil {
			return resolvedProfileFields{}, fmt.Errorf("resolve audio passthrough for sandbox %s/%s: %w", namespace, name, aerr)
		}
		mounts = append(mounts, audioMounts...)
	}
	if len(profile.PrivateDirs) > 0 {
		expandedPrivateDirs := make([]string, len(profile.PrivateDirs))
		for i, pd := range profile.PrivateDirs {
			expandedPrivateDirs[i] = config.ExpandHome(pd)
		}
		privateMounts, perr := PrivateDirMounts(stateDir, sandboxID, expandedPrivateDirs)
		if perr != nil {
			return resolvedProfileFields{}, fmt.Errorf("resolve private dirs for sandbox %s/%s: %w", namespace, name, perr)
		}
		mounts = append(mounts, privateMounts...)
	}
	if profile.Instructions != "" || len(profile.Skills) > 0 {
		hostHome, herr := os.UserHomeDir()
		if herr != nil {
			return resolvedProfileFields{}, fmt.Errorf("resolve instructions/skills for sandbox %s/%s: determine host home dir: %w", namespace, name, herr)
		}
		if instrMount, ierr := InstructionsMount(hostHome, config.ExpandHome(profile.Instructions)); ierr != nil {
			return resolvedProfileFields{}, fmt.Errorf("resolve instructions for sandbox %s/%s: %w", namespace, name, ierr)
		} else if instrMount != nil {
			mounts = append(mounts, *instrMount)
		}
		expandedSkills := make([]string, len(profile.Skills))
		for i, sk := range profile.Skills {
			expandedSkills[i] = config.ExpandHome(sk)
		}
		skillMounts, serr := SkillMounts(hostHome, expandedSkills)
		if serr != nil {
			return resolvedProfileFields{}, fmt.Errorf("resolve skills for sandbox %s/%s: %w", namespace, name, serr)
		}
		mounts = append(mounts, skillMounts...)
	}
	gitPolicy := cloneGitPolicy(profile.Git)
	worktreeMounts, effectiveWorktreeRepos, worktrees, werr := WorktreeMounts(
		context.Background(), stateDir, sandboxID, namespace, name, profile.Git.Repos)
	if werr != nil {
		return resolvedProfileFields{}, fmt.Errorf("resolve worktrees for sandbox %s/%s: %w", namespace, name, werr)
	}
	mounts = append(mounts, worktreeMounts...)
	gitPolicy.Repos = append(gitPolicy.Repos, effectiveWorktreeRepos...)

	envCopy := make(map[string]string, len(profile.Env))
	for k, v := range profile.Env {
		envCopy[k] = v
	}
	return resolvedProfileFields{
		AgentArgs:     append([]string(nil), profile.AgentArgs...),
		Env:           envCopy,
		Mounts:        mounts,
		Tools:         profile.Tools,
		AllowURLs:     append([]string(nil), profile.AllowURLs...),
		RestartPolicy: effectiveRestartPolicy(profile.RestartPolicy),
		GitPolicy:     gitPolicy,
		Audio:         profile.Audio,
		PrivateDirs:   append([]string(nil), profile.PrivateDirs...),
		Worktrees:     worktrees,
		WorkDir:       profile.WorkDir,
		QuietMode:     profile.QuietMode,
	}, nil
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

// quietChatSandboxPath is where cmd/muro-quiet-chat is expected to be
// installed on the host (scripts/install.sh, alongside the real `claude`
// binary) — a "~"-prefixed path expanded the same way every other
// ~/.claude*/~/.local mount already is (config.ExpandHome), so it resolves
// to the identical path inside the sandbox once ~/.local is mounted.
const quietChatSandboxPath = "~/.local/bin/muro-quiet-chat"

func (m *Manager) buildLaunchSpec(sb *state.Sandbox) LaunchSpec {
	agentArgs := make([]string, len(sb.AgentArgs))
	for i, a := range sb.AgentArgs {
		// SessionIDTemplateToken substitution: agent_args are exec'd
		// directly with no shell in between, so a literal "$MURO_SESSION_ID"
		// reference would never expand — this is the one place that
		// actually happens.
		agentArgs[i] = strings.ReplaceAll(a, SessionIDTemplateToken, sb.SessionID)
	}
	cmd := append([]string{sb.Agent}, agentArgs...)
	if sb.Agent == "" {
		cmd = []string{"/bin/sh"} // fallback; real agent command construction is a later (bwrap/cmd) concern
	}
	if sb.QuietMode {
		// QuietMode replaces the configured Agent/AgentArgs entirely — the
		// wrapper always drives Claude Code itself via print mode (see
		// cmd/muro-quiet-chat and config.Profile.QuietMode's doc comment).
		// It's installed alongside the real `claude` binary under
		// ~/.local/bin, which every claude-base-derived profile already
		// mounts read-only at the identical sandbox path, so no separate
		// mount is needed here.
		cmd = []string{config.ExpandHome(quietChatSandboxPath)}
	}
	// LogPath failing to compute (only possible if os.UserHomeDir() fails,
	// e.g. $HOME unset with no XDG override) is not worth failing the
	// whole launch over — logs just won't capture anything for this
	// sandbox, same as if the platform genuinely has no home directory.
	logPath, _ := config.SandboxLogPath(sb.Namespace, sb.Name)
	spec := LaunchSpec{
		SandboxID: sb.ID,
		Mounts:    sb.Mounts,
		Tools:     sb.Tools,
		Env:       sb.Env,
		Cmd:       cmd,
		PTY:       true,
		LogPath:   logPath,
		WorkDir:   sb.WorkDir,
	}
	if m.pubsubEnabled() {
		spec.AgentSocketPath = AgentSocketPath(m.pubStateDir, sb.ID)
	}
	if len(sb.GitPolicy.Repos) > 0 {
		spec.ToolSocketPath = ToolSocketPath(m.stateDir, sb.ID)
	}
	if sb.Audio {
		spec.AudioRuntimeDir = os.Getenv("XDG_RUNTIME_DIR")
	}
	spec.SessionID = sb.SessionID
	return spec
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

	id, err := state.NewID()
	if err != nil {
		return nil, err
	}
	sessionID, err := NewSessionID()
	if err != nil {
		return nil, err
	}

	fields, err := resolveSandboxFieldsFromProfile(profile, namespace, name, m.stateDir, id)
	if err != nil {
		return nil, err
	}

	sb := &state.Sandbox{
		ID:            id,
		Name:          name,
		Namespace:     namespace,
		Profile:       profile.Name,
		Agent:         profile.Agent,
		AgentArgs:     fields.AgentArgs,
		Env:           fields.Env,
		Mounts:        fields.Mounts,
		Tools:         fields.Tools,
		AllowURLs:     fields.AllowURLs,
		RestartPolicy: fields.RestartPolicy,
		State:         state.StateRunning,
		StartedAt:     time.Now(),
		GitPolicy:     fields.GitPolicy,
		Audio:         fields.Audio,
		SessionID:     sessionID,
		PrivateDirs:   fields.PrivateDirs,
		Worktrees:     fields.Worktrees,
		WorkDir:       fields.WorkDir,
		QuietMode:     fields.QuietMode,
	}

	// Started BEFORE Launch, not after: the listener must already be up
	// the instant bwrap mounts its host path in, or a fast-starting
	// sandboxed process could dial before anything is listening — see
	// startAgentBridge's own doc comment.
	m.startAgentBridge(sb)
	m.startToolBridge(sb)

	handle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(sb))
	if err != nil {
		m.stopAgentBridge(sb.ID)
		m.stopToolBridge(sb.ID)
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
func (m *Manager) Restart(namespace, name string, fromProfile bool) error {
	live, ok := m.store.Get(namespace, name)
	if !ok {
		return fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	sb := *live // copy before mutating — see Reload's comment

	// fromProfile is `muro sandbox restart --from-profile`: the only way to
	// get an edited profile's changes (mounts, tools, allow_urls, agent,
	// git policy, audio) into an already-running sandbox without stopping
	// it and launching a brand-new one under a fresh ID. Without this flag,
	// restart keeps its original meaning: relaunch with whatever this
	// sandbox already has stored (its own prior mounts, or whatever `muro
	// sandbox update` staged), unrelated to the profile file's current
	// content.
	if fromProfile {
		profile, err := config.LoadProfile(sb.Profile)
		if err != nil {
			return fmt.Errorf("reload profile %q for restart: %w", sb.Profile, err)
		}
		fields, err := resolveSandboxFieldsFromProfile(profile, namespace, name, m.stateDir, sb.ID)
		if err != nil {
			return err
		}
		sb.Agent = profile.Agent
		sb.AgentArgs = fields.AgentArgs
		sb.Env = fields.Env
		sb.Mounts = fields.Mounts
		sb.Tools = fields.Tools
		sb.AllowURLs = fields.AllowURLs
		sb.RestartPolicy = fields.RestartPolicy
		sb.GitPolicy = fields.GitPolicy
		sb.Audio = fields.Audio
		sb.PrivateDirs = fields.PrivateDirs
		sb.Worktrees = fields.Worktrees
		sb.WorkDir = fields.WorkDir
		sb.QuietMode = fields.QuietMode
		// sb.SessionID is deliberately left untouched here — it's a stable
		// per-sandbox-INSTANCE identity (same ID = same session), not
		// something a profile edit should ever change.

		// The tool-socket listener (if any) was started once with the OLD
		// mounts/GitPolicy captured in its own fields — startToolBridge is
		// a no-op if one already exists for this sandbox ID, so a stale
		// listener would otherwise keep enforcing the pre-edit git policy
		// forever. Tear it down so the startToolBridge call below (after
		// sb's fields are updated, before Launch — same timing rule Run
		// already follows) creates a fresh one with the current values.
		m.stopToolBridge(sb.ID)
	}

	key := mapKey(namespace, name)
	m.attachReg.Detach(key)

	if h, ok := m.getHandle(key); ok {
		m.clearHandle(key) // bump epoch before the kill so the old watchLoop sees itself superseded
		if err := m.isolator.Stop(h); err != nil {
			return err
		}
	}

	if fromProfile {
		m.startToolBridge(&sb)
	}

	handle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(&sb))
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
	// network bridge and outbound address — re-register it. The allowlist
	// rule is keyed by the stable sb.ID, not the address, so it doesn't
	// need updating for the address change itself — but it DOES need
	// re-setting regardless, because this call is the only thing that
	// (re)populates it in THIS murod process's in-memory proxy state. If
	// murod itself restarted since this sandbox was last (Re)launched,
	// ReattachAll's own SetAllowlist call is what would normally have
	// restored it — but that only runs for sandboxes state.Reconcile found
	// still StateRunning at that exact moment; anything that raced past
	// that check (or whose Reattach itself failed, logged but not fatal)
	// starts this murod process with an empty allowlist entry for that ID,
	// and every one of its requests gets silently denied by the
	// fail-closed proxy from then on — confirmed as a real, reproduced bug
	// (`journalctl --user -u murod`, repeated "proxy: denied ... host=api.
	// anthropic.com" entries for a profile whose allow_urls plainly
	// includes it) rather than a hypothetical. Setting it here unconditionally
	// is idempotent and cheap, and closes that gap for good instead of
	// relying on ReattachAll always having won the race.
	if m.proxy != nil {
		m.proxy.SetAllowlist(sb.ID, sb.AllowURLs)
	}
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
	m.stopAgentBridge(sb.ID)
	m.stopToolBridge(sb.ID)

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

// Delete permanently removes a sandbox's record (state.Store.Delete), its
// captured log file, and any private directories it owns (PrivateDirs,
// RemovePrivateDirs) — the confirmation prompt for this is a CLI concern
// (internal/cli), not enforced here. Refuses a still-active sandbox (must
// be stopped/crashed/restart-exhausted first, same as the rest of this
// package's active-state checks) so `delete` never doubles as an implicit
// stop.
// Delete permanently removes a stopped sandbox's record, log, and private
// data. discardWorktrees names the MountPath of every worktree (DESIGN.md
// §15) whose unmerged commits the caller has explicitly accepted losing
// (`muro sandbox delete --discard-worktree <mount_path>`) — any worktree
// NOT listed there that still has commits unmerged into its base branch
// causes the WHOLE delete to be refused up front (nothing is deleted, the
// same "refuse outright, don't partially apply" posture the active-sandbox
// check above already has), since the existing --yes confirmation only
// covers deleting metadata/logs, never discarding real, unmerged code.
func (m *Manager) Delete(namespace, name string, discardWorktrees []string) error {
	sb, ok := m.store.Get(namespace, name)
	if !ok {
		return fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}
	if isActive(sb.State) {
		return fmt.Errorf("sandbox %s/%s is still active (state %q) — stop it first", namespace, name, sb.State)
	}

	discard := make(map[string]bool, len(discardWorktrees))
	for _, mp := range discardWorktrees {
		discard[mp] = true
	}
	for _, wt := range sb.Worktrees {
		if discard[wt.MountPath] {
			continue
		}
		has, err := worktree.HasUnmergedCommits(context.Background(), wt.Host, wt.BaseBranch)
		if err != nil {
			return fmt.Errorf("sandbox %s/%s: check worktree %q for unmerged commits: %w", namespace, name, wt.MountPath, err)
		}
		if has {
			return fmt.Errorf("sandbox %s/%s has unmerged commits on branch %q (mount %q) — "+
				"run `muro sandbox merge %s/%s --repo %s` first, or pass --discard-worktree %s to discard them",
				namespace, name, wt.Branch, wt.MountPath, namespace, name, wt.MountPath, wt.MountPath)
		}
	}

	if err := m.store.Delete(namespace, name); err != nil {
		return err
	}

	if logPath, err := config.SandboxLogPath(namespace, name); err == nil {
		_ = os.Remove(logPath) // a missing log (nothing was ever captured) is not an error
	}
	if m.stateDir != "" {
		_ = RemovePrivateDirs(m.stateDir, sb.ID)
	}
	// Best-effort, like RemovePrivateDirs above — the sandbox record is
	// already gone at this point, so a worktree cleanup failure (e.g. the
	// real repo was moved/deleted out from under it) must not turn a
	// successful delete into an error; nothing unmerged reaches here, the
	// guard above already ensured that.
	for _, wt := range sb.Worktrees {
		if discard[wt.MountPath] {
			_ = worktree.Discard(context.Background(), wt.RepoHost, wt.Host, wt.Branch)
		} else {
			_ = worktree.Prune(context.Background(), wt.RepoHost, wt.Host, wt.Branch)
		}
	}
	return nil
}

// Merge squash-merges sandbox namespace/name's git worktree at mountPath
// (DESIGN.md §15) into its base branch with message as the final commit
// message, then prunes the worktree and drops it (and its mount) from the
// sandbox's own record. Works regardless of the sandbox's current state —
// the merge itself is a host-side git operation on the real repo,
// independent of whether the sandbox process is running — but if the
// sandbox IS currently active, its already-launched mount table still
// includes the now-pruned worktree path, so it's marked StateReloadPending
// (the same existing mechanism Update already uses for a mount change that
// can't be hot-applied) rather than silently leaving a live sandbox with a
// mount pointing at a directory that no longer exists.
func (m *Manager) Merge(namespace, name, mountPath, message string) (commit string, err error) {
	sb, ok := m.store.Get(namespace, name)
	if !ok {
		return "", fmt.Errorf("sandbox %s/%s not found", namespace, name)
	}

	idx := -1
	for i, wt := range sb.Worktrees {
		if wt.MountPath == mountPath {
			idx = i
			break
		}
	}
	if idx == -1 {
		return "", fmt.Errorf("sandbox %s/%s has no worktree at mount_path %q", namespace, name, mountPath)
	}
	wt := sb.Worktrees[idx]

	has, err := worktree.HasUnmergedCommits(context.Background(), wt.Host, wt.BaseBranch)
	if err != nil {
		return "", fmt.Errorf("check worktree %q for unmerged commits: %w", mountPath, err)
	}
	if !has {
		return "", fmt.Errorf("worktree %q has no commits to merge", mountPath)
	}

	commit, err = worktree.SquashMerge(context.Background(), wt.RepoHost, wt.Host, wt.Branch, wt.BaseBranch, message)
	if err != nil {
		return "", err
	}

	// The merge itself already succeeded and is not reversible from here —
	// a prune failure (e.g. the worktree directory was manually tampered
	// with) must not be reported as the merge having failed, only surfaced
	// separately.
	pruneErr := worktree.Prune(context.Background(), wt.RepoHost, wt.Host, wt.Branch)

	sb.Worktrees = append(append([]state.WorktreeInfo(nil), sb.Worktrees[:idx]...), sb.Worktrees[idx+1:]...)
	newMounts := make([]config.Mount, 0, len(sb.Mounts))
	for _, mnt := range sb.Mounts {
		if mnt.Host == wt.Host && mnt.SandboxPath == wt.MountPath {
			continue
		}
		newMounts = append(newMounts, mnt)
	}
	sb.Mounts = newMounts
	if isActive(sb.State) {
		sb.State = state.StateReloadPending
	}
	if putErr := m.store.Put(sb); putErr != nil {
		return commit, fmt.Errorf("merge succeeded (commit %s) but failed to update sandbox record: %w", commit, putErr)
	}
	if pruneErr != nil {
		return commit, fmt.Errorf("merge succeeded (commit %s) but failed to prune the worktree afterward: %w", commit, pruneErr)
	}
	return commit, nil
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

		newHandle, err := m.isolator.Launch(context.Background(), m.buildLaunchSpec(&sb))
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
	switch {
	case sb.RestartPolicy == "on-failure" && !cleanExit:
		sb.State = state.StateRestartExhausted
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "restart-exhausted")
		}
	case cleanExit:
		// The process exited on its own with code 0 (e.g. the agent/shell
		// ran `exit`) and restart_policy didn't call for a relaunch — this
		// is an ordinary stop, not a crash, exactly like `muro sandbox
		// stop` setting StateStopped, just initiated from inside the
		// sandbox instead of by the user's own command.
		sb.State = state.StateStopped
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "stopped")
		}
	default:
		sb.State = state.StateCrashed
		if m.publisher != nil {
			_ = m.publisher.PublishStatus(namespace, name, "crashed")
		}
	}
	final := sb
	_ = m.store.Put(&final)
}
