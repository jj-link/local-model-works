package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlacementCandidatesEnumerateAndValidateEveryHFRevision(t *testing.T) {
	cache := t.TempDir()
	modelRoot := filepath.Join(cache, "hub", "models--Acme--Model")
	validRevision := strings.Repeat("a", 40)
	invalidRevision := strings.Repeat("b", 40)
	validSnapshot := filepath.Join(modelRoot, "snapshots", validRevision)
	invalidSnapshot := filepath.Join(modelRoot, "snapshots", invalidRevision)
	if err := os.MkdirAll(validSnapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(invalidSnapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(validSnapshot, "config.json"), []byte(`{"model_type":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(invalidSnapshot, "weights.safetensors"), []byte("version https://git-lfs.github.com/spec/v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	candidates := placementCandidates(context.Background(), cache)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %+v", candidates)
	}
	byIdentity := map[string]placementCandidate{}
	for _, candidate := range candidates {
		byIdentity[candidate.Identity] = candidate
		if candidate.Path != modelRoot {
			t.Fatalf("placement path = %q, want repository root %q", candidate.Path, modelRoot)
		}
	}
	valid := byIdentity["hf://Acme/Model@"+validRevision]
	if valid.State != "valid" || len(valid.Diagnostics) != 0 || valid.Size == 0 {
		t.Fatalf("valid candidate = %+v", valid)
	}
	invalid := byIdentity["hf://Acme/Model@"+invalidRevision]
	if invalid.State != "invalid" || len(invalid.Diagnostics) == 0 {
		t.Fatalf("invalid candidate = %+v", invalid)
	}
}

func TestPlacementCandidatesRecognizeRootHubLayout(t *testing.T) {
	root := t.TempDir()
	revision := strings.Repeat("c", 40)
	snapshot := filepath.Join(root, "models--Org--Repo", "snapshots", revision)
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidates := placementCandidates(context.Background(), root)
	if len(candidates) != 1 || candidates[0].Identity != "hf://Org/Repo@"+revision {
		t.Fatalf("candidates = %+v", candidates)
	}
}
