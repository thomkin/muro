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
