package control

import (
	"encoding/json"
	"testing"
)

func TestRequestResponse_JSONRoundTrip(t *testing.T) {
	req := Request{
		Type:    TypeSandboxRun,
		Payload: mustMarshal(SandboxRunRequest{Profile: "claude-default", Name: "claude-1", Namespace: "default"}),
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Request
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != TypeSandboxRun {
		t.Errorf("Type = %q, want %q", got.Type, TypeSandboxRun)
	}

	var payload SandboxRunRequest
	if err := json.Unmarshal(got.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Profile != "claude-default" || payload.Name != "claude-1" || payload.Namespace != "default" {
		t.Errorf("payload = %+v, want Profile=claude-default Name=claude-1 Namespace=default", payload)
	}
}

func TestResponse_ErrorRoundTrip(t *testing.T) {
	resp := errResp(errFor("sandbox default/x not found"))
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Response
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OK {
		t.Errorf("OK = true, want false")
	}
	if got.Error != "sandbox default/x not found" {
		t.Errorf("Error = %q", got.Error)
	}
}

func TestSandboxUpdateRequest_SelectorRoundTrip(t *testing.T) {
	req := SandboxUpdateRequest{
		Selector:  UpdateSelector{Profile: "claude-default"},
		Namespace: "default",
		AllowURLs: []string{"https://example.com"},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got SandboxUpdateRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Selector.Profile != "claude-default" || got.Selector.Name != "" || got.Selector.All {
		t.Errorf("Selector = %+v, want only Profile set", got.Selector)
	}
	if len(got.AllowURLs) != 1 || got.AllowURLs[0] != "https://example.com" {
		t.Errorf("AllowURLs = %v", got.AllowURLs)
	}
}

// errFor is a tiny helper so this file doesn't need to import "errors" just
// for one test.
type simpleErr string

func (e simpleErr) Error() string { return string(e) }
func errFor(msg string) error     { return simpleErr(msg) }
