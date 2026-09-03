package tui

import (
	"fmt"
	"sort"

	"github.com/charmbracelet/bubbles/list"

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

// sortedNamespaces returns the distinct namespaces present across
// sandboxes, sorted — used to build the Running tab's namespace-switcher
// row (model.go's viewSplitPane) and to decide which namespace's sandboxes
// m.running currently shows.
func sortedNamespaces(sandboxes []*control.SandboxView) []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, sb := range sandboxes {
		if !seen[sb.Namespace] {
			seen[sb.Namespace] = true
			namespaces = append(namespaces, sb.Namespace)
		}
	}
	sort.Strings(namespaces)
	return namespaces
}

// sandboxItemsForNamespace returns list.Items for just ns's sandboxes,
// sorted by name — the Running tab shows one namespace at a time (a
// namespace-switcher row cycles between them) rather than a single
// ever-growing flat list, since muro supports isolating sandboxes by
// namespace (SPEC.md §7) and that stops being navigable once more than a
// handful of sandboxes are running across more than one.
func sandboxItemsForNamespace(sandboxes []*control.SandboxView, ns string) []list.Item {
	var matching []*control.SandboxView
	for _, sb := range sandboxes {
		if sb.Namespace == ns {
			matching = append(matching, sb)
		}
	}
	sort.Slice(matching, func(i, j int) bool { return matching[i].Name < matching[j].Name })

	items := make([]list.Item, len(matching))
	for i, sb := range matching {
		items[i] = sandboxItem{view: sb}
	}
	return items
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
