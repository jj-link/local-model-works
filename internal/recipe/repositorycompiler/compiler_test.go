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

	dgxCheckout := t.TempDir()
	for _, name := range []string{"README.md", ".env.sample", "start.sh", "stop.sh"} {
		if err := os.WriteFile(filepath.Join(dgxCheckout, name), []byte("# reviewed upstream contract\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dgxSource := recipe.RepositorySource{
		RepositoryID: repositoryID(t, QwenDGXSparkRepositoryURL), URL: QwenDGXSparkRepositoryURL, Path: ".",
		CommitSHA: strings.Repeat("7", 40), TreeSHA: strings.Repeat("8", 40),
	}
	dgxCompiler := &MiaQwenDGXSparkCompiler{validator: validator}
	assertDeterministic(t, dgxCompiler, dgxSource, dgxCheckout)
	compiledDGX, err := dgxCompiler.Compile(context.Background(), dgxSource, dgxCheckout, nil)
	if err != nil {
		t.Fatal(err)
	}
	dgxManifest, err := recipe.Parse(compiledDGX.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if dgxManifest.Metadata.Name != "MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark" ||
		dgxManifest.Metadata.Version != "1.0.0" ||
		dgxManifest.Metadata.Model != "qwen3.8-27b-sglang" ||
		dgxManifest.Metadata.Engine != "sglang" ||
		dgxManifest.Metadata.License != "MIT" ||
		dgxManifest.Metadata.Source == nil ||
		dgxManifest.Metadata.Source.Revision != strings.Repeat("7", 40) {
		t.Fatalf("managed DGX Spark metadata = %+v", dgxManifest.Metadata)
	}
	if dgxManifest.Compatibility.NodeCount != 1 ||
		dgxManifest.Compatibility.Accelerator == nil ||
		len(dgxManifest.Compatibility.Accelerator.Architectures) != 1 ||
		dgxManifest.Compatibility.Accelerator.Architectures[0] != "sm_121" ||
		dgxManifest.Compatibility.Accelerator.Count != 1 ||
		dgxManifest.Compatibility.Accelerator.MinMemoryBytes != 128849018880 {
		t.Fatalf("managed DGX Spark compatibility = %+v", dgxManifest.Compatibility)
	}
	if len(dgxManifest.Artifacts) != 1 {
		t.Fatalf("DGX Spark artifacts = %d", len(dgxManifest.Artifacts))
	}
	dgxModel := dgxManifest.Artifacts[0]
	if dgxModel.SizeBytes != 23772921363 || dgxModel.Mount != "/models/base" || dgxModel.DefaultVariant != "packed_fp4_current" ||
		len(dgxModel.Variants) != 2 || dgxModel.Variants[1].Name != "dense_bf16_head" ||
		dgxModel.Variants[0].Source.Identity != "hf://RadixArk/Qwen3.8-27B-NVFP4" ||
		dgxModel.Variants[0].Source.Revision != "91cea059647696fd83964e43d57db122ff745993" ||
		dgxModel.Variants[1].Source.Identity != "hf://RadixArk/Qwen3.8-27B-NVFP4-BF16-LMHead" ||
		dgxModel.Variants[1].Source.Revision != "009632fef96dd349150baa780c984e62e70e91fe" {
		t.Fatalf("managed DGX Spark model artifact = %+v", dgxModel)
	}
	if got := dgxManifest.Workloads[0].Image.Digest; got != "sha256:febfb971c7352570fc445c466ebd6ffc9d896024958e544a60f2137fd85856b1" {
		t.Fatalf("DGX Spark image digest = %q", got)
	}
	dgxWorkload := dgxManifest.Workloads[0]
	if dgxWorkload.NetworkMode != "bridge" || dgxWorkload.Resources.CPU != 10 ||
		dgxWorkload.Resources.CPUSetCpus != "5-9,15-19" ||
		dgxWorkload.Resources.MemoryBytes != 124554051584 ||
		dgxWorkload.Resources.ShmBytes != 34359738368 ||
		dgxWorkload.Resources.TmpfsBytes != 8589934592 ||
		dgxWorkload.Resources.Pids != 8192 ||
		dgxWorkload.HostPreparation == nil ||
		dgxWorkload.HostPreparation.Swappiness == nil ||
		*dgxWorkload.HostPreparation.Swappiness != 60 ||
		!dgxWorkload.HostPreparation.RequireSwap ||
		!dgxWorkload.HostPreparation.DropPageCache {
		t.Fatalf("managed DGX Spark workload resources = %+v", dgxWorkload.Resources)
	}
	dgxPort := false
	for _, p := range dgxWorkload.Ports {
		if p.Container == 8000 && p.Host == 8000 {
			dgxPort = true
		}
		if p.Container == 8888 || p.Host == 8888 {
			t.Fatalf("DGX Spark workload must not declare port 8888: %+v", dgxWorkload.Ports)
		}
	}
	if !dgxPort {
		t.Fatalf("DGX Spark workload missing port 8000: %+v", dgxWorkload.Ports)
	}
	if dgxPort && !strings.Contains(strings.Join(dgxWorkload.Args, "\n"), "--speculative-algorithm\nEAGLE") {
		t.Fatalf("DGX Spark workload missing EAGLE flags: %v", dgxWorkload.Args)
	}
	for i, arg := range dgxWorkload.Args {
		if arg == "--port" && i+1 < len(dgxWorkload.Args) && dgxWorkload.Args[i+1] != "8000" {
			t.Fatalf("DGX Spark launch port = %s, want 8000", dgxWorkload.Args[i+1])
		}
	}
	if err := os.Remove(filepath.Join(dgxCheckout, "start.sh")); err != nil {
		t.Fatal(err)
	}
	_, err = dgxCompiler.Compile(context.Background(), dgxSource, dgxCheckout, nil)
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("DGX Spark layout error = %v", err)
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

	glmCheckout := t.TempDir()
	for _, name := range []string{"Dockerfile", "start.sh", ".env.example"} {
		target := filepath.Join(glmCheckout, name)
		if err := os.WriteFile(target, []byte("# pinned upstream contract\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for asset := range glm53UpstreamAssets {
		target := filepath.Join(glmCheckout, filepath.FromSlash(asset))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("# reviewed upstream asset\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	glmSource := recipe.RepositorySource{
		RepositoryID: repositoryID(t, GLM53RepositoryURL), URL: GLM53RepositoryURL, Path: ".",
		CommitSHA: strings.Repeat("e", 40), TreeSHA: strings.Repeat("f", 40),
	}
	glmCompiler := &MiaGLM53EXL3Compiler{validator: validator}
	assertDeterministic(t, glmCompiler, glmSource, glmCheckout)
	compiledGLM, err := glmCompiler.Compile(context.Background(), glmSource, glmCheckout, nil)
	if err != nil {
		t.Fatal(err)
	}
	glmManifest, err := recipe.Parse(compiledGLM.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if glmManifest.Metadata.Name != "glm53-flash-exl3-dflash2-spark-tp2" ||
		glmManifest.Metadata.Model != "GLM-5.3-Flash-EXL3" ||
		glmManifest.Metadata.License != "MIT" ||
		glmManifest.Compatibility.NodeCount != 2 ||
		len(glmManifest.Artifacts) != 2 ||
		glmManifest.Artifacts[0].SizeBytes == 0 {
		t.Fatalf("managed GLM contract = %+v", glmManifest)
	}
	if got := glmManifest.Workloads[0].Image.Digest; got != "sha256:9bb1557a4234fce63d59599e44d10747eabd742beb337eebf9e7070be8a0fd58" {
		t.Fatalf("GLM image digest = %q", got)
	}
	if err := os.Remove(filepath.Join(glmCheckout, "overlay", "patch_glm5_drafter_group.py")); err != nil {
		t.Fatal(err)
	}
	_, err = glmCompiler.Compile(context.Background(), glmSource, glmCheckout, nil)
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("GLM layout error = %v", err)
	}

	nvfp4Checkout := t.TempDir()
	for _, name := range []string{
		"README.md",
		"launch-glm53-vllm-tp2-dflash2.sh",
		"chat_template_mm.jinja",
		"docker/sparse_attn_indexer_kpool_sm121.py",
	} {
		target := filepath.Join(nvfp4Checkout, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("# reviewed upstream contract\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nvfp4Source := recipe.RepositorySource{
		RepositoryID: repositoryID(t, GLM53NVFP4RepositoryURL), URL: GLM53NVFP4RepositoryURL, Path: ".",
		CommitSHA: strings.Repeat("1", 40), TreeSHA: strings.Repeat("2", 40),
	}
	nvfp4Compiler := &TonyGLM53NVFP4Compiler{validator: validator}
	assertDeterministic(t, nvfp4Compiler, nvfp4Source, nvfp4Checkout)
	compiledNVFP4, err := nvfp4Compiler.Compile(context.Background(), nvfp4Source, nvfp4Checkout, nil)
	if err != nil {
		t.Fatal(err)
	}
	nvfp4Manifest, err := recipe.Parse(compiledNVFP4.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if nvfp4Manifest.Metadata.Name != "glm53-flash-nvfp4-dflash2-spark-tp2" ||
		nvfp4Manifest.Metadata.Model != "glm-5.3-flash" ||
		len(nvfp4Manifest.Artifacts) != 2 ||
		len(nvfp4Manifest.Artifacts[0].Variants) != 3 ||
		nvfp4Manifest.Workloads[0].StartOrder != "workers-first" ||
		nvfp4Manifest.Workloads[0].HostPreparation == nil ||
		nvfp4Manifest.Workloads[0].HostPreparation.Swappiness == nil ||
		*nvfp4Manifest.Workloads[0].HostPreparation.Swappiness != 0 {
		t.Fatalf("managed NVFP4 contract = %+v", nvfp4Manifest)
	}
	stable := nvfp4Manifest.Artifacts[0].Variants[0]
	if stable.Name != "censored" ||
		stable.Source.Identity != "hf://RedHatAI/GLM-5.3-Flash-NVFP4" ||
		stable.Source.Revision != "36c184c6cda000a481711306df5adde42f63321a" {
		t.Fatalf("stable checkpoint = %+v", stable)
	}
	if got := nvfp4Manifest.Workloads[0].Image.Digest; got != "sha256:4def0ef644cb2e9814136dcffd5e385e21bc594f48f3b292234051904abe85a6" {
		t.Fatalf("NVFP4 image digest = %q", got)
	}
	if err := os.Remove(filepath.Join(nvfp4Checkout, "docker", "sparse_attn_indexer_kpool_sm121.py")); err != nil {
		t.Fatal(err)
	}
	_, err = nvfp4Compiler.Compile(context.Background(), nvfp4Source, nvfp4Checkout, nil)
	if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
		t.Fatalf("NVFP4 layout error = %v", err)
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
