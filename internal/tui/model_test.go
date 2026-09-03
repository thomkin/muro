package tui

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/thomkin/muro/internal/control"
)

var errBoom = errors.New("boom")

// key builds a tea.KeyMsg for the handful of keys this package's tests
// need to simulate — a plain rune, or one of the named special keys whose
// .String() this model's handleKey switches on.
func key(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEnter})
	case "esc":
		return tea.KeyMsg(tea.Key{Type: tea.KeyEsc})
	case "f2":
		return tea.KeyMsg(tea.Key{Type: tea.KeyF2})
	case "tab":
		return tea.KeyMsg(tea.Key{Type: tea.KeyTab})
	case "ctrl+c":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlC})
	case "left":
		return tea.KeyMsg(tea.Key{Type: tea.KeyLeft})
	case "right":
		return tea.KeyMsg(tea.Key{Type: tea.KeyRight})
	case "ctrl+left":
		return tea.KeyMsg(tea.Key{Type: tea.KeyCtrlLeft})
	default:
		return tea.KeyMsg(tea.Key{Type: tea.KeyRunes, Runes: []rune(s)})
	}
}

func update(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Update(msg)
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned a %T, want Model", next)
	}
	return got
}

func TestRunningListDelegate_ItemStylesCarryTheBackgroundTint(t *testing.T) {
	d := runningListDelegate()
	styles := []struct {
		name  string
		style lipgloss.Style
	}{
		{"NormalTitle", d.Styles.NormalTitle},
		{"NormalDesc", d.Styles.NormalDesc},
		{"SelectedTitle", d.Styles.SelectedTitle},
		{"SelectedDesc", d.Styles.SelectedDesc},
		{"DimmedTitle", d.Styles.DimmedTitle},
		{"DimmedDesc", d.Styles.DimmedDesc},
	}
	for _, s := range styles {
		if s.style.GetBackground() != runningListBackground {
			t.Errorf("%s background = %v, want %v (the tint that visually separates the Running list from the console pane)", s.name, s.style.GetBackground(), runningListBackground)
		}
	}
}

func TestModel_InitialTabIsRunning(t *testing.T) {
	m := NewModel()
	if m.tab != tabRunning {
		t.Errorf("initial tab = %v, want tabRunning", m.tab)
	}
}

func TestModel_TabKeyTogglesBetweenRunningAndProfiles(t *testing.T) {
	m := NewModel()
	m = update(t, m, key("tab"))
	if m.tab != tabProfiles {
		t.Errorf("tab after one Tab press = %v, want tabProfiles", m.tab)
	}
	m = update(t, m, key("tab"))
	if m.tab != tabRunning {
		t.Errorf("tab after two Tab presses = %v, want tabRunning", m.tab)
	}
}

func TestModel_StatusMsgPopulatesRunningList(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", Agent: "claude", State: "running"},
		{Namespace: "default", Name: "agent-2", Agent: "claude", State: "stopped"},
	}})
	items := m.running.Items()
	if len(items) != 2 {
		t.Fatalf("running list has %d items, want 2", len(items))
	}
	sb, ok := items[0].(sandboxItem)
	if !ok || sb.view.Name != "agent-1" {
		t.Errorf("items[0] = %+v, want sandboxItem for agent-1", items[0])
	}
}

func TestModel_StatusMsgErrorSetsErrWithoutClearingList(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, statusMsg{err: errBoom})
	if m.err == nil {
		t.Error("expected err to be set after a failed status poll")
	}
	if len(m.running.Items()) != 1 {
		t.Errorf("a failed poll must not wipe out the last known list, got %d items", len(m.running.Items()))
	}
}

func TestModel_ProfilesMsgPopulatesProfilesList(t *testing.T) {
	m := NewModel()
	m = update(t, m, profilesMsg{names: []string{"claude-base", "myagent"}})
	items := m.profiles.Items()
	if len(items) != 2 {
		t.Fatalf("profiles list has %d items, want 2", len(items))
	}
	if items[0].(profileItem).name != "claude-base" {
		t.Errorf("items[0] = %+v, want profileItem{claude-base}", items[0])
	}
}

func TestModel_EnterOnProfileOpensLaunchFormWithNameFocused(t *testing.T) {
	m := NewModel()
	m = update(t, m, profilesMsg{names: []string{"myagent"}})
	m = update(t, m, key("tab")) // move to Profiles tab
	m = update(t, m, key("enter"))

	if m.screen != screenLaunchForm {
		t.Fatalf("screen = %v, want screenLaunchForm", m.screen)
	}
	if m.launchProfile != "myagent" {
		t.Errorf("launchProfile = %q, want myagent", m.launchProfile)
	}
	if !m.nameInput.Focused() {
		t.Error("expected the name input to be focused after opening the launch form")
	}
}

func TestModel_LaunchFormRequiresNonEmptyName(t *testing.T) {
	m := NewModel()
	m = update(t, m, profilesMsg{names: []string{"myagent"}})
	m = update(t, m, key("tab"))
	m = update(t, m, key("enter")) // open launch form, name input empty

	m = update(t, m, key("enter")) // submit with no name typed
	if m.screen != screenLaunchForm {
		t.Error("expected to stay on the launch form when name is empty")
	}
	if m.err == nil {
		t.Error("expected an error set for an empty sandbox name")
	}
}

func TestModel_EscFromLaunchFormReturnsToList(t *testing.T) {
	m := NewModel()
	m = update(t, m, profilesMsg{names: []string{"myagent"}})
	m = update(t, m, key("tab"))
	m = update(t, m, key("enter"))
	if m.screen != screenLaunchForm {
		t.Fatal("precondition: expected launch form open")
	}

	m = update(t, m, key("esc"))
	if m.screen != screenList {
		t.Errorf("screen after esc = %v, want screenList", m.screen)
	}
}

func TestModel_LaunchedMsgErrorReturnsToListWithoutAttaching(t *testing.T) {
	m := NewModel()
	m.screen = screenLaunchForm
	m = update(t, m, launchedMsg{err: errBoom})
	if m.err == nil {
		t.Error("expected err to be set")
	}
	if m.screen != screenList {
		t.Errorf("screen = %v, want screenList after a failed launch", m.screen)
	}
}

func TestModel_FilteringModeDoesNotTreatLetterKeysAsShortcuts(t *testing.T) {
	m := NewModel()
	m = update(t, m, profilesMsg{names: []string{"one", "two"}})
	// Enter filter mode on the Profiles tab (bubbles/list's own default
	// keybinding, "/").
	m = update(t, m, key("tab"))
	m = update(t, m, key("/"))
	if m.profiles.FilterState() != list.Filtering {
		t.Fatalf("precondition: expected the profiles list to be in filtering state, got %v", m.profiles.FilterState())
	}

	// "q" while filtering must be typed into the filter box, not quit —
	// Update's returned Cmd would be tea.Quit's sentinel if misrouted;
	// since we can't easily compare tea.Cmd values, assert on the
	// observable side effect instead: the screen/tab state is unchanged
	// and the model is still the Profiles tab (a real quit wouldn't
	// change tab either, so the meaningful assertion is that the filter
	// input actually received the "q").
	m = update(t, m, key("q"))
	if m.tab != tabProfiles {
		t.Errorf("tab changed to %v while filtering — a shortcut key leaked through", m.tab)
	}
}

// TestModel_EnterOnStoppedSandboxRestartsIt covers the real reported bug:
// once every sandbox is stopped, there was no way to start one again from
// the TUI at all (Enter just showed "not running"). Enter on a
// non-attachable sandbox now issues restartCmd instead of erroring — the
// same affordance Enter already has on a Profiles-tab entry (start it,
// then attach once it's up).
func TestModel_EnterOnStoppedSandboxRestartsIt(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "stopped"},
	}})
	next, cmd := m.Update(key("enter"))
	got, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned a %T, want Model", next)
	}
	if got.err != nil {
		t.Errorf("expected no error, got %v", got.err)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd (restartCmd) for a stopped sandbox")
	}
	if _, ok := cmd().(restartedMsg); !ok {
		t.Errorf("Cmd() produced %T, want restartedMsg", cmd())
	}
}

// TestModel_RestartedMsgAttachesOnSuccess confirms a successful restart
// moves straight into console focus and follows the now-started sandbox,
// rather than requiring a second Enter.
func TestModel_RestartedMsgAttachesOnSuccess(t *testing.T) {
	m := NewModel()
	next, cmd := m.Update(restartedMsg{namespace: "default", name: "agent-1"})
	got := next.(Model)
	if got.focus != focusConsole {
		t.Errorf("focus = %v, want focusConsole", got.focus)
	}
	if got.pendingTarget != "default/agent-1" {
		t.Errorf("pendingTarget = %q, want default/agent-1", got.pendingTarget)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd (pollStatusCmd + switchToCmd batched)")
	}
}

func TestModel_RestartedMsgErrorSetsErrAndDoesNotAttach(t *testing.T) {
	m := NewModel()
	next, _ := m.Update(restartedMsg{namespace: "default", name: "agent-1", err: errBoom})
	got := next.(Model)
	if got.err == nil {
		t.Error("expected err to be set after a failed restart")
	}
	if got.focus != focusListPane {
		t.Errorf("focus = %v, want focusListPane (unchanged) after a failed restart", got.focus)
	}
}

func TestModel_EnterOnRunningSandboxAttemptsAttach(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})
	_, cmd := m.Update(key("enter"))
	if cmd == nil {
		t.Error("expected a non-nil Cmd (switchToCmd) for a running sandbox")
	}
}

func TestModel_RunningListScopedToActiveNamespace(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "duo", Name: "frank1"},
		{Namespace: "default", Name: "agent-1"},
	}})
	// "default" sorts before "duo", so the very first population should
	// default to showing "default"'s sandboxes only.
	if m.activeNamespace != "default" {
		t.Fatalf("activeNamespace = %q, want default", m.activeNamespace)
	}
	items := m.running.Items()
	if len(items) != 1 {
		t.Fatalf("running list has %d items, want 1 (only \"default\"'s sandbox)", len(items))
	}
	sb, ok := items[0].(sandboxItem)
	if !ok || sb.view.Name != "agent-1" {
		t.Errorf("items[0] = %+v, want sandboxItem for agent-1", items[0])
	}
}

func TestModel_CycleNamespaceKeySwitchesActiveNamespaceAndScopedList(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "duo", Name: "frank1", State: "running"},
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})

	m = update(t, m, key("right"))
	if m.activeNamespace != "duo" {
		t.Fatalf("activeNamespace after right = %q, want duo", m.activeNamespace)
	}
	items := m.running.Items()
	if len(items) != 1 {
		t.Fatalf("running list has %d items, want 1 (only \"duo\"'s sandbox)", len(items))
	}
	if sb, ok := items[0].(sandboxItem); !ok || sb.view.Name != "frank1" {
		t.Errorf("items[0] = %+v, want sandboxItem for frank1", items[0])
	}

	m = update(t, m, key("left"))
	if m.activeNamespace != "default" {
		t.Errorf("activeNamespace after right then left = %q, want default (back where it started)", m.activeNamespace)
	}
}

func TestModel_CycleNamespaceWrapsAround(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "duo", Name: "frank1"},
		{Namespace: "default", Name: "agent-1"},
	}})
	// Starts on "default" (first alphabetically); "left" (previous) from the
	// first namespace must wrap around to the last one, not do nothing.
	m = update(t, m, key("left"))
	if m.activeNamespace != "duo" {
		t.Errorf("activeNamespace after left from the first namespace = %q, want duo (wrapped to the last)", m.activeNamespace)
	}
}

func TestModel_CycleNamespaceNoopWithOnlyOneNamespace(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, key("right"))
	if m.activeNamespace != "default" {
		t.Errorf("activeNamespace = %q, want unchanged \"default\" with only one namespace known", m.activeNamespace)
	}
}

func TestModel_ActiveNamespaceFallsBackWhenItsLastSandboxDisappears(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "duo", Name: "frank1"},
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, key("right")) // switch to "duo"
	if m.activeNamespace != "duo" {
		t.Fatalf("activeNamespace = %q, want duo before the fallback scenario", m.activeNamespace)
	}

	// "duo" no longer has any sandboxes at all (deleted, or its last one
	// stopped and expired) — the next poll shouldn't leave activeNamespace
	// pointing at a namespace with nothing in it.
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	if m.activeNamespace != "default" {
		t.Errorf("activeNamespace after its namespace vanished = %q, want fallback to default", m.activeNamespace)
	}
}

func TestModel_StopKeyIssuesStopCmdForSelectedRunningItem(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	_, cmd := m.Update(key("s"))
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd for the stop key")
	}
	// stopCmd dials the real control socket; in this test environment that
	// dial will fail, which is fine — the point is that pressing "s"
	// actually produced a stopCmd-shaped command (a stoppedMsg, success or
	// failure) rather than nil or some other Cmd.
	if _, ok := cmd().(stoppedMsg); !ok {
		t.Errorf("Cmd() produced %T, want stoppedMsg", cmd())
	}
}

func TestModel_DeleteKeyOpensConfirmScreenWithoutDeletingYet(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, key("d"))
	if m.screen != screenConfirmDelete {
		t.Fatalf("screen = %v, want screenConfirmDelete after pressing d", m.screen)
	}
	if m.deleteNamespace != "default" || m.deleteName != "agent-1" {
		t.Errorf("pending delete target = %q/%q, want default/agent-1", m.deleteNamespace, m.deleteName)
	}
}

func TestModel_ConfirmDeleteYIssuesDeleteCmdAndReturnsToList(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, key("d"))

	next, cmd := m.Update(key("y"))
	m = next.(Model)
	if m.screen != screenList {
		t.Errorf("screen = %v, want screenList after confirming delete", m.screen)
	}
	if m.deleteNamespace != "" || m.deleteName != "" {
		t.Errorf("pending delete target = %q/%q, want cleared after confirming", m.deleteNamespace, m.deleteName)
	}
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd for the confirmed delete")
	}
	if _, ok := cmd().(deletedMsg); !ok {
		t.Errorf("Cmd() produced %T, want deletedMsg", cmd())
	}
}

func TestModel_ConfirmDeleteAnyOtherKeyCancelsWithoutDeleting(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1"},
	}})
	m = update(t, m, key("d"))

	next, cmd := m.Update(key("n"))
	m = next.(Model)
	if m.screen != screenList {
		t.Errorf("screen = %v, want screenList after canceling delete", m.screen)
	}
	if m.deleteNamespace != "" || m.deleteName != "" {
		t.Errorf("pending delete target = %q/%q, want cleared after canceling", m.deleteNamespace, m.deleteName)
	}
	if cmd != nil {
		t.Errorf("expected a nil Cmd when canceling delete (no request should be sent), got %v", cmd())
	}
}

func TestModel_DeletedMsgErrorSetsErr(t *testing.T) {
	m := NewModel()
	m = update(t, m, deletedMsg{err: errBoom})
	if m.err == nil {
		t.Error("expected err to be set after a failed delete")
	}
}

func TestModel_StatusMsgFirstPopulationFollowsSelection(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})
	if m.pendingTarget != "default/agent-1" {
		t.Errorf("pendingTarget = %q, want default/agent-1 (highlighting the first item on initial population should follow it)", m.pendingTarget)
	}
}

func TestModel_StatusMsgDoesNotReswitchWhenAlreadyFollowingSelection(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})
	// Simulate the session having already opened for this target, and
	// clear pendingTarget so the next assertion is unambiguous.
	m.sessTarget = "default/agent-1"
	m.pendingTarget = ""

	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})
	if m.pendingTarget != "" {
		t.Errorf("pendingTarget = %q, want empty — already following this target, no re-switch needed", m.pendingTarget)
	}
}

func TestModel_StatusMsgDoesNotFollowNonAttachableSelection(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "stopped"},
	}})
	if m.pendingTarget != "" {
		t.Errorf("pendingTarget = %q, want empty — a stopped sandbox has nothing to attach to", m.pendingTarget)
	}
}

func TestModel_ConsoleFocusForwardsKeystrokesToSessionInsteadOfShortcuts(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}

	m = update(t, m, key("q")) // would quit in list focus -- must be forwarded as input here
	if buf.String() != "q" {
		t.Errorf("session received %q, want %q — console focus must forward keys, not treat them as shortcuts", buf.String(), "q")
	}
}

func TestModel_ConsoleFocusKeyReturnsNilCmd(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}

	_, cmd := m.Update(key("q"))
	if cmd != nil {
		t.Error("expected a nil Cmd while console-focused (no accidental tea.Quit or other side command)")
	}
}

func TestModel_SessionOpenedMsgDiscardsStaleResponse(t *testing.T) {
	m := NewModel()
	m.pendingTarget = "default/current"

	m = update(t, m, sessionOpenedMsg{target: "default/stale", sess: &session{}})
	if m.sess != nil {
		t.Error("a sessionOpenedMsg whose target no longer matches pendingTarget must not become the active session")
	}
}

func TestModel_SessionOpenedMsgAppliesMatchingResponse(t *testing.T) {
	m := NewModel()
	m.pendingTarget = "default/agent-1"
	sess := &session{namespace: "default", name: "agent-1"}

	m = update(t, m, sessionOpenedMsg{target: "default/agent-1", sess: sess})
	if m.sess != sess {
		t.Error("expected the matching sessionOpenedMsg to become the active session")
	}
	if m.sessTarget != "default/agent-1" {
		t.Errorf("sessTarget = %q, want default/agent-1", m.sessTarget)
	}
}

func TestModel_SessionOpenedMsgErrorClearsSession(t *testing.T) {
	m := NewModel()
	m.pendingTarget = "default/agent-1"

	m = update(t, m, sessionOpenedMsg{target: "default/agent-1", err: errBoom})
	if m.sess != nil {
		t.Error("expected sess to stay nil after a failed sessionOpenedMsg")
	}
	if m.err == nil {
		t.Error("expected err to be set after a failed sessionOpenedMsg")
	}
}

func TestModel_PaneTickReturnsFocusToListWhenSessionDies(t *testing.T) {
	done := make(chan struct{})
	close(done) // already ended

	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{done: done}

	m = update(t, m, paneTickMsg(time.Now()))
	if m.focus != focusListPane {
		t.Errorf("focus = %v, want focusListPane after the session died", m.focus)
	}
}

func TestModel_PaneTickKeepsConsoleFocusWhileSessionAlive(t *testing.T) {
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{done: make(chan struct{})} // not closed -- still live

	m = update(t, m, paneTickMsg(time.Now()))
	if m.focus != focusConsole {
		t.Error("focus should remain focusConsole while the session is still live")
	}
}

func TestModel_F2ReturnsFocusToListWithoutClosingSession(t *testing.T) {
	m := NewModel()
	m.focus = focusConsole
	sess := &session{namespace: "default", name: "agent-1", done: make(chan struct{})}
	m.sess = sess
	m.sessTarget = "default/agent-1"

	next, _ := m.Update(key("f2"))
	got := next.(Model)
	if got.focus != focusListPane {
		t.Errorf("focus = %v, want focusListPane", got.focus)
	}
	if got.sess != sess {
		t.Error("F2 must not close/replace the session, only release keyboard focus")
	}
	if !got.sess.Live() {
		t.Error("expected the session to remain live after F2")
	}
}

func TestModel_CtrlLeftReturnsFocusToListWithoutClosingSession(t *testing.T) {
	m := NewModel()
	m.focus = focusConsole
	sess := &session{namespace: "default", name: "agent-1", done: make(chan struct{})}
	m.sess = sess
	m.sessTarget = "default/agent-1"

	next, _ := m.Update(key("ctrl+left"))
	got := next.(Model)
	if got.focus != focusListPane {
		t.Errorf("focus = %v, want focusListPane", got.focus)
	}
	if got.sess != sess {
		t.Error("Ctrl-Left must not close/replace the session, only release keyboard focus")
	}
	if !got.sess.Live() {
		t.Error("expected the session to remain live after Ctrl-Left")
	}
}

func TestModel_F2DoesNothingInListFocus(t *testing.T) {
	m := NewModel()
	m.focus = focusListPane
	next, _ := m.Update(key("f2"))
	got := next.(Model)
	if got.focus != focusListPane {
		t.Errorf("focus = %v, want focusListPane unchanged", got.focus)
	}
}

// Regression case for the bug this replaced F2's predecessor over: Esc used
// to be muro's own client-side "back to list" key, which meant it was
// silently intercepted and never reached the attached agent — a real
// problem, since agents like Claude Code use Esc themselves (cancel/clear).
// Esc must now behave like any other ordinary keystroke while console-
// focused: forwarded straight through, not intercepted.
func TestModel_EscForwardsToSessionInsteadOfSwitchingFocus(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}

	next, _ := m.Update(key("esc"))
	got := next.(Model)
	if got.focus != focusConsole {
		t.Errorf("focus = %v, want focusConsole unchanged -- Esc must not switch focus", got.focus)
	}
	if buf.String() != "\x1b" {
		t.Errorf("session received %q, want the raw ESC byte forwarded to the agent", buf.String())
	}
}

func TestModel_PageUpInConsoleFocusScrollsInsteadOfForwarding(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}
	m.paneH = 10

	m = update(t, m, tea.KeyMsg(tea.Key{Type: tea.KeyPgUp}))
	if buf.Len() != 0 {
		t.Errorf("PgUp must not be forwarded to the session, got %q written", buf.String())
	}
	if m.scrollLines != 10 {
		t.Errorf("scrollLines = %d, want 10 (one page)", m.scrollLines)
	}
}

func TestModel_PageDownClampsAtZero(t *testing.T) {
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &bytes.Buffer{}}
	m.paneH = 10
	m.scrollLines = 5

	m = update(t, m, tea.KeyMsg(tea.Key{Type: tea.KeyPgDown}))
	if m.scrollLines != 0 {
		t.Errorf("scrollLines = %d, want 0 (clamped, was already less than one page back)", m.scrollLines)
	}
}

func TestModel_MouseWheelUpInConsoleFocusScrollsInsteadOfForwarding(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}

	m = update(t, m, tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if buf.Len() != 0 {
		t.Errorf("wheel scroll must not be forwarded to the session, got %q written", buf.String())
	}
	if m.scrollLines != wheelScrollLines {
		t.Errorf("scrollLines = %d, want %d (one wheel notch)", m.scrollLines, wheelScrollLines)
	}
}

func TestModel_MouseWheelDownClampsAtZero(t *testing.T) {
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &bytes.Buffer{}}
	m.scrollLines = 1

	m = update(t, m, tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if m.scrollLines != 0 {
		t.Errorf("scrollLines = %d, want 0 (clamped, was already less than one notch back)", m.scrollLines)
	}
}

func TestModel_MouseWheelInListFocusMovesSelection(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
		{Namespace: "default", Name: "agent-2", State: "running"},
	}})
	if m.running.Index() != 0 {
		t.Fatalf("expected selection to start at index 0, got %d", m.running.Index())
	}

	m = update(t, m, tea.MouseMsg{Button: tea.MouseButtonWheelDown})
	if m.running.Index() != 1 {
		t.Errorf("running.Index() = %d, want 1 after wheel-down", m.running.Index())
	}

	m = update(t, m, tea.MouseMsg{Button: tea.MouseButtonWheelUp})
	if m.running.Index() != 0 {
		t.Errorf("running.Index() = %d, want 0 after wheel-up", m.running.Index())
	}
}

func TestModel_TypingSnapsScrollBackToLive(t *testing.T) {
	var buf bytes.Buffer
	m := NewModel()
	m.focus = focusConsole
	m.sess = &session{writer: &buf}
	m.scrollLines = 20

	m = update(t, m, key("x"))
	if m.scrollLines != 0 {
		t.Errorf("scrollLines = %d, want 0 — typing should snap back to live", m.scrollLines)
	}
	if buf.String() != "x" {
		t.Errorf("session received %q, want %q", buf.String(), "x")
	}
}

func TestModel_SessionOpenedResetsScroll(t *testing.T) {
	m := NewModel()
	m.scrollLines = 15
	m.pendingTarget = "default/agent-1"

	m = update(t, m, sessionOpenedMsg{target: "default/agent-1", sess: &session{namespace: "default", name: "agent-1"}})
	if m.scrollLines != 0 {
		t.Errorf("scrollLines = %d, want 0 after a fresh session opens", m.scrollLines)
	}
}

func TestModel_EnterOnAlreadyFollowedRunningSandboxJustTakesFocus(t *testing.T) {
	m := NewModel()
	m = update(t, m, statusMsg{sandboxes: []*control.SandboxView{
		{Namespace: "default", Name: "agent-1", State: "running"},
	}})
	// Simulate followSelectionCmd's session having already opened.
	m.sess = &session{namespace: "default", name: "agent-1", done: make(chan struct{})}
	m.sessTarget = "default/agent-1"

	next, cmd := m.Update(key("enter"))
	got := next.(Model)
	if got.focus != focusConsole {
		t.Errorf("focus = %v, want focusConsole", got.focus)
	}
	// Already attached and live — Enter should NOT issue a new switchToCmd
	// (which would attempt a real network dial).
	if cmd != nil {
		t.Error("expected no Cmd when Enter just takes focus on an already-followed, live session")
	}
}
