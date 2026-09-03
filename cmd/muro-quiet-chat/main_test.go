package main

import (
	"os/exec"
	"testing"
)

func TestReplyText_ExtractsResultField(t *testing.T) {
	got := replyText([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"Ciao! Come stai?"}`))
	if got != "Ciao! Come stai?" {
		t.Errorf("replyText() = %q, want the result field's value", got)
	}
}

func TestReplyText_FallsBackToRawOutputOnUnexpectedSchema(t *testing.T) {
	raw := `not json at all`
	if got := replyText([]byte(raw)); got != raw {
		t.Errorf("replyText() = %q, want the raw output unchanged so nothing silently vanishes", got)
	}
}

func TestReplyText_FallsBackWhenResultFieldMissing(t *testing.T) {
	raw := `{"type":"result","subtype":"error_max_turns"}`
	if got := replyText([]byte(raw)); got != raw {
		t.Errorf("replyText() = %q, want the raw JSON surfaced when there's no result field to extract", got)
	}
}

func TestDescribeErr_ExtractsStderrFromExitError(t *testing.T) {
	_, err := exec.Command("sh", "-c", "echo boom >&2; exit 1").Output()
	if err == nil {
		t.Fatal("expected a non-nil error from a command that exits 1")
	}
	if got := describeErr(err); got != "boom" {
		t.Errorf("describeErr() = %q, want %q (a bare \"exit status 1\" was the whole bug this fixes)", got, "boom")
	}
}

func TestDescribeErr_FallsBackToErrorStringWhenNoStderr(t *testing.T) {
	_, err := exec.Command("sh", "-c", "exit 1").Output()
	if err == nil {
		t.Fatal("expected a non-nil error from a command that exits 1")
	}
	if got := describeErr(err); got != err.Error() {
		t.Errorf("describeErr() = %q, want %q", got, err.Error())
	}
}
