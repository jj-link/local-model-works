package recipe_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipe/repositorycompiler"
)

func TestInstallRepositoryCommitKeepsOldVersionAndRejectsMovedHead(t *testing.T) {
	ctx := context.Background()
	repositoryPath := filepath.Join(t.TempDir(), "native-repository")
	if err := os.MkdirAll(repositoryPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRun(t, repositoryPath, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	writeNativeBundle(t, repositoryPath, "1.0.0", repositoryPath, "first")
	c1 := commitRepository(t, repositoryPath, "c1")

	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "recipe.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	queries := db.New(database)
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	packageRoot := filepath.Join(t.TempDir(), "packages")
	t.Cleanup(func() { _ = recipe.RemovePackage(packageRoot) })
	service, err := recipe.New(database, queries, events.NewEventBus(queries), validator, "", t.TempDir(), packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	service.SetRepositoryCompilerRegistry(repositorycompiler.NewRegistry(validator))
	first, err := service.Import(ctx, recipe.RecipeSource{Type: "git", Remote: repositoryPath, Revision: c1})
	if err != nil {
		t.Fatal(err)
	}
	repositoryID, normalizedURL, normalizedPath, err := recipe.RepositoryIdentity(recipe.Source{URL: repositoryPath, Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := queries.UpsertRecipeRepository(ctx, db.UpsertRecipeRepositoryParams{
		ID: repositoryID, SourceUrl: normalizedURL, SourcePath: normalizedPath,
		TrackingRef: "HEAD", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.AttachRecipeRepositoryVersion(ctx, db.AttachRecipeRepositoryVersionParams{
		RepositoryID: repositoryID, RecipeDigest: first.Digest, CommitSha: c1,
		Canonical: 1, InstalledAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.SetRecipeRepositoryCurrent(ctx, db.SetRecipeRepositoryCurrentParams{
		CurrentDigest: sql.NullString{String: first.Digest, Valid: true}, UpdatedAt: now, ID: repositoryID,
	}); err != nil {
		t.Fatal(err)
	}

	writeNativeBundle(t, repositoryPath, "1.1.0", repositoryPath, "second")
	c2 := commitRepository(t, repositoryPath, "c2")
	candidate, err := service.PreviewRepositoryCommit(ctx, repositoryID, c2)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Digest == first.Digest {
		t.Fatal("new commit reused the old package digest")
	}
	afterPreview, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPreview.Versions) != 1 || afterPreview.Current == nil || afterPreview.Current.Digest != first.Digest || afterPreview.InstalledCommit != c1 {
		t.Fatalf("preview changed repository: %+v", afterPreview)
	}
	if _, err := service.Get(ctx, candidate.Digest); !errors.Is(err, recipe.ErrUnknown) {
		t.Fatalf("preview persisted candidate recipe: %v", err)
	}
	if _, err := os.Stat(filepath.Join(packageRoot, strings.TrimPrefix(candidate.Digest, "sha256:"))); !os.IsNotExist(err) {
		t.Fatalf("preview persisted candidate package: %v", err)
	}
	second, err := service.StageRepositoryCommit(ctx, repositoryID, c2)
	if err != nil {
		t.Fatal(err)
	}
	afterStage, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterStage.Versions) != 2 || afterStage.Current == nil ||
		afterStage.Current.Digest != first.Digest || afterStage.InstalledCommit != c1 {
		t.Fatalf("stage changed repository current: %+v", afterStage)
	}
	if _, err := os.Stat(filepath.Join(packageRoot, strings.TrimPrefix(candidate.Digest, "sha256:"))); err != nil {
		t.Fatalf("stage did not persist candidate package: %v", err)
	}
	if err := recipe.ActivateRepositoryVersion(ctx, queries, repositoryID, second.Digest); err != nil {
		t.Fatal(err)
	}
	if second.Digest != candidate.Digest {
		t.Fatalf("installed digest %q != preview digest %q", second.Digest, candidate.Digest)
	}
	if second.Digest == first.Digest {
		t.Fatal("new commit reused the old package digest")
	}
	if _, err := service.Get(ctx, first.Digest); err != nil {
		t.Fatalf("old digest is no longer addressable: %v", err)
	}
	repository, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repository.Versions) != 2 || repository.Current == nil || repository.Current.Digest != second.Digest || repository.InstalledCommit != c2 {
		t.Fatalf("repository versions = %+v", repository)
	}

	writeNativeBundle(t, repositoryPath, "1.2.0", repositoryPath, "third")
	_ = commitRepository(t, repositoryPath, "c3")
	_, err = service.InstallRepositoryCommit(ctx, repositoryID, c2)
	var packError *recipe.PackError
	if !errors.As(err, &packError) || packError.Code != "recipe.update_stale" {
		t.Fatalf("moved HEAD error = %v", err)
	}
	repositoryAfterStale, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if repositoryAfterStale.Current == nil || repositoryAfterStale.Current.Digest != second.Digest || len(repositoryAfterStale.Versions) != 2 {
		t.Fatalf("stale update changed repository: %+v", repositoryAfterStale)
	}
}

func writeNativeBundle(t *testing.T, root, version, remote, marker string) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("f", 64)
	manifest := fmt.Sprintf(`apiVersion: localmodelworks/v1alpha1
kind: Recipe
metadata:
  name: native-smoke
  version: %s
  displayName: Native smoke
  description: Native repository update smoke fixture.
  license: MIT
  source:
    url: %q
    revision: "0000000000000000000000000000000000000000"
    path: .
compatibility:
  nodeCount: 1
artifacts: []
workloads:
  - image:
      reference: example.invalid/native@%s
      digest: %s
    command: [/bin/sh, /lmw/assets/serve.sh]
    args: []
    resources: {cpu: 1, memoryBytes: 16777216, pids: 64}
assets: [serve.sh]
`, version, remote, digest, digest)
	if err := os.WriteFile(filepath.Join(root, "recipe.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "serve.sh"), []byte("#!/bin/sh\necho "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func commitRepository(t *testing.T, root, message string) string {
	t.Helper()
	if _, err := gitRun(t, root, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRun(t, root, "commit", "-q", "-m", message); err != nil {
		t.Fatal(err)
	}
	commit, err := gitOutput(t, root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return commit
}
