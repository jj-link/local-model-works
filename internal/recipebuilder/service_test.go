package recipebuilder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestDraftPersistsHashedCandidatesAndRejectsStaleUpdate(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	service := New(db.New(database), state, validator, nil)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "start.sh"), []byte("docker run --privileged image\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "asset.txt"), []byte("asset\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("asset.txt", filepath.Join(source, "link")); err != nil {
		t.Skip(err)
	}
	draft, err := service.CreateFromDir(ctx, map[string]string{"type": "git"}, strings.Repeat("a", 40), strings.Repeat("b", 40), nil, source)
	if err != nil {
		t.Fatal(err)
	}
	if draft.State != "needs_input" || len(draft.Candidates) != 2 {
		t.Fatalf("draft = %+v", draft)
	}
	codes := map[string]bool{}
	for _, diagnostic := range draft.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes["recipe.draft_symlink"] || !codes["recipe.draft_host_lifecycle"] {
		t.Fatalf("diagnostics = %+v", draft.Diagnostics)
	}
	for _, candidate := range draft.Candidates {
		stored := filepath.Join(state, "drafts", draft.ID, "source", "sha256-"+candidate.SHA256)
		if info, err := os.Stat(stored); err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("candidate %s mode=%v err=%v", candidate.Path, info.Mode().Perm(), err)
		}
	}
	manifest, _ := json.Marshal(map[string]any{"invalid": true})
	updated, err := service.Update(ctx, draft.ID, draft.Version, manifest, []string{draft.Candidates[0].SHA256})
	if err != nil || updated.Version != draft.Version+1 || updated.State != "needs_input" {
		t.Fatalf("updated = %+v, err=%v", updated, err)
	}
	if _, err := service.Update(ctx, draft.ID, draft.Version, manifest, nil); err == nil || !strings.Contains(err.Error(), "version_conflict") {
		t.Fatalf("stale update error = %v", err)
	}
	selectedJSON, _ := json.Marshal(updated.SelectedAssets)
	diagnosticsJSON, _ := json.Marshal(updated.Diagnostics)
	rows, err := service.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: "analyzing", Manifest: string(updated.Manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), ID: draft.ID, Version: updated.Version,
	})
	if err != nil || rows != 1 {
		t.Fatalf("reserve draft: rows=%d err=%v", rows, err)
	}
	if _, err := service.Update(ctx, draft.ID, updated.Version+1, manifest, nil); err == nil || !strings.Contains(err.Error(), "draft_busy") {
		t.Fatalf("concurrent update during install = %v", err)
	}
	if err := service.Delete(ctx, draft.ID); err == nil || !strings.Contains(err.Error(), "draft_busy") {
		t.Fatalf("concurrent delete during install = %v", err)
	}
	rows, err = service.q.UpdateRecipeDraft(ctx, db.UpdateRecipeDraftParams{
		State: "needs_input", Manifest: string(updated.Manifest), SelectedAssets: string(selectedJSON),
		Diagnostics: string(diagnosticsJSON), ID: draft.ID, Version: updated.Version + 1,
	})
	if err != nil || rows != 1 {
		t.Fatalf("release draft: rows=%d err=%v", rows, err)
	}
	if err := service.Delete(ctx, draft.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "drafts", draft.ID)); !os.IsNotExist(err) {
		t.Fatalf("draft source remains after delete: %v", err)
	}
}

func TestUpdateDraftStartsFromInstalledManifest(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	database, err := db.Open(ctx, filepath.Join(state, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	recipes, err := recipe.New(
		database,
		queries,
		events.NewEventBus(queries),
		validator,
		"",
		filepath.Join(state, "catalog"),
		filepath.Join(state, "recipes"),
	)
	if err != nil {
		t.Fatal(err)
	}
	baseDigest := "sha256:" + strings.Repeat("a", 64)
	baseRevision := strings.Repeat("b", 40)
	baseManifest, err := json.Marshal(recipe.Manifest{
		APIVersion: recipe.APIVersion,
		Kind:       "Recipe",
		Metadata: recipe.Metadata{
			Name: "update-base", Version: "1.0.2", DisplayName: "Update base",
			Description: "base", License: "MIT",
			Source: &recipe.Source{URL: "https://github.com/example/base", Revision: baseRevision, Path: "."},
		},
		Assets: []string{"start.sh", "missing.sh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: baseDigest, Name: "update-base", Version: "1.0.2",
		Source: `{"type":"local","path":"fixture"}`, TrustState: recipe.TrustLocal,
		Manifest: string(baseManifest),
	}); err != nil {
		t.Fatal(err)
	}

	service := New(queries, state, validator, recipes)
	candidateRevision := strings.Repeat("c", 40)
	source := GitSource{
		Remote: "https://github.com/example/base.git", Revision: candidateRevision,
		Path: ".", BaseRecipeDigest: baseDigest,
	}
	updateManifest, err := service.updateManifest(ctx, source, candidateRevision)
	if err != nil {
		t.Fatal(err)
	}
	var updated recipe.Manifest
	if err := json.Unmarshal(updateManifest, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Metadata.Version != "1.0.3" {
		t.Fatalf("updated version = %q", updated.Metadata.Version)
	}
	if updated.Metadata.Source == nil || updated.Metadata.Source.Revision != candidateRevision {
		t.Fatalf("updated source = %+v", updated.Metadata.Source)
	}

	candidateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(candidateDir, "start.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	draft, err := service.CreateFromDir(ctx, source, candidateRevision, strings.Repeat("d", 40), updateManifest, candidateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(draft.SelectedAssets) != 1 {
		t.Fatalf("selected assets = %+v", draft.SelectedAssets)
	}
	missing := false
	for _, diagnostic := range draft.Diagnostics {
		if diagnostic.Code == "recipe.draft_asset_missing" && diagnostic.Path == "missing.sh" {
			missing = true
		}
	}
	if !missing {
		t.Fatalf("missing asset diagnostic absent: %+v", draft.Diagnostics)
	}
}
