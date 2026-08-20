package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestRecipeValidateAndPack(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "recipe", "testdata", "pass-single-node")
	var validated bytes.Buffer
	if err := runRecipeValidate([]string{dir}, &validated); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !strings.Contains(validated.String(), `"valid":true`) {
		t.Fatalf("validate output = %s", validated.String())
	}
	output := filepath.Join(t.TempDir(), "layout")
	var packed bytes.Buffer
	if err := runRecipePack([]string{"--output", output, dir}, &packed); err != nil {
		t.Fatalf("pack: %v", err)
	}
	if err := recipe.VerifyLayout(output); err != nil {
		t.Fatalf("packed layout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "blobs", "sha256")); err != nil {
		t.Fatal(err)
	}
	if err := runRecipePack([]string{"--output", output, dir}, &bytes.Buffer{}); err == nil {
		t.Fatal("pack overwrote existing output")
	}
}

func TestRecipeInitInspectsPinnedGitWithoutExecuting(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "init", "-q")
	runGitTest(t, repo, "config", "user.name", "LMW Test")
	runGitTest(t, repo, "config", "user.email", "lmw@example.test")
	marker := filepath.Join(t.TempDir(), "must-not-exist")
	script := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(repo, "start.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("inspect me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", ".")
	runGitTest(t, repo, "commit", "-q", "-m", "fixture")

	output := filepath.Join(t.TempDir(), "draft")
	var stdout bytes.Buffer
	if err := runRecipeInit([]string{"--from-git", repo, "--revision", "HEAD", "--output", output}, &stdout); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository lifecycle script executed: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(output, "recipe.draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report draftReport
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Revision) != 40 || len(report.Tree) != 40 || len(report.Candidates) != 2 {
		t.Fatalf("draft report = %+v", report)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code != "recipe.draft_incomplete" {
			t.Fatalf("diagnostic = %+v", diagnostic)
		}
	}
	if _, err := os.Stat(filepath.Join(output, "recipe.yaml")); err != nil {
		t.Fatal(err)
	}
}

func runGitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
