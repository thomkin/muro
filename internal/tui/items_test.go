package tui

import (
	"strings"
	"testing"

	"github.com/thomkin/muro/internal/control"
)

func TestSandboxItem_TitleAndFilterValueUseNamespaceName(t *testing.T) {
	i := sandboxItem{view: &control.SandboxView{Namespace: "duo", Name: "frank1"}}
	if i.Title() != "duo/frank1" {
		t.Errorf("Title() = %q, want duo/frank1", i.Title())
	}
	if i.FilterValue() != "duo/frank1" {
		t.Errorf("FilterValue() = %q, want duo/frank1", i.FilterValue())
	}
}

func TestSandboxItem_DescriptionIncludesAgentAndState(t *testing.T) {
	i := sandboxItem{view: &control.SandboxView{Agent: "/bin/claude", State: "running", StartedAt: "2026-08-29T00:00:00Z"}}
	d := i.Description()
	if !strings.Contains(d, "/bin/claude") || !strings.Contains(d, "running") {
		t.Errorf("Description() = %q, want it to mention the agent and state", d)
	}
}

func TestProfileItem_TitleAndFilterValueUseName(t *testing.T) {
	i := profileItem{name: "code-reviewer"}
	if i.Title() != "code-reviewer" || i.FilterValue() != "code-reviewer" {
		t.Errorf("Title/FilterValue = %q/%q, want code-reviewer for both", i.Title(), i.FilterValue())
	}
}

func TestSortedNamespaces_DistinctSortedNoDuplicates(t *testing.T) {
	got := sortedNamespaces([]*control.SandboxView{
		{Namespace: "duo", Name: "frank2"},
		{Namespace: "default", Name: "zzz"},
		{Namespace: "duo", Name: "frank1"},
		{Namespace: "default", Name: "aaa"},
	})
	want := []string{"default", "duo"}
	if len(got) != len(want) {
		t.Fatalf("sortedNamespaces() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sortedNamespaces()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSortedNamespaces_EmptyInputProducesNoNamespaces(t *testing.T) {
	if got := sortedNamespaces(nil); len(got) != 0 {
		t.Errorf("sortedNamespaces(nil) = %v, want none", got)
	}
}

func TestSandboxItemsForNamespace_FiltersAndSortsByName(t *testing.T) {
	items := sandboxItemsForNamespace([]*control.SandboxView{
		{Namespace: "duo", Name: "frank2"},
		{Namespace: "default", Name: "zzz"},
		{Namespace: "duo", Name: "frank1"},
		{Namespace: "default", Name: "aaa"},
	}, "duo")

	wantNames := []string{"frank1", "frank2"}
	if len(items) != len(wantNames) {
		t.Fatalf("sandboxItemsForNamespace(..., \"duo\") returned %d items, want %d: %+v", len(items), len(wantNames), items)
	}
	for i, want := range wantNames {
		sb, ok := items[i].(sandboxItem)
		if !ok || sb.view.Name != want {
			t.Errorf("items[%d] = %+v, want sandboxItem for %q", i, items[i], want)
		}
	}
}

func TestSandboxItemsForNamespace_NoMatchesReturnsEmpty(t *testing.T) {
	items := sandboxItemsForNamespace([]*control.SandboxView{
		{Namespace: "default", Name: "a"},
	}, "duo")
	if len(items) != 0 {
		t.Errorf("sandboxItemsForNamespace() for a namespace with no sandboxes = %+v, want none", items)
	}
}
