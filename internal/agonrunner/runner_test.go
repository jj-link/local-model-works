package agonrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedRootPreflight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(path, []byte("expected"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runPreflight([]string{"--sentinel", path, "--expect", "expected"}); err != nil {
		t.Fatal(err)
	}
	if err := runPreflight([]string{"--sentinel", path, "--expect", "wrong"}); err == nil || err.Error() != "autoresearch.runner_not_colocated" {
		t.Fatalf("mismatch error = %v", err)
	}
}

func TestFactoryCommandsResolveShippedPrompts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_PLUGIN_ROOT", root)
	for _, factory := range []string{"idea", "proposal", "deep_lit", "experiment", "paper", "paper-edit", "paper-compile"} {
		role, prompt, task, err := commandForFactory(factory)
		if err != nil || role == "" || task == "" || !strings.HasPrefix(prompt, root+string(filepath.Separator)) {
			t.Fatalf("factory %s = %q %q %q %v", factory, role, prompt, task, err)
		}
	}
	if _, _, _, err := commandForFactory("unknown"); err == nil {
		t.Fatal("accepted unknown factory")
	}
}

func TestSSHPreflightIsExplicit(t *testing.T) {
	if err := preflightSSH(context.Background(), projectConfig{Input: map[string]any{}}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	credentials := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentials, "spark-key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LMW_CREDENTIAL_DIR", credentials)
	err := preflightSSH(context.Background(), projectConfig{Input: map[string]any{"ssh_secret_name": "spark-key"}}, t.TempDir())
	if err == nil || err.Error() != "autoresearch.ssh_hosts_missing" {
		t.Fatalf("missing host error = %v", err)
	}
}
