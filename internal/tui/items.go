package tui

import (
	"fmt"

	"github.com/thomkin/muro/internal/control"
)

// sandboxItem adapts a *control.SandboxView (the same type `muro status`
// already renders as a table row) into a bubbles/list.DefaultItem.
type sandboxItem struct {
	view *control.SandboxView
}

func (i sandboxItem) FilterValue() string { return i.view.Namespace + "/" + i.view.Name }
func (i sandboxItem) Title() string       { return i.view.Namespace + "/" + i.view.Name }
func (i sandboxItem) Description() string {
	return fmt.Sprintf("%s · %s · started %s", i.view.Agent, i.view.State, i.view.StartedAt)
}

// profileItem adapts a profile name (config.ListProfiles) into a
// bubbles/list.DefaultItem.
type profileItem struct {
	name string
}

func (i profileItem) FilterValue() string { return i.name }
func (i profileItem) Title() string       { return i.name }
func (i profileItem) Description() string {
	return "profile — press enter to launch a new sandbox from it"
}
