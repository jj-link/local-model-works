package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
)

func TestCheckUpdatesCachesGitHubHeadAcrossRecipes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	service, err := New(database, queries, events.NewEventBus(queries), validator, filepath.Join(root, "catalog"), filepath.Join(root, "packages"))
	if err != nil {
		t.Fatal(err)
	}

	installed := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	createUpdateRecipe(t, ctx, queries, "alpha", installed, "https://github.com/MiaAI-Lab/shared", "alpha")
	createUpdateRecipe(t, ctx, queries, "beta", candidate, "https://github.com/MiaAI-Lab/shared.git", "beta")
	createUpdateRecipe(t, ctx, queries, "local-only", installed, "https://fixtures.local/local-only", ".")

	resolveCalls := 0
	service.resolveGitHead = func(_ context.Context, remote string) (string, string, error) {
		resolveCalls++
		if remote != "https://github.com/MiaAI-Lab/shared" {
			t.Fatalf("normalized remote = %q", remote)
		}
		return "main", candidate, nil
	}

	statuses, err := service.CheckUpdatesNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolveCalls)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want 2: %+v", len(statuses), statuses)
	}
	byRevision := map[string]UpdateStatus{}
	for _, status := range statuses {
		byRevision[status.InstalledRevision] = status
	}
	if status := byRevision[installed]; status.State != "available" || status.CandidateRevision != candidate {
		t.Fatalf("available status = %+v", status)
	}
	if status := byRevision[candidate]; status.State != "current" || status.CandidateRevision != candidate {
		t.Fatalf("current status = %+v", status)
	}

	items, err := service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	updates := map[string]*UpdateStatus{}
	for _, item := range items {
		updates[item.Name] = item.Update
	}
	if updates["alpha"] == nil || updates["alpha"].State != "available" {
		t.Fatalf("alpha cached update = %+v", updates["alpha"])
	}
	if updates["beta"] == nil || updates["beta"].State != "current" {
		t.Fatalf("beta cached update = %+v", updates["beta"])
	}
	if updates["local-only"] != nil {
		t.Fatalf("non-GitHub recipe update = %+v", updates["local-only"])
	}

	if _, err := service.checkUpdates(ctx, time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if resolveCalls != 1 {
		t.Fatalf("fresh cached check called resolver %d times", resolveCalls)
	}
}

func TestNormalizeGitHubRemoteRejectsUnsafeSources(t *testing.T) {
	valid, ok := normalizeGitHubRemote("https://github.com/MiaAI-Lab/example")
	if !ok || valid != "https://github.com/MiaAI-Lab/example" {
		t.Fatalf("valid remote = %q, %t", valid, ok)
	}
	for _, raw := range []string{
		"http://github.com/MiaAI-Lab/example",
		"https://example.com/MiaAI-Lab/example",
		"file:///etc/passwd",
		"https://github.com/MiaAI-Lab/example/extra",
		"https://user@github.com/MiaAI-Lab/example",
	} {
		if normalized, accepted := normalizeGitHubRemote(raw); accepted {
			t.Fatalf("unsafe remote %q normalized to %q", raw, normalized)
		}
	}
}

func createUpdateRecipe(t *testing.T, ctx context.Context, queries *db.Queries, name, revision, remote, sourcePath string) {
	t.Helper()
	manifest := &Manifest{
		APIVersion: APIVersion,
		Kind:       "Recipe",
		Metadata: Metadata{
			Name:        name,
			Version:     "1.0.0",
			Description: "update fixture",
			License:     "MIT",
			Source:      &Source{URL: remote, Revision: revision, Path: sourcePath},
		},
		Artifacts: []Artifact{},
		Workloads: []Workload{},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(name))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: digest, Name: name, Version: "1.0.0", Source: `{"type":"local","path":"fixture"}`,
		Manifest: string(manifestJSON),
	}); err != nil {
		t.Fatal(err)
	}
	row, err := queries.GetRecipe(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachRepositoryVersion(ctx, queries, manifest, digest, "", row.InstalledAt); err != nil {
		t.Fatal(err)
	}
}
