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
		role, prompt, task, err := commandForFactory(factory, map[string]any{})
		if err != nil || role == "" || task == "" || !strings.HasPrefix(prompt, root+string(filepath.Separator)) {
			t.Fatalf("factory %s = %q %q %q %v", factory, role, prompt, task, err)
		}
	}
	role, prompt, _, err := commandForFactory("idea", map[string]any{"candidate_count": 2})
	if err != nil || role != "idea-intake-dispatcher" || filepath.Base(prompt) != "idea-intake.md" {
		t.Fatalf("idea intake = %q %q %v", role, prompt, err)
	}
	role, prompt, _, err = commandForFactory("idea", map[string]any{})
	if err != nil || role != "idea-dispatcher" || filepath.Base(prompt) != "idea-tick.md" {
		t.Fatalf("idea refinement = %q %q %v", role, prompt, err)
	}
	_, _, experimentTask, err := commandForFactory("experiment", map[string]any{"paper_request": "Collect exact evidence"})
	if err != nil || !strings.Contains(experimentTask, "Collect exact evidence") {
		t.Fatalf("experiment handback task = %q, %v", experimentTask, err)
	}
	_, _, releaseTask, err := commandForFactory("paper", map[string]any{"release": true})
	if err != nil || !strings.Contains(releaseTask, "stale reviews") || !strings.Contains(releaseTask, "human_release") {
		t.Fatalf("release task = %q, %v", releaseTask, err)
	}
	if _, _, _, err := commandForFactory("unknown", nil); err == nil {
		t.Fatal("accepted unknown factory")
	}
}

func TestCodexProviderCommandPinsConfiguredBaseURL(t *testing.T) {
	root := t.TempDir()
	prompt := filepath.Join(root, "prompt.md")
	output := filepath.Join(root, "output.txt")
	if err := os.WriteFile(prompt, []byte("prompt"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := filepath.Join(root, "credentials")
	if err := os.Mkdir(credentials, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentials, "spark-local"), []byte("lmw-local"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LMW_CREDENTIAL_DIR", credentials)
	command, err := providerCommand(context.Background(), AgentOptions{
		RunID: "run", InvocationID: "invocation", Role: "idea-creator", Backend: "codex",
		Model: "deepseek", BaseURL: "http://100.92.139.82:8888/v1/", SecretName: "spark-local",
		WorkingDirectory: root, PromptPath: prompt, OutputPath: output, Task: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Join(command.Args, " ")
	if !strings.Contains(arguments, `openai_base_url="http://100.92.139.82:8888/v1"`) {
		t.Fatalf("codex arguments = %s", arguments)
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
