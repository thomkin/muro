// Package tui implements `muro tui`: a live-updating list of running
// sandboxes on the left, a launchable-profiles tab, and — the point of
// this package — a permanently-visible console pane on the right that
// mirrors whichever running sandbox is currently highlighted, live.
//
// Architecture note: unlike the plain `muro sandbox attach` CLI command
// (which hands the whole real terminal to the sandboxed process and gets
// it back on detach — internal/cli/sandbox.go, internal/ptyio), a pane
// that sits permanently next to a list can't do that: Bubble Tea has to
// keep rendering the list *at the same time* as the agent's own output.
// That means actually parsing the agent's raw terminal output — cursor
// moves, colors, its own redraws — into a screen buffer muro owns, then
// drawing that into the pane (pane.go, backed by github.com/hinshun/
// vt10x), not a byte passthrough. See pane.go and keys.go for the two
// pieces that follow from that: rendering vt10x's cell grid, and
// reconstructing raw bytes from Bubble Tea's own parsed key events to
// forward to the attached agent (Bubble Tea's input loop discards the
// original bytes once parsed).
//
// murod is not touched by this package at all: everything here is a
// client of the existing control API (internal/control), exactly the
// "just another client of the same control API" SPEC.md §5 already
// anticipated. Only one attach connection is ever open at once — DESIGN.md
// §12's exclusive-attacher-per-sandbox model, unchanged — so switching the
// highlighted sandbox always closes the previous session before opening
// the next.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/thomkin/muro/internal/control"
)

type activeTab int

const (
	tabRunning activeTab = iota
	tabProfiles
)

type screen int

const (
	screenList screen = iota
	screenLaunchForm
	screenConfirmDelete
)

// focusMode governs where keystrokes go while the Running tab is active.
// The Profiles tab has no console to route input to, so focus is only
// ever meaningful there.
type focusMode int

const (
	// focusListPane: arrows move the list selection (which, moving,
	// immediately opens a live session for the newly-highlighted sandbox
	// — see switchSelectionCmd); typed characters are not forwarded
	// anywhere.
	focusListPane focusMode = iota
	// focusConsole: every keystroke is reconstructed to raw bytes
	// (keys.go) and written straight to the attached agent, including
	// arrows — Ctrl-P Ctrl-Q returns to focusListPane (reusing the
	// existing, server-tested detach sequence unchanged).
	focusConsole
)

var (
	activeTabStyle       = lipgloss.NewStyle().Bold(true).Underline(true)
	inactiveTabStyle     = lipgloss.NewStyle().Faint(true)
	errStyle             = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	footerStyle          = lipgloss.NewStyle().Faint(true)
	consoleFocusHint     = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Bold(true)
	paneBorderStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	scrollIndicatorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
)

// runningListBackground is a subtle dark panel tint (distinct from a
// typical terminal's own default background) applied to every Running-tab
// list item — the console pane right next to it renders whatever the
// attached agent (e.g. Claude Code) itself draws, which usually assumes
// the terminal's own default background, so without this the list and the
// live console pane could read as one continuous surface instead of two
// distinct panes.
const runningListBackground = lipgloss.Color("235")

// runningListDelegate is list.NewDefaultDelegate() with runningListBackground
// baked into every item style (normal/selected/dimmed, title and
// description) so the tinted background fills each row's full padded
// width, not just the visible text — used only for m.running, never
// m.profiles, since the Profiles tab has no adjacent console pane to
// distinguish itself from.
func runningListDelegate() list.DefaultDelegate {
	d := list.NewDefaultDelegate()
	d.Styles.NormalTitle = d.Styles.NormalTitle.Background(runningListBackground)
	d.Styles.NormalDesc = d.Styles.NormalDesc.Background(runningListBackground)
	d.Styles.SelectedTitle = d.Styles.SelectedTitle.Background(runningListBackground)
	d.Styles.SelectedDesc = d.Styles.SelectedDesc.Background(runningListBackground)
	d.Styles.DimmedTitle = d.Styles.DimmedTitle.Background(runningListBackground)
	d.Styles.DimmedDesc = d.Styles.DimmedDesc.Background(runningListBackground)
	return d
}

// Model is the top-level tea.Model for `muro tui`.
type Model struct {
	tab    activeTab
	screen screen
	focus  focusMode

	running  list.Model
	profiles list.Model

	nameInput     textinput.Model
	launchProfile string

	// deleteNamespace/deleteName are the pending target while screen ==
	// screenConfirmDelete — set when "d" is pressed on a Running-tab
	// sandboxItem, read (and cleared) once the operator answers y/N.
	deleteNamespace string
	deleteName      string

	// allSandboxes is the last full statusMsg poll result, across every
	// namespace; namespaces is the distinct, sorted set of namespaces
	// within it; activeNamespace is which one m.running currently shows —
	// the Running tab displays one namespace at a time (a switcher row,
	// viewSplitPane, cycles between them via ← / →) rather than a single
	// flat list spanning all of them.
	allSandboxes    []*control.SandboxView
	namespaces      []string
	activeNamespace string

	sess          *session // the one live attach connection, if any (nil = nothing highlighted/attachable yet)
	sessTarget    string   // namespace/name sess is (or was last) following
	pendingTarget string   // namespace/name of the most recently issued switchToCmd — lets a stale, superseded sessionOpenedMsg be recognized and discarded
	paneW         int      // console pane's rendered width, in cells
	paneH         int      // console pane's rendered height, in cells
	scrollLines   int      // lines scrolled back from live, 0 = live (pane.go's renderScrollback)

	err           error
	width, height int
}

// NewModel constructs the initial Model — list contents are populated
// asynchronously by the commands Init returns, not here (this package
// never blocks on I/O outside a tea.Cmd).
func NewModel() Model {
	running := list.New(nil, runningListDelegate(), 0, 0)
	running.Title = "Running"
	profiles := list.New(nil, list.NewDefaultDelegate(), 0, 0)
	profiles.Title = "Profiles"

	name := textinput.New()
	name.Placeholder = "sandbox name"
	name.CharLimit = 64

	return Model{running: running, profiles: profiles, nameInput: name}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(pollStatusCmd(), loadProfilesCmd(), tickCmd(), paneTickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		listHeight := msg.Height - 5 // tabs line + footer + margins
		if listHeight < 3 {
			listHeight = 3
		}
		m.profiles.SetSize(msg.Width, listHeight)

		// Running tab is a split: a narrower list on the left, the
		// console pane filling the rest on the right (viewSplitPane).
		listWidth := msg.Width / 3
		if listWidth < 24 {
			listWidth = 24
		}
		if listWidth > msg.Width-20 {
			listWidth = msg.Width - 20
		}
		m.running.SetSize(listWidth, listHeight)
		m.paneW = msg.Width - listWidth - 5 // gap + pane border
		if m.paneW < 10 {
			m.paneW = 10
		}
		m.paneH = listHeight - 3 // pane header line + border
		if m.paneH < 3 {
			m.paneH = 3
		}
		if m.sess != nil {
			// The real sandbox pty is NOT resized to match (no
			// resize-forwarding protocol exists in muro yet, DESIGN.md,
			// deliberately deferred) — this only reshapes how vt10x itself
			// wraps/tracks the emulated screen, a partial improvement
			// that costs nothing to include.
			m.sess.term.Resize(m.paneW, m.paneH)
		}
		return m, nil

	case tickMsg:
		return m, tea.Batch(pollStatusCmd(), tickCmd())

	case paneTickMsg:
		if m.sess != nil && !m.sess.Live() && m.focus == focusConsole {
			// The attach stream ended (server-side detach, e.g. Ctrl-P
			// Ctrl-Q, or the sandbox exiting) — stop pretending keystrokes
			// still go anywhere. The pane keeps showing its last frame;
			// selecting a different sandbox or pressing Enter again is
			// what reattaches, not anything automatic.
			m.focus = focusListPane
		}
		return m, paneTickCmd()

	case statusMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.allSandboxes = msg.sandboxes
		m.namespaces = sortedNamespaces(msg.sandboxes)
		if !containsString(m.namespaces, m.activeNamespace) {
			// First population, or the namespace being viewed lost its last
			// sandbox (e.g. just deleted) — fall back to the first
			// namespace that actually has something in it, if any.
			if len(m.namespaces) > 0 {
				m.activeNamespace = m.namespaces[0]
			} else {
				m.activeNamespace = ""
			}
		}
		cmd := m.running.SetItems(sandboxItemsForNamespace(msg.sandboxes, m.activeNamespace))
		if follow := m.followSelectionCmd(); follow != nil {
			cmd = tea.Batch(cmd, follow)
		}
		return m, cmd

	case profilesMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		items := make([]list.Item, len(msg.names))
		for i, n := range msg.names {
			items[i] = profileItem{name: n}
		}
		return m, m.profiles.SetItems(items)

	case sessionOpenedMsg:
		if msg.target != m.pendingTarget {
			// Superseded by a later selection change that dialed faster —
			// discard rather than clobber the newer session.
			if msg.sess != nil {
				msg.sess.Close()
			}
			return m, nil
		}
		if m.sess != nil {
			m.sess.Close()
		}
		if msg.err != nil {
			m.sess = nil
			m.sessTarget = ""
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.sess = msg.sess
		m.sessTarget = msg.target
		m.scrollLines = 0 // a freshly (re)attached session always starts at live
		return m, nil

	case launchedMsg:
		m.screen = screenList
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.tab = tabRunning
		m.focus = focusConsole
		m.pendingTarget = msg.view.Namespace + "/" + msg.view.Name
		return m, tea.Batch(pollStatusCmd(), switchToCmd(msg.view.Namespace, msg.view.Name, m.paneW, m.paneH))

	case stoppedMsg:
		if msg.err != nil {
			m.err = msg.err
		}
		return m, pollStatusCmd()

	case deletedMsg:
		if msg.err != nil {
			m.err = msg.err
		}
		return m, pollStatusCmd()

	case restartedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, pollStatusCmd()
		}
		m.err = nil
		target := msg.namespace + "/" + msg.name
		m.focus = focusConsole
		m.pendingTarget = target
		return m, tea.Batch(pollStatusCmd(), switchToCmd(msg.namespace, msg.name, m.paneW, m.paneH))

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)
	}

	return m, m.updateActiveWidget(msg)
}

// cycleNamespace moves activeNamespace forward (dir 1) or back (dir -1)
// through m.namespaces, wrapping at either end, and refreshes m.running to
// show the new namespace's sandboxes — "left"/"right" arrow keys in handleKey. A no-op if
// there's nothing to cycle through (0 or 1 namespace known).
func (m *Model) cycleNamespace(dir int) tea.Cmd {
	if len(m.namespaces) < 2 {
		return nil
	}
	cur := 0
	for i, ns := range m.namespaces {
		if ns == m.activeNamespace {
			cur = i
			break
		}
	}
	next := (cur + dir + len(m.namespaces)) % len(m.namespaces)
	m.activeNamespace = m.namespaces[next]
	cmd := m.running.SetItems(sandboxItemsForNamespace(m.allSandboxes, m.activeNamespace))
	if follow := m.followSelectionCmd(); follow != nil {
		cmd = tea.Batch(cmd, follow)
	}
	return cmd
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// isAttachable mirrors internal/sandbox.isActive's state set — a sandbox
// that isn't running/reload-pending/restarting has no live pty to attach
// to at all.
func isAttachable(state string) bool {
	switch state {
	case "running", "reload-pending", "restarting":
		return true
	default:
		return false
	}
}

// followSelectionCmd opens a session for the Running tab's currently
// selected item if it isn't already the one being followed — covers the
// initial list population (nothing followed yet) and the selection moving
// to a different sandbox. Deliberately does NOT re-trigger just because a
// previously-followed session died (e.g. via an explicit Ctrl-P Ctrl-Q
// detach) — that would silently reattach right after the operator chose
// to detach, which is exactly the surprise a detach is supposed to avoid.
func (m *Model) followSelectionCmd() tea.Cmd {
	item, ok := m.running.SelectedItem().(sandboxItem)
	if !ok {
		return nil
	}
	target := item.view.Namespace + "/" + item.view.Name
	if m.sessTarget == target {
		return nil
	}
	if !isAttachable(item.view.State) {
		return nil
	}
	m.pendingTarget = target
	return switchToCmd(item.view.Namespace, item.view.Name, m.paneW, m.paneH)
}

// updateActiveWidget forwards msg to whichever widget currently owns
// keyboard focus (the name-input form, one of the two lists) — used for
// message types handleKey doesn't special-case (list navigation, filter
// typing, blink ticks, etc.). Running-tab list updates additionally check
// whether the selection moved to a different sandbox, so arrow-key
// navigation (and filtering) immediately follows the new highlight, not
// just an explicit Enter.
func (m *Model) updateActiveWidget(msg tea.Msg) tea.Cmd {
	if m.screen == screenLaunchForm {
		var cmd tea.Cmd
		m.nameInput, cmd = m.nameInput.Update(msg)
		return cmd
	}
	if m.tab == tabProfiles {
		var cmd tea.Cmd
		m.profiles, cmd = m.profiles.Update(msg)
		return cmd
	}
	var cmd tea.Cmd
	m.running, cmd = m.running.Update(msg)
	if follow := m.followSelectionCmd(); follow != nil {
		cmd = tea.Batch(cmd, follow)
	}
	return cmd
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.screen == screenLaunchForm {
		switch msg.String() {
		case "esc":
			m.screen = screenList
			return m, nil
		case "enter":
			name := strings.TrimSpace(m.nameInput.Value())
			if name == "" {
				m.err = fmt.Errorf("sandbox name is required")
				return m, nil
			}
			m.err = nil
			m.screen = screenList
			return m, launchCmd(m.launchProfile, name)
		}
		return m, m.updateActiveWidget(msg)
	}

	if m.screen == screenConfirmDelete {
		ns, name := m.deleteNamespace, m.deleteName
		m.deleteNamespace, m.deleteName = "", ""
		m.screen = screenList
		switch msg.String() {
		case "y", "Y":
			return m, deleteCmd(ns, name)
		default:
			// Matches the CLI's own "[y/N]" default: anything other than
			// y/Y (including Esc, Enter, or any stray keystroke) cancels
			// rather than deletes — an irreversible action must never be
			// the accidental-keypress default.
			return m, nil
		}
	}

	// Console focus: almost every key is raw input for the attached agent
	// — no "q" quits, no "tab" switches tabs, and (as of this fix) no Esc
	// either: Esc used to be muro's own client-side "back to the list" key,
	// but real-use feedback showed that steals Esc away from the agent
	// itself, which needs it for its own purposes (Claude Code uses Esc to
	// cancel/clear) — every single Esc press was silently eaten by the TUI
	// before it ever reached the sandboxed process. Two exceptions remain,
	// checked first, neither of which any CLI agent plausibly binds:
	//
	//   - F2 or Ctrl-Left return keyboard focus to the list, client-side
	//     only — the session itself is left running (still fed live in the
	//     background), just no longer receiving keystrokes. Bubble Tea's
	//     own key encoding never forwards F-keys to the agent in the first
	//     place (keys.go's keyBytes: "unrecognized ... dropped, not guessed
	//     at"), and Ctrl-Left specifically isn't in keyBytes'
	//     negativeKeySequences table either (only plain arrows are) — so
	//     repurposing either here changes nothing about what the agent used
	//     to receive — zero collision risk, unlike Esc. Ctrl-P Ctrl-Q is
	//     still forwarded as two ordinary bytes and still works as the
	//     "real" server-side detach (internal/control/stream.go's existing
	//     detachScanner, ending the stream exactly like `muro sandbox
	//     attach` — paneTickMsg, above, is what notices that and flips focus
	//     back too), but a real terminal or multiplexer sitting between the
	//     operator and this process can intercept or remap a two-key
	//     control-character chord in ways outside muro's control — which is
	//     exactly why Esc was tried as a single-key alternative before this
	//     fix. F2/Ctrl-Left keep that single-key reliability without Esc's
	//     collision problem.
	//   - PageUp/PageDown scroll the pane's own history buffer
	//     (pane.go's session.history) instead of being forwarded — see
	//     scrollBy below.
	if m.focus == focusConsole {
		switch msg.Type {
		case tea.KeyF2, tea.KeyCtrlLeft:
			m.focus = focusListPane
			return m, nil
		case tea.KeyPgUp:
			m.scrollBack(1)
			return m, nil
		case tea.KeyPgDown:
			m.scrollBack(-1)
			return m, nil
		}
		if m.sess != nil {
			if b := keyBytes(msg); b != nil {
				m.scrollLines = 0 // typing snaps back to live -- you're interacting, you want to see it react
				_, _ = m.sess.Write(b)
			}
		}
		return m, nil
	}

	// While a list is actively capturing filter text, every keystroke
	// (including letters that would otherwise be global shortcuts below,
	// e.g. "q" or "s") must reach the list itself, not be intercepted.
	activeList := m.running
	if m.tab == tabProfiles {
		activeList = m.profiles
	}
	if activeList.FilterState() == list.Filtering {
		return m, m.updateActiveWidget(msg)
	}

	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		if m.tab == tabRunning {
			m.tab = tabProfiles
		} else {
			m.tab = tabRunning
		}
		m.err = nil
		return m, nil
	case "enter":
		if m.tab == tabRunning {
			item, ok := m.running.SelectedItem().(sandboxItem)
			if !ok {
				return m, nil
			}
			// A stopped/crashed sandbox has no live pty to attach to —
			// but `muro sandbox restart` already works on one (confirmed
			// live), so Enter here starts it and attaches once it's up
			// (restartedMsg, above), the same affordance Enter already
			// has on a Profiles-tab entry, rather than just erroring.
			if !isAttachable(item.view.State) {
				m.err = nil
				return m, restartCmd(item.view.Namespace, item.view.Name)
			}
			m.err = nil
			target := item.view.Namespace + "/" + item.view.Name
			if m.sess != nil && m.sessTarget == target && m.sess.Live() {
				// Already following this one (highlighting it already
				// opened a session) — Enter just takes keyboard focus,
				// no need to reattach.
				m.focus = focusConsole
				return m, nil
			}
			m.focus = focusConsole
			m.pendingTarget = target
			return m, switchToCmd(item.view.Namespace, item.view.Name, m.paneW, m.paneH)
		}
		item, ok := m.profiles.SelectedItem().(profileItem)
		if !ok {
			return m, nil
		}
		m.launchProfile = item.name
		m.nameInput.SetValue("")
		m.nameInput.Focus()
		m.screen = screenLaunchForm
		m.err = nil
		return m, nil
	case "s":
		if m.tab == tabRunning {
			if item, ok := m.running.SelectedItem().(sandboxItem); ok {
				return m, stopCmd(item.view.Namespace, item.view.Name)
			}
		}
		return m, nil
	case "d":
		if m.tab == tabRunning {
			if item, ok := m.running.SelectedItem().(sandboxItem); ok {
				m.deleteNamespace = item.view.Namespace
				m.deleteName = item.view.Name
				m.screen = screenConfirmDelete
				m.err = nil
			}
		}
		return m, nil
	case "left":
		if m.tab == tabRunning {
			cmd := m.cycleNamespace(-1)
			return m, cmd
		}
		return m, nil
	case "right":
		if m.tab == tabRunning {
			cmd := m.cycleNamespace(1)
			return m, cmd
		}
		return m, nil
	}

	return m, m.updateActiveWidget(msg)
}

// handleMouse handles the one mouse interaction this TUI supports: the
// wheel (tea.WithMouseCellMotion, internal/cli/tui.go — click/drag events
// also arrive here but are ignored). Wheel over the console pane scrolls
// its history exactly like PgUp/PgDn, just a few lines per notch instead of
// a full page, matching how terminal emulators themselves scroll. Wheel
// while a list has focus moves its selection the same as arrow keys would
// — mirrors updateActiveWidget's own tab branching, including only
// following the Running-tab selection into a live attach (the Profiles tab
// has no such "follow" behavior; Enter is still required to launch there).
func (m Model) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Button != tea.MouseButtonWheelUp && msg.Button != tea.MouseButtonWheelDown {
		return m, nil
	}
	up := msg.Button == tea.MouseButtonWheelUp

	if m.focus == focusConsole {
		lines := wheelScrollLines
		if !up {
			lines = -lines
		}
		m.scrollByLines(lines)
		return m, nil
	}

	if m.screen == screenLaunchForm {
		return m, nil
	}
	if m.tab == tabProfiles {
		if up {
			m.profiles.CursorUp()
		} else {
			m.profiles.CursorDown()
		}
		return m, nil
	}
	if up {
		m.running.CursorUp()
	} else {
		m.running.CursorDown()
	}
	return m, m.followSelectionCmd()
}

func (m Model) View() string {
	var b strings.Builder

	runningTab := "Running"
	profilesTab := "Profiles"
	if m.tab == tabRunning {
		runningTab = activeTabStyle.Render(runningTab)
		profilesTab = inactiveTabStyle.Render(profilesTab)
	} else {
		runningTab = inactiveTabStyle.Render(runningTab)
		profilesTab = activeTabStyle.Render(profilesTab)
	}
	b.WriteString(runningTab + "   " + profilesTab + "\n\n")

	if m.screen == screenLaunchForm {
		fmt.Fprintf(&b, "Launch new sandbox from profile %q\n\n", m.launchProfile)
		b.WriteString("Name: " + m.nameInput.View() + "\n\n")
		b.WriteString(footerStyle.Render("enter: launch and attach  ·  esc: cancel"))
	} else if m.screen == screenConfirmDelete {
		fmt.Fprintf(&b, "Delete %s/%s?\n\n", m.deleteNamespace, m.deleteName)
		b.WriteString(errStyle.Render("This permanently removes its record, log, and any private session data.") + "\n\n")
		b.WriteString(footerStyle.Render("y: confirm delete  ·  any other key: cancel"))
	} else if m.tab == tabRunning {
		b.WriteString(m.viewSplitPane())
	} else {
		b.WriteString(m.profiles.View())
	}

	if m.err != nil {
		b.WriteString("\n" + errStyle.Render("error: "+m.err.Error()))
	}

	if m.screen == screenList {
		b.WriteString("\n" + footerStyle.Render(m.footerHint()))
	}

	return b.String()
}

// viewSplitPane renders the Running tab's left list next to the right
// console pane — the whole point of this package (model.go's package doc).
// scrollBack moves the console pane's scroll position by dir "pages" (one
// pane-height's worth of lines each) — positive scrolls back into history,
// negative scrolls toward live. Used by PgUp/PgDn.
func (m *Model) scrollBack(dir int) {
	page := m.paneH
	if page < 1 {
		page = 1
	}
	m.scrollByLines(dir * page)
}

// wheelScrollLines is how many lines one mouse-wheel notch scrolls — small
// enough for smooth, granular scrolling (unlike PgUp/PgDn's full-page
// jump), matching the handful-of-lines-per-notch convention most terminal
// emulators default to themselves.
const wheelScrollLines = 3

// scrollByLines moves the console pane's scroll position by n lines —
// positive scrolls back into history, negative scrolls toward live, clamped
// at 0 (live). Only meaningful with an active session; a no-op otherwise.
func (m *Model) scrollByLines(n int) {
	if m.sess == nil {
		return
	}
	m.scrollLines += n
	if m.scrollLines < 0 {
		m.scrollLines = 0
	}
}

func (m Model) viewSplitPane() string {
	paneHeader := m.sessTarget
	if paneHeader == "" {
		paneHeader = "(nothing highlighted)"
	} else if m.focus == focusConsole {
		paneHeader = consoleFocusHint.Render(paneHeader + " — typing")
	}
	if m.scrollLines > 0 {
		paneHeader += scrollIndicatorStyle.Render(fmt.Sprintf("  (scrolled back %d — PgDn to return to live)", m.scrollLines))
	}

	var content string
	if m.sess == nil {
		content = lipgloss.NewStyle().Faint(true).Render("nothing to show yet")
	} else if m.scrollLines > 0 {
		content = renderScrollback(m.sess.historySnapshot(), m.scrollLines, m.paneW, m.paneH)
	} else {
		content = renderPane(m.sess.term, m.paneW, m.paneH)
	}
	pane := paneHeader + "\n" + paneBorderStyle.Width(m.paneW).Height(m.paneH).Render(content)

	leftColumn := m.running.View()
	if len(m.namespaces) > 1 {
		leftColumn = m.namespaceTabsView() + "\n" + leftColumn
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, leftColumn, "  ", pane)
}

// namespaceTabsView renders a row of namespace names above the Running
// list, the active one styled the same way the top-level Running/Profiles
// tabs are (activeTabStyle/inactiveTabStyle) — only called when more than
// one namespace is actually in use (viewSplitPane), so the common
// single-namespace case stays exactly as uncluttered as before this
// feature existed.
func (m Model) namespaceTabsView() string {
	parts := make([]string, len(m.namespaces))
	for i, ns := range m.namespaces {
		if ns == m.activeNamespace {
			parts[i] = activeTabStyle.Render(ns)
		} else {
			parts[i] = inactiveTabStyle.Render(ns)
		}
	}
	return strings.Join(parts, "  ")
}

func (m Model) footerHint() string {
	if m.tab == tabProfiles {
		return "enter: launch  ·  tab: switch pane  ·  /: filter  ·  q: quit"
	}
	if m.focus == focusConsole {
		return "typing goes to " + m.sessTarget + "  ·  f2 or ctrl-←: back to the list  ·  pgup/pgdn or wheel: scroll  ·  ctrl-p ctrl-q: end session"
	}
	hint := "↑/↓: highlight (console follows)  ·  enter: type into it (or start, if stopped)  ·  s: stop  ·  d: delete"
	if len(m.namespaces) > 1 {
		hint += "  ·  ←/→: switch namespace"
	}
	return hint + "  ·  tab: switch pane  ·  q: quit"
}
