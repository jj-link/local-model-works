package fakeagent

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestSystemInfoPublishesEnrollmentIdentity(t *testing.T) {
	server := NewServer(t, "", "127.0.0.1:0")
	defer server.Stop()
	client := login(t, server)
	response, err := client.Get("http://" + server.HTTPAddr + "/api/v1/system/info")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("system info status = %d", response.StatusCode)
	}
	var body struct {
		AgentURL      string `json:"agent_url"`
		CAFingerprint string `json:"ca_fingerprint"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.AgentURL != "https://localhost:9443" {
		t.Fatalf("agent_url = %q", body.AgentURL)
	}
	if body.CAFingerprint != server.CA.Fingerprint() {
		t.Fatalf("ca_fingerprint = %q, want %q", body.CAFingerprint, server.CA.Fingerprint())
	}
}
