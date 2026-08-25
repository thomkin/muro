package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderJSON_ValidIndentedJSON(t *testing.T) {
	var buf bytes.Buffer
	in := map[string]any{"name": "claude-1", "count": 2}
	if err := RenderJSON(&buf, in); err != nil {
		t.Fatalf("RenderJSON error: %v", err)
	}

	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if out["name"] != "claude-1" {
		t.Errorf("name = %v, want claude-1", out["name"])
	}
	if !strings.Contains(buf.String(), "\n  ") {
		t.Errorf("expected indented JSON, got: %s", buf.String())
	}
}

func TestRenderTable_HeadersAndRowsPresent(t *testing.T) {
	var buf bytes.Buffer
	headers := []string{"NAME", "STATE"}
	rows := [][]string{
		{"claude-1", "running"},
		{"claude-2", "stopped"},
	}
	if err := RenderTable(&buf, headers, rows); err != nil {
		t.Fatalf("RenderTable error: %v", err)
	}

	out := buf.String()
	for _, want := range []string{"NAME", "STATE", "claude-1", "running", "claude-2", "stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q, got:\n%s", want, out)
		}
	}
}

func TestRenderTable_EmptyRowsStillRendersHeader(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderTable(&buf, []string{"NAME", "STATE"}, nil); err != nil {
		t.Fatalf("RenderTable error: %v", err)
	}
	if !strings.Contains(buf.String(), "NAME") {
		t.Errorf("expected header even with no rows, got:\n%s", buf.String())
	}
}
