package repositorycompiler

import (
	"context"
	"errors"
	"io/fs"
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
	materializeTemplate(t, "qwen38-27b-rtx6000pro-dflash2", checkout, func(string) bool { return true })
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

func TestManagedCompilersAreDeterministicAndRejectLayoutChanges(t *testing.T) {
	validator := mustValidator(t)
	qwenCheckout := t.TempDir()
	materializeTemplate(t, "qwen38-27b-rtx6000pro-dflash2", qwenCheckout, func(path string) bool {
		return strings.HasPrefix(path, "patch/sglang/")
	})
	qwenSource := recipe.RepositorySource{
		RepositoryID: repositoryID(t, QwenRepositoryURL), URL: QwenRepositoryURL, Path: ".",
		CommitSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40),
	}
	qwen := &MiaQwenSGLangCompiler{validator: validator}
	assertDeterministic(t, qwen, qwenSource, qwenCheckout)
	compiledQwen, err := qwen.Compile(context.Background(), qwenSource, qwenCheckout, nil)
	if err != nil {
		t.Fatal(err)
	}
	qwenManifest, err := recipe.Parse(compiledQwen.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if qwenManifest.Metadata.Name != "qwen38-27b-radixark-nvfp4-dflash2-avt" ||
		qwenManifest.Metadata.Version != "1.0.3" ||
		qwenManifest.Metadata.License != "Apache-2.0" {
		t.Fatalf("managed Qwen metadata = %+v", qwenManifest.Metadata)
	}
	if err := os.Remove(filepath.Join(qwenCheckout, filepath.FromSlash(qwenManagedLicense))); err != nil {
		t.Fatal(err)
	}
	assertDeterministic(t, qwen, qwenSource, qwenCheckout)
	if err := os.WriteFile(filepath.Join(qwenCheckout, "patch", "sglang", "unexpected.py"), []byte("pass\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = qwen.Compile(context.Background(), qwenSource, qwenCheckout, nil)
	var packErr *recipe.PackError
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("unexpected layout error = %v", err)
	}

	deepCheckout := t.TempDir()
	for _, name := range []string{".env.dspark.example", "docker-compose.dspark.yml", "dspark-numeric-knobs.sh"} {
		if err := os.WriteFile(filepath.Join(deepCheckout, name), []byte("PINNED=\"value\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	deepSource := recipe.RepositorySource{
		RepositoryID: repositoryID(t, DeepSeekRepositoryURL), URL: DeepSeekRepositoryURL, Path: ".",
		CommitSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40),
	}
	assertDeterministic(t, &MiaDeepSeekDSparkCompiler{validator: validator}, deepSource, deepCheckout)
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

func materializeTemplate(t *testing.T, template, destination string, include func(string) bool) {
	t.Helper()
	err := fs.WalkDir(recipeassets.Templates, template, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(template, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !include(rel) {
			return nil
		}
		content, err := recipeassets.Templates.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
