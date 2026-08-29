package tui

import (
	"strings"
	"testing"

	"github.com/hinshun/vt10x"
)

func TestRenderPane_PlainText(t *testing.T) {
	term := vt10x.New(vt10x.WithSize(10, 3))
	if _, err := term.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	out := renderPane(term, 10, 3)
	if !strings.Contains(out, "h") || !strings.Contains(out, "i") {
		t.Errorf("renderPane output missing written text: %q", out)
	}
}

func TestRenderPane_BoldAndPlainCellsStyleDifferently(t *testing.T) {
	term := vt10x.New(vt10x.WithSize(10, 3))
	// \x1b[1m = bold on, \x1b[0m = reset.
	if _, err := term.Write([]byte("\x1b[1mB\x1b[0mp")); err != nil {
		t.Fatal(err)
	}
	term.Lock()
	bold := term.Cell(0, 0)
	plain := term.Cell(1, 0)
	term.Unlock()

	boldOut := renderGlyph(bold)
	plainOut := renderGlyph(plain)
	if boldOut == plainOut {
		t.Errorf("bold and plain cells rendered identically: %q == %q", boldOut, plainOut)
	}
	if bold.Mode&vtAttrBold == 0 {
		t.Error("expected the 'B' cell to have the bold attribute set")
	}
	if plain.Mode&vtAttrBold != 0 {
		t.Error("expected the 'p' cell to NOT have the bold attribute set")
	}
}

func TestRenderPane_ClampsToTerminalSizeWithoutPanicking(t *testing.T) {
	term := vt10x.New(vt10x.WithSize(5, 2))
	// Requesting a pane LARGER than the terminal's own size must not
	// panic (vt10x.Cell has no bounds checking of its own) — this is the
	// regression case for renderPane's clamping.
	out := renderPane(term, 40, 20)
	lines := strings.Split(out, "\n")
	if len(lines) != 20 {
		t.Errorf("got %d lines, want 20 (pane height, even though terminal is only 2 rows tall)", len(lines))
	}
}

func TestVtColor_ANSIIndex(t *testing.T) {
	c, ok := vtColor(vt10x.Red)
	if !ok {
		t.Fatal("expected ok=true for a plain ANSI color")
	}
	if string(c) != "1" {
		t.Errorf("vtColor(Red) = %q, want ANSI index \"1\"", c)
	}
}

func TestVtColor_TrueColorHex(t *testing.T) {
	// r=173, g=88, b=179 packed as vt10x's SGR 38;2 handling does.
	packed := vt10x.Color(173<<16 | 88<<8 | 179)
	c, ok := vtColor(packed)
	if !ok {
		t.Fatal("expected ok=true for a true-color value")
	}
	if string(c) != "#ad58b3" {
		t.Errorf("vtColor(true color) = %q, want #ad58b3", c)
	}
}

func TestVtColor_DefaultSentinelReturnsNoExplicitColor(t *testing.T) {
	_, ok := vtColor(vt10x.DefaultFG)
	if ok {
		t.Error("expected ok=false for vt10x.DefaultFG (no explicit color, use terminal default)")
	}
}

func TestScrollCutoff_ZeroOrNegativeReturnsWholeBuffer(t *testing.T) {
	history := []byte("a\nb\nc\n")
	if got := scrollCutoff(history, 0); got != len(history) {
		t.Errorf("scrollCutoff(0) = %d, want %d (whole buffer)", got, len(history))
	}
	if got := scrollCutoff(history, -3); got != len(history) {
		t.Errorf("scrollCutoff(-3) = %d, want %d (whole buffer)", got, len(history))
	}
}

func TestScrollCutoff_CountsLinesBackFromTheEnd(t *testing.T) {
	history := []byte("line1\nline2\nline3\nline4\n")
	// 1 line back should cut off after "line1\nline2\nline3\n" -- i.e.
	// exclude "line4\n", the most recent line.
	cutoff := scrollCutoff(history, 1)
	got := string(history[:cutoff])
	if got != "line1\nline2\nline3\n" {
		t.Errorf("scrollCutoff(1) content = %q, want %q", got, "line1\nline2\nline3\n")
	}
}

func TestScrollCutoff_MoreLinesThanExistReturnsZero(t *testing.T) {
	history := []byte("only one line\n")
	if got := scrollCutoff(history, 100); got != 0 {
		t.Errorf("scrollCutoff(100) = %d, want 0 (scrolled past the start of history)", got)
	}
}

func TestRenderScrollback_ShowsEarlierContentNotTheLatestLine(t *testing.T) {
	history := []byte("first\nsecond\nthird\n")
	out := renderScrollback(history, 1, 20, 5)
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("renderScrollback(1 line back) missing earlier content: %q", out)
	}
	if strings.Contains(out, "third") {
		t.Errorf("renderScrollback(1 line back) should not show the most recent line yet: %q", out)
	}
}
