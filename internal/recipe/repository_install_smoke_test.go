package recipe_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestImportReviewedSavesWithoutDevicesAndPreservesSameCommitVersions(t *testing.T) {
	ctx := context.Background()
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
	service, err := recipe.New(database, queries, events.NewEventBus(queries), validator, t.TempDir(), packageRoot)
	if err != nil {
		t.Fatal(err)
	}
	service.SetInstallHook(func() { t.Fatal("reviewed save contacted devices") })
	sourceRoot := t.TempDir()
	remote := "https://github.com/fixture/reviewed"
	writeNativeBundle(t, sourceRoot, "1.0.0", remote, "saved helper")
	manifest, _, err := recipe.PackFromDir(sourceRoot, validator)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("a", 40)
	manifest.Metadata.Source.Revision = commit
	tree := strings.Repeat("b", 40)
	makeSource := func(description string) recipe.RecipeSource {
		t.Helper()
		manifest.Metadata.Description = description
		doc, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		packed, err := recipe.PackManifest(doc, map[string][]byte{"serve.sh": []byte("#!/bin/sh\necho saved helper\n")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		layout := t.TempDir()
		if err := recipe.WriteLayout(layout, packed); err != nil {
			t.Fatal(err)
		}
		return recipe.RecipeSource{Type: "local", Path: layout, Remote: remote, Revision: commit, Tree: tree, SourcePath: ".", TrackingRef: "refs/tags/stable"}
	}
	firstSource := makeSource("first saved configuration")
	first, err := service.ImportReviewed(ctx, firstSource, "", "")
	if err != nil {
		t.Fatal(err)
	}
	repositoryID, _, _, err := recipe.RepositoryIdentity(*manifest.Metadata.Source)
	if err != nil {
		t.Fatal(err)
	}
	secondSource := makeSource("operator corrected configuration at the same commit")
	second, err := service.ImportReviewed(ctx, secondSource, repositoryID, first.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if second.Digest == first.Digest {
		t.Fatal("changed configuration reused old digest")
	}
	repository, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if repository.Current == nil || repository.Current.Digest != second.Digest ||
		repository.TrackingRef != "refs/tags/stable" || len(repository.Versions) != 2 || len(repository.InstalledDevices) != 0 {
		t.Fatalf("reviewed save = %+v", repository)
	}
	for _, version := range repository.Versions {
		if version.CommitSHA != commit || version.TreeSHA != tree || version.Canonical != (version.Recipe.Digest == second.Digest) {
			t.Fatalf("same-commit canonical link = %+v", version)
		}
	}
	var provenance recipe.RecipeSource
	if err := json.Unmarshal(second.Source, &provenance); err != nil {
		t.Fatal(err)
	}
	if provenance.Type != "git" || provenance.Remote != remote || provenance.Path != "." || provenance.Revision != commit || provenance.Tree != tree ||
		provenance.TrackingRef != "refs/tags/stable" {
		t.Fatalf("saved provenance = %+v", provenance)
	}
	if _, err := service.ReadPackage(ctx, first.Digest); err != nil {
		t.Fatalf("old package lost: %v", err)
	}
	if replay, err := service.ImportReviewed(ctx, secondSource, repositoryID, first.Digest); err != nil || replay.Digest != second.Digest {
		t.Fatalf("current result replay = %+v, %v", replay, err)
	}
	staleSource := makeSource("stale editor")
	if _, err := service.ImportReviewed(ctx, staleSource, repositoryID, first.Digest); !packErrorCode(err, "recipe.update_stale") {
		t.Fatalf("stale save error = %v", err)
	}
	if _, err := service.ImportReviewed(ctx, firstSource, repositoryID, first.Digest); !packErrorCode(err, "recipe.update_stale") {
		t.Fatalf("old digest replay undid newer selection: %v", err)
	}
	if _, err := service.ImportReviewed(ctx, staleSource, "", ""); !packErrorCode(err, "recipe.repository_exists") {
		t.Fatalf("duplicate addition error = %v", err)
	}
	after, err := service.GetRepository(ctx, repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Current == nil || after.Current.Digest != second.Digest || len(after.Versions) != 2 {
		t.Fatalf("rejected save mutated repository: %+v", after)
	}
	deployments, err := queries.ListDeployments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 0 {
		t.Fatalf("save created deployments: %+v", deployments)
	}
}

func packErrorCode(err error, code string) bool {
	var packError *recipe.PackError
	return errors.As(err, &packError) && packError.Code == code
}

func writeNativeBundle(t *testing.T, root, version, remote, marker string) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("f", 64)
	manifest := fmt.Sprintf(`apiVersion: localmodelworks/v1alpha1
kind: Recipe
metadata:
  name: native-smoke
  version: %s
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
    resources: {pids: 64}
assets: [serve.sh]
`, version, remote, digest, digest)
	if err := os.WriteFile(filepath.Join(root, "recipe.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "serve.sh"), []byte("#!/bin/sh\necho "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
