package cli

import (
	"encoding/json"
	"io"

	"github.com/olekukonko/tablewriter"
)

// RenderJSON writes v to w as indented JSON — the --json path every
// list/show/status command supports (DESIGN.md §9).
func RenderJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// RenderTable writes headers/rows to w as a human-readable table — the
// default (non --json) output every list/show/status command produces.
func RenderTable(w io.Writer, headers []string, rows [][]string) error {
	t := tablewriter.NewWriter(w)
	t.Header(headers)
	for _, r := range rows {
		if err := t.Append(r); err != nil {
			return err
		}
	}
	return t.Render()
}
