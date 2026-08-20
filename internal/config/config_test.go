package config

import (
	"strings"
	"testing"
)

func TestNormalizedPublicAgentURL(t *testing.T) {
	cfg := Server{ServerName: "lmw.example.test", PublicAgentURL: "https://LMW.EXAMPLE.TEST:9443/"}
	got, err := cfg.NormalizedPublicAgentURL()
	if err != nil {
		t.Fatalf("valid agent URL: %v", err)
	}
	if got != "https://lmw.example.test:9443" {
		t.Fatalf("normalized URL = %q", got)
	}
	for _, value := range []string{"", "http://lmw.example.test:9443", "https://other.example.test:9443", "https://lmw.example.test/path"} {
		cfg.PublicAgentURL = value
		if _, err := cfg.NormalizedPublicAgentURL(); err == nil {
			t.Fatalf("invalid URL %q accepted", value)
		}
	}
}

func TestLoadServerRequiresExplicitPublicURLs(t *testing.T) {
	t.Setenv(EnvPublicOrigin, "https://ui.example.test")
	t.Setenv(EnvPublicAgentURL, "https://agent.example.test:9443")
	t.Setenv(EnvServerName, "agent.example.test")
	cfg := LoadServer()
	if !strings.Contains(cfg.PublicOrigin, "ui.example.test") || !strings.Contains(cfg.PublicAgentURL, "agent.example.test") {
		t.Fatalf("server public URLs not loaded: %+v", cfg)
	}
}
