package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/thomkin/muro/internal/config"
	"github.com/thomkin/muro/internal/control"
)

// pollInterval is how often the Running tab re-fetches sandbox status —
// a plain poll, not a genuine server push: internal/control.Client.Call is
// strictly one-shot (no `--watch`/streaming request type exists), and
// adding one would mean changing murod itself, which this feature
// deliberately avoids (DESIGN.md's own "just another client of the same
// control API, no daemon changes needed" promise). At "single operator, a
// handful of sandboxes" scale, a JSON round-trip over a local Unix socket
// every couple seconds is not a real cost.
const pollInterval = 1500 * time.Millisecond

type statusMsg struct {
	sandboxes []*control.SandboxView
	err       error
}

type profilesMsg struct {
	names []string
	err   error
}

type tickMsg time.Time

type launchedMsg struct {
	view *control.SandboxView
	err  error
}

type stoppedMsg struct{ err error }

func tickCmd() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func pollStatusCmd() tea.Cmd {
	return func() tea.Msg {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			return statusMsg{err: err}
		}
		defer c.Close()
		var resp control.StatusResponse
		if err := c.Call(control.TypeStatus, control.StatusRequest{}, &resp); err != nil {
			return statusMsg{err: err}
		}
		return statusMsg{sandboxes: resp.Sandboxes}
	}
}

func loadProfilesCmd() tea.Cmd {
	return func() tea.Msg {
		names, err := config.ListProfiles()
		return profilesMsg{names: names, err: err}
	}
}

func launchCmd(profileName, name string) tea.Cmd {
	return func() tea.Msg {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			return launchedMsg{err: err}
		}
		defer c.Close()
		var view control.SandboxView
		req := control.SandboxRunRequest{Profile: profileName, Name: name}
		if err := c.Call(control.TypeSandboxRun, req, &view); err != nil {
			return launchedMsg{err: err}
		}
		return launchedMsg{view: &view}
	}
}

func stopCmd(namespace, name string) tea.Cmd {
	return func() tea.Msg {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			return stoppedMsg{err: err}
		}
		defer c.Close()
		req := control.SandboxStopRequest{Namespace: namespace, Name: name}
		if err := c.Call(control.TypeSandboxStop, req, nil); err != nil {
			return stoppedMsg{err: err}
		}
		return stoppedMsg{}
	}
}

type deletedMsg struct{ err error }

// deleteCmd permanently removes namespace/name's record, log, and any
// private session data (`muro sandbox delete`, DESIGN.md §9) — issued by
// handleKey's screenConfirmDelete branch after the operator presses y to
// confirm. DiscardWorktrees is deliberately left empty: a sandbox with an
// unmerged git worktree refuses to delete outright rather than silently
// discarding real code (DESIGN.md §15), and the TUI has no affordance yet
// for reviewing/accepting that loss — the resulting error surfaces via
// m.err exactly like any other failed command, same as the CLI without
// --discard-worktree.
func deleteCmd(namespace, name string) tea.Cmd {
	return func() tea.Msg {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			return deletedMsg{err: err}
		}
		defer c.Close()
		req := control.SandboxDeleteRequest{Namespace: namespace, Name: name}
		if err := c.Call(control.TypeSandboxDelete, req, nil); err != nil {
			return deletedMsg{err: err}
		}
		return deletedMsg{}
	}
}

// restartedMsg is restartCmd's result — carries namespace/name (unlike
// stoppedMsg) since a successful restart is immediately followed by
// switchToCmd to attach, which needs to know which sandbox to attach to.
type restartedMsg struct {
	namespace, name string
	err             error
}

// restartCmd relaunches namespace/name — the same `muro sandbox restart`
// request type (DESIGN.md §9), which already works on a stopped sandbox,
// not only a running one that needs its config refreshed (confirmed live:
// `muro sandbox restart` on a stopped sandbox brings it back up). Used by
// handleKey's Enter case on a non-attachable Running-tab item, so
// highlighting a stopped sandbox and pressing Enter starts it and attaches
// once it's up, the same affordance Enter already has on a Profiles-tab
// entry, instead of just showing an error.
func restartCmd(namespace, name string) tea.Cmd {
	return func() tea.Msg {
		c, err := control.Dial(control.ResolveSocketPath())
		if err != nil {
			return restartedMsg{namespace: namespace, name: name, err: err}
		}
		defer c.Close()
		req := control.SandboxRestartRequest{Namespace: namespace, Name: name}
		if err := c.Call(control.TypeSandboxRestart, req, nil); err != nil {
			return restartedMsg{namespace: namespace, name: name, err: err}
		}
		return restartedMsg{namespace: namespace, name: name}
	}
}
