package repositorycompiler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassets "github.com/jj-link/local-model-works/recipes"
)

func TestNativeCompilerPinsCommitDeterministically(t *testing.T) {
	validator := mustValidator(t)
	checkout := t.TempDir()
	materializeTemplate(t, "qwen38-27b-rtx6000pro-dflash2", checkout)
	compiler := &NativeBundleCompiler{validator: validator}
	commit1 := strings.Repeat("1", 40)
	commit2 := strings.Repeat("2", 40)
	source := recipe.RepositorySource{
		RepositoryID: repositoryID(t, "https://fixtures.local/native"), URL: "https://fixtures.local/native",
		Path: ".", CommitSHA: commit1, TreeSHA: strings.Repeat("3", 40),
	}
	first, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestDigest != second.ManifestDigest || string(first.ConfigJSON) != string(second.ConfigJSON) {
		t.Fatal("same native commit did not compile byte-for-byte")
	}
	source.CommitSHA = commit2
	third, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if third.ManifestDigest == first.ManifestDigest {
		t.Fatal("different native commits produced the same digest")
	}
}

func TestUpstreamCompilerVerifiesAuthoredSourceContract(t *testing.T) {
	validator := mustValidator(t)
	checkout := t.TempDir()
	template, err := recipeassets.Templates.ReadFile("deepseek-v4-flash-vision-exp-dspark-tp2/recipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document, err := recipe.YAMLOrJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".env.dspark.example", "start-deepseek-v4-flash-dspark.sh", "docker-compose.dspark.yml", "docker-compose.dspark-nfs.override.yml", "stop-deepseek-v4-flash-dspark.sh", "prepare-dspark-model-cache.sh"} {
		content, err := os.ReadFile(filepath.Join("testdata", "deepseek-v4-flash-vision-exp-dspark-tp2", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(checkout, name), content, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source := recipe.RepositorySource{
		RepositoryID: repositoryID(t, DeepSeekRepositoryURL), URL: DeepSeekRepositoryURL,
		Path: ".", CommitSHA: manifest.Metadata.Source.Revision, TreeSHA: strings.Repeat("b", 40),
	}
	compiler, ok := NewRegistry(validator).Lookup(source, checkout)
	if !ok {
		t.Fatal("reviewed upstream procedure unavailable")
	}
	assertDeterministic(t, compiler, source, checkout)
	packed, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	rootAlias := source
	rootAlias.Path = ""
	rootPackage, err := compiler.Compile(context.Background(), rootAlias, checkout, nil)
	if err != nil {
		t.Fatalf("repository-root update failed: %v", err)
	}
	if rootPackage.ManifestDigest != packed.ManifestDigest {
		t.Fatal("equivalent repository-root paths produced different saved packages")
	}
	compiled, err := recipe.Parse(packed.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Workloads[0].Upstream == nil || len(compiled.Assets) != 0 || len(compiled.Artifacts) != 0 {
		t.Fatal("source procedure was replaced by a managed runtime or asset bundle")
	}
	freshVersion, err := parseManagedVersion(compiled.Metadata.Version)
	if err != nil || compareManagedVersions(freshVersion, [3]int{2, 1, 2}) <= 0 {
		t.Fatalf("fresh compiled recipe does not supersede the previous launch contracts: %s (%v)", compiled.Metadata.Version, err)
	}
	upgraded, err := compiler.Compile(context.Background(), source, checkout, &recipe.RecipeDetail{Recipe: recipe.Recipe{Version: "2.1.2"}})
	if err != nil {
		t.Fatal(err)
	}
	upgradedManifest, err := recipe.Parse(upgraded.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	upgradedVersion, err := parseManagedVersion(upgradedManifest.Metadata.Version)
	if err != nil || compareManagedVersions(upgradedVersion, [3]int{2, 1, 2}) <= 0 {
		t.Fatalf("import cannot replace the previous compiled recipe: %s (%v)", upgradedManifest.Metadata.Version, err)
	}
	target := source
	target.CommitSHA = strings.Repeat("c", 40)
	updated, err := compiler.Compile(context.Background(), target, checkout, &recipe.RecipeDetail{Recipe: recipe.Recipe{Version: "99.0.7"}})
	if err != nil {
		t.Fatalf("compatible newer commit rejected: %v", err)
	}
	updatedManifest, err := recipe.Parse(updated.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if updatedManifest.Metadata.Source.Revision != target.CommitSHA || updatedManifest.Metadata.Version != "99.0.8" {
		t.Fatal("update did not advance the actual source pin and saved version")
	}
	wrongPath := source
	wrongPath.Path = "another-procedure"
	_, err = compiler.Compile(context.Background(), wrongPath, checkout, nil)
	var packErr *recipe.PackError
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("different authored working directory accepted: %v", err)
	}
	if err := os.Remove(filepath.Join(checkout, "prepare-dspark-model-cache.sh")); err != nil {
		t.Fatal(err)
	}
	_, err = compiler.Compile(context.Background(), source, checkout, nil)
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("missing original install script accepted: %v", err)
	}
}

func TestRegistryPrefersAuthoredNativeBundleOverReviewedProcedure(t *testing.T) {
	checkout := t.TempDir()
	materializeTemplate(t, "qwen38-27b-rtx6000pro-dflash2", checkout)
	source := recipe.RepositorySource{
		RepositoryID: repositoryID(t, QwenRepositoryURL), URL: QwenRepositoryURL,
		Path: ".", CommitSHA: strings.Repeat("d", 40),
	}
	compiler, ok := NewRegistry(mustValidator(t)).Lookup(source, checkout)
	if !ok {
		t.Fatal("authored native bundle unavailable")
	}
	packed, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse(packed.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.Source.Revision != source.CommitSHA {
		t.Fatal("native source commit was replaced by the older reviewed procedure")
	}
}

func TestIncompleteUpstreamLifecycleCannotUseGeneratedSubstitute(t *testing.T) {
	source := recipe.RepositorySource{
		RepositoryID: repositoryID(t, GLM53NVFP4RepositoryURL), URL: GLM53NVFP4RepositoryURL,
		Path: ".", CommitSHA: "050081dc41ce6edd4d3f15fa19dc3410ba4210e3",
	}
	checkout := t.TempDir()
	compiler, ok := NewRegistry(mustValidator(t)).Lookup(source, checkout)
	if !ok {
		t.Fatal("expected explicit source review boundary")
	}
	_, err := compiler.Compile(context.Background(), source, checkout, nil)
	var packErr *recipe.PackError
	if !errors.As(err, &packErr) || packErr.Code != "recipe.upstream_review_required" {
		t.Fatalf("incomplete original lifecycle was adapted automatically: %v", err)
	}
}

func TestRegistryRejectsUnsupportedImperativeRepository(t *testing.T) {
	registry := NewRegistry(mustValidator(t))
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "start.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(recipe.RepositorySource{
		RepositoryID: repositoryID(t, "https://fixtures.local/imperative"), Path: ".",
	}, checkout); ok {
		t.Fatal("imperative repository unexpectedly accepted")
	}
}

func assertDeterministic(t *testing.T, compiler recipe.RepositoryCompiler, source recipe.RepositorySource, checkout string) {
	t.Helper()
	first, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.Compile(context.Background(), source, checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestDigest != second.ManifestDigest || string(first.ManifestJSON) != string(second.ManifestJSON) || string(first.ConfigJSON) != string(second.ConfigJSON) {
		t.Fatal("managed compiler output is not deterministic")
	}
}

func mustValidator(t *testing.T) *recipe.Validator {
	t.Helper()
	validator, err := recipe.NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func repositoryID(t *testing.T, rawURL string) string {
	t.Helper()
	id, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: rawURL, Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func materializeTemplate(t *testing.T, template, destination string) {
	t.Helper()
	content, err := recipeassets.Templates.ReadFile(template + "/recipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "recipe.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
}
