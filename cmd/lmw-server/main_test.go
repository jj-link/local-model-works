package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/config"
)

func TestServerRefusesEmptyStateWithoutOperator(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	err := run(config.Server{
		StateRoot:    root,
		ConfigDir:    filepath.Join(root, "config"),
		HTTPAddr:     "127.0.0.1:0",
		AgentAddr:    "127.0.0.1:0",
		ServerName:   "localhost",
		PublicOrigin: "https://lmw.example.test",
	}, "test", "test")
	if err == nil || !strings.Contains(err.Error(), "no operator user exists") || !strings.Contains(err.Error(), "lmw admin create") {
		t.Fatalf("error = %v", err)
	}
}
