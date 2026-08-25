package cli

import "testing"

// resetUpdateSelectorFlags restores the package-level flag vars
// resolveUpdateSelector reads, so tests don't leak state into each other
// (these are normally set by cobra flag parsing, which isn't happening in
// these unit tests).
func resetUpdateSelectorFlags() {
	updateProfileFlag = ""
	updateAllFlag = false
	sandboxNamespaceFlag = ""
}

func TestResolveUpdateSelector_PositionalName(t *testing.T) {
	resetUpdateSelectorFlags()
	sel, ns, err := resolveUpdateSelector([]string{"claude-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Name != "claude-1" || sel.Profile != "" || sel.All {
		t.Errorf("got %+v", sel)
	}
	if ns != "" {
		t.Errorf("ns = %q, want empty (bare name has no namespace prefix)", ns)
	}
}

func TestResolveUpdateSelector_NamespacedPositionalName(t *testing.T) {
	resetUpdateSelectorFlags()
	sel, ns, err := resolveUpdateSelector([]string{"work/claude-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Name != "claude-1" || ns != "work" {
		t.Errorf("got selector=%+v ns=%q", sel, ns)
	}
}

func TestResolveUpdateSelector_ProfileFlag(t *testing.T) {
	resetUpdateSelectorFlags()
	updateProfileFlag = "claude-default"
	sandboxNamespaceFlag = "default"
	sel, ns, err := resolveUpdateSelector(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sel.Profile != "claude-default" || ns != "default" {
		t.Errorf("got selector=%+v ns=%q", sel, ns)
	}
}

func TestResolveUpdateSelector_AllFlag(t *testing.T) {
	resetUpdateSelectorFlags()
	updateAllFlag = true
	sel, _, err := resolveUpdateSelector(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sel.All {
		t.Errorf("got %+v, want All=true", sel)
	}
}

func TestResolveUpdateSelector_NoneProvidedIsUsageError(t *testing.T) {
	resetUpdateSelectorFlags()
	_, _, err := resolveUpdateSelector(nil)
	if err == nil {
		t.Fatal("expected a usage error when none of <agent-name>/--profile/--all is given")
	}
	var ce *cliError
	if !asCliError(err, &ce) {
		t.Fatalf("expected a *cliError, got %T: %v", err, err)
	}
	if ce.code != ExitUsageError {
		t.Errorf("exit code = %d, want %d", ce.code, ExitUsageError)
	}
}

func TestResolveUpdateSelector_MoreThanOneIsUsageError(t *testing.T) {
	resetUpdateSelectorFlags()
	updateProfileFlag = "claude-default"
	_, _, err := resolveUpdateSelector([]string{"claude-1"})
	if err == nil {
		t.Fatal("expected a usage error when both <agent-name> and --profile are given")
	}
	var ce *cliError
	if !asCliError(err, &ce) || ce.code != ExitUsageError {
		t.Errorf("got %v, want a usage-error cliError", err)
	}

	resetUpdateSelectorFlags()
	updateAllFlag = true
	_, _, err = resolveUpdateSelector([]string{"claude-1"})
	if err == nil {
		t.Fatal("expected a usage error when both <agent-name> and --all are given")
	}

	resetUpdateSelectorFlags()
	updateProfileFlag = "claude-default"
	updateAllFlag = true
	_, _, err = resolveUpdateSelector(nil)
	if err == nil {
		t.Fatal("expected a usage error when both --profile and --all are given")
	}
}

// asCliError is errors.As without importing errors into every test file
// that just needs this one check.
func asCliError(err error, target **cliError) bool {
	ce, ok := err.(*cliError)
	if !ok {
		return false
	}
	*target = ce
	return true
}
