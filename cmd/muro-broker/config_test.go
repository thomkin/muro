package main

import "testing"

func TestParseFlags_Defaults(t *testing.T) {
	cfg, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags error: %v", err)
	}
	if cfg.Listen != ":1883" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, ":1883")
	}
	if cfg.authEnabled() {
		t.Error("authEnabled() = true with no credentials, want false")
	}
}

func TestParseFlags_CustomListen(t *testing.T) {
	cfg, err := parseFlags([]string{"--listen", "127.0.0.1:9999"})
	if err != nil {
		t.Fatalf("parseFlags error: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want %q", cfg.Listen, "127.0.0.1:9999")
	}
}

func TestParseFlags_AuthEnabledWhenBothSet(t *testing.T) {
	cfg, err := parseFlags([]string{"--username", "u", "--password", "p"})
	if err != nil {
		t.Fatalf("parseFlags error: %v", err)
	}
	if !cfg.authEnabled() {
		t.Error("authEnabled() = false with username+password set, want true")
	}
}

func TestParseFlags_RejectsUsernameWithoutPassword(t *testing.T) {
	if _, err := parseFlags([]string{"--username", "u"}); err == nil {
		t.Fatal("expected error for --username without --password, got nil")
	}
}

func TestParseFlags_RejectsPasswordWithoutUsername(t *testing.T) {
	if _, err := parseFlags([]string{"--password", "p"}); err == nil {
		t.Fatal("expected error for --password without --username, got nil")
	}
}

func TestParseFlags_RejectsEmptyListen(t *testing.T) {
	if _, err := parseFlags([]string{"--listen", ""}); err == nil {
		t.Fatal("expected error for empty --listen, got nil")
	}
}
