// Package repositorycompiler contains deterministic, non-executing compilers
// for native recipe bundles and explicitly supported third-party repositories.
package repositorycompiler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassets "github.com/jj-link/local-model-works/recipes"
)

const (
	QwenRepositoryURL         = "https://github.com/MiaAI-Lab/Qwen3.8-27B-RTX-6000-PRO-SGLang-DSpark"
	DeepSeekRepositoryURL     = "https://github.com/MiaAI-Lab/DeepSeek-v4-Flash-DSpark-2x-DGX-Spark"
	GLM53RepositoryURL        = "https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks"
	GLM53NVFP4RepositoryURL   = "https://github.com/tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark"
	QwenDGXSparkRepositoryURL = "https://github.com/MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark"
)

const qwenManagedLicense = "patch/sglang/LICENSE"

var glm53UpstreamAssets = map[string]struct{}{
	"files/chat_template.jinja":                    {},
	"overlay/patch_glm_video_placeholders.py":      {},
	"overlay/patch_suppress_stops_in_reasoning.py": {},
	"overlay/patch_scheduler_decode_floor.py":      {},
	"overlay/patch_glm5_drafter_group.py":          {},
	"overlay/patch_hybrid_prefix_hit.py":           {},
	"scripts/boot-shape-warmup.sh":                 {},
}

var glm53NVFP4UpstreamAssets = map[string]struct{}{
	"chat_template_mm.jinja":                    {},
	"docker/sparse_attn_indexer_kpool_sm121.py": {},
}

type Compiler = recipe.RepositoryCompiler

// Registry selects an explicit driver before falling back to a native bundle.
type Registry struct {
	validator *recipe.Validator
	drivers   map[string]Compiler
}

func NewRegistry(validator *recipe.Validator) *Registry {
	qwenID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: QwenRepositoryURL, Path: "."})
	deepSeekID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: DeepSeekRepositoryURL, Path: "."})
	glm53ID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: GLM53RepositoryURL, Path: "."})
	glm53NVFP4ID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: GLM53NVFP4RepositoryURL, Path: "."})
	qwenDGXID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: QwenDGXSparkRepositoryURL, Path: "."})
	return &Registry{
		validator: validator,
		drivers: map[string]Compiler{
			qwenID:       &MiaQwenSGLangCompiler{validator: validator},
			deepSeekID:   &MiaDeepSeekDSparkCompiler{validator: validator},
			glm53ID:      &MiaGLM53EXL3Compiler{validator: validator},
			glm53NVFP4ID: &TonyGLM53NVFP4Compiler{validator: validator},
			qwenDGXID:    &MiaQwenDGXSparkCompiler{validator: validator},
		},
	}
}

func (r *Registry) Lookup(source recipe.RepositorySource, checkout string) (Compiler, bool) {
	if compiler, ok := r.drivers[source.RepositoryID]; ok {
		return compiler, true
	}
	sourceRoot, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, false
	}
	for _, name := range []string{"recipe.yaml", "recipe.json"} {
		if info, statErr := os.Stat(filepath.Join(sourceRoot, name)); statErr == nil && info.Mode().IsRegular() {
			return &NativeBundleCompiler{validator: r.validator}, true
		}
	}
	return nil, false
}

func (r *Registry) SupportsRepository(repositoryID string) bool {
	_, ok := r.drivers[repositoryID]
	return ok
}

// NativeBundleCompiler delegates native declarative bundles to the canonical
// packer without modifying the checkout.
type NativeBundleCompiler struct {
	validator *recipe.Validator
}

func (c *NativeBundleCompiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	_, packed, err := recipe.PackRepositoryDir(root, c.validator, recipe.Source{
		URL: source.URL, Path: source.Path, Revision: source.CommitSHA,
	})
	if err != nil {
		return nil, err
	}
	return packed, nil
}

// MiaQwenDGXSparkCompiler maps the upstream imperative DGX Spark
// distribution to a fully declarative single-node LMW workload. The
// reviewed start/stop scripts are required for layout drift detection
// but never executed; the controller-owned template carries the whole
// runtime contract.
type MiaQwenDGXSparkCompiler struct {
	validator *recipe.Validator
}

func (c *MiaQwenDGXSparkCompiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"README.md", ".env.sample", "start.sh", "stop.sh"} {
		info, statErr := os.Lstat(filepath.Join(root, required))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: required, Message: "required upstream file is missing or unsafe"}
		}
	}
	return compileManagedAssets(source, checkout, "qwen38-27b-dgx-spark-mtp", nil, false, c.validator)
}

// MiaQwenSGLangCompiler combines the controller-owned runtime template with
// the exact allow-listed SGLang patch files from the pinned upstream commit.
type MiaQwenSGLangCompiler struct {
	validator *recipe.Validator
}

func (c *MiaQwenSGLangCompiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	return compileManaged(source, checkout, "qwen38-27b-rtx6000pro-dflash2", true, c.validator)
}

// MiaDeepSeekDSparkCompiler retains the declarative two-node runtime contract
// and validates that the pinned upstream layout remains the supported one.
type MiaDeepSeekDSparkCompiler struct {
	validator *recipe.Validator
}

func (c *MiaDeepSeekDSparkCompiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	for _, required := range []string{".env.dspark.example", "docker-compose.dspark.yml", "dspark-numeric-knobs.sh"} {
		info, statErr := os.Lstat(filepath.Join(root, required))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: required, Message: "required upstream file is missing or unsafe"}
		}
	}
	return compileManaged(source, checkout, "deepseek-v4-flash-0731-dspark-tp2", false, c.validator)
}

// MiaGLM53EXL3Compiler maps the upstream two-Spark shell distribution to a
// rank-local LMW workload while retaining only the exact reviewed overlay
// files from the pinned commit.
type MiaGLM53EXL3Compiler struct {
	validator *recipe.Validator
}

func (c *MiaGLM53EXL3Compiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"Dockerfile", "start.sh", ".env.example"} {
		info, statErr := os.Lstat(filepath.Join(root, required))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: required, Message: "required upstream file is missing or unsafe"}
		}
	}
	return compileManagedAssets(
		source,
		checkout,
		"glm53-flash-exl3-dflash2-spark-tp2",
		func(asset string) bool {
			_, ok := glm53UpstreamAssets[filepath.ToSlash(asset)]
			return ok
		},
		false,
		c.validator,
	)
}

// TonyGLM53NVFP4Compiler turns the reviewed imperative TP2 distribution into
// an immutable LMW contract and packages only the two runtime assets required
// from upstream. The upstream launch script is required for layout drift
// detection but is never executed.
type TonyGLM53NVFP4Compiler struct {
	validator *recipe.Validator
}

func (c *TonyGLM53NVFP4Compiler) Compile(_ context.Context, source recipe.RepositorySource, checkout string, _ *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	for _, required := range []string{
		"README.md",
		"launch-glm53-vllm-tp2-dflash2.sh",
		"chat_template_mm.jinja",
		"docker/sparse_attn_indexer_kpool_sm121.py",
	} {
		info, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(required)))
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: required, Message: "required upstream file is missing or unsafe"}
		}
	}
	return compileManagedAssets(
		source,
		checkout,
		"glm53-flash-nvfp4-dflash2-spark-tp2",
		func(asset string) bool {
			_, ok := glm53NVFP4UpstreamAssets[filepath.ToSlash(asset)]
			return ok
		},
		false,
		c.validator,
	)
}

func compileManaged(source recipe.RepositorySource, checkout, template string, upstreamPatches bool, validator *recipe.Validator) (*recipe.PackResult, error) {
	var upstreamAsset func(string) bool
	if upstreamPatches {
		upstreamAsset = func(asset string) bool {
			return strings.HasPrefix(asset, "patch/sglang/") && asset != qwenManagedLicense
		}
	}
	return compileManagedAssets(source, checkout, template, upstreamAsset, upstreamPatches, validator)
}

func compileManagedAssets(source recipe.RepositorySource, checkout, template string, upstreamAsset func(string) bool, rejectUnexpectedPatches bool, validator *recipe.Validator) (*recipe.PackResult, error) {
	manifestBytes, err := recipeassets.Templates.ReadFile(template + "/recipe.yaml")
	if err != nil {
		return nil, err
	}
	document, err := recipe.YAMLOrJSON(manifestBytes)
	if err != nil {
		return nil, err
	}
	manifest, err := recipe.Parse(document)
	if err != nil {
		return nil, err
	}
	manifest.Metadata.Source = &recipe.Source{URL: source.URL, Path: source.Path, Revision: source.CommitSHA}
	if err := validatePinnedManifest(manifest); err != nil {
		return nil, err
	}

	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	assets := make(map[string][]byte, len(manifest.Assets))
	requiredUpstream := make(map[string]struct{})
	optionalUpstream := map[string]struct{}{qwenManagedLicense: {}}
	for _, asset := range manifest.Assets {
		var content []byte
		if upstreamAsset != nil && upstreamAsset(asset) {
			requiredUpstream[filepath.ToSlash(asset)] = struct{}{}
			content, err = readRegular(filepath.Join(root, filepath.FromSlash(asset)))
		} else {
			content, err = recipeassets.Templates.ReadFile(template + "/" + asset)
		}
		if err != nil {
			return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: asset, Message: err.Error()}
		}
		assets[asset] = content
	}
	if rejectUnexpectedPatches {
		if err := rejectUnexpectedPatchFiles(
			filepath.Join(root, "patch", "sglang"),
			requiredUpstream,
			optionalUpstream,
		); err != nil {
			return nil, err
		}
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if _, diagnostics, err := validator.ValidateStrict(canonical); err != nil {
		return nil, err
	} else {
		var errors []string
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == "error" {
				errors = append(errors, diagnostic.Message)
			}
		}
		if len(errors) > 0 {
			return nil, fmt.Errorf("recipe validation: %s", strings.Join(errors, "; "))
		}
	}
	return recipe.PackManifest(canonical, assets, map[string]string{
		"localmodelworks.repository.commit": source.CommitSHA,
		"localmodelworks.repository.tree":   source.TreeSHA,
		"localmodelworks.compiler":          template + "@1",
	})
}

func validatePinnedManifest(manifest *recipe.Manifest) error {
	for _, workload := range manifest.Workloads {
		if workload.Image.Digest == "" || !strings.HasPrefix(workload.Image.Digest, "sha256:") || !strings.Contains(workload.Image.Reference, "@"+workload.Image.Digest) {
			return &recipe.PackError{Code: "recipe.image_unpinned", Message: "managed workload image must be pinned by matching digest"}
		}
	}
	for _, artifact := range manifest.Artifacts {
		sources := make([]recipe.ArtSource, 0, 1+len(artifact.Variants))
		if artifact.Source != nil {
			sources = append(sources, *artifact.Source)
		}
		for _, variant := range artifact.Variants {
			sources = append(sources, variant.Source)
		}
		for _, source := range sources {
			if source.Type == "huggingface" && len(source.Revision) != 40 {
				return &recipe.PackError{Code: "recipe.artifact_unpinned", Asset: artifact.Name, Message: "Hugging Face artifact revision must be an exact commit"}
			}
			if source.Digest != "" && !strings.HasPrefix(source.Digest, "sha256:") {
				return &recipe.PackError{Code: "recipe.artifact_unpinned", Asset: artifact.Name, Message: "artifact digest must be sha256"}
			}
		}
	}
	return nil
}

func rejectUnexpectedPatchFiles(root string, required, optional map[string]struct{}) error {
	seenRequired := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unsafe patch entry %s", path)
		}
		rel, err := filepath.Rel(filepath.Dir(filepath.Dir(root)), path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if _, ok := required[rel]; ok {
			seenRequired++
			return nil
		}
		if _, ok := optional[rel]; ok {
			return nil
		}
		return &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: rel, Message: "unexpected upstream patch file"}
	})
	if err != nil {
		return err
	}
	if seenRequired != len(required) {
		return &recipe.PackError{Code: "recipe.repository_layout_changed", Message: "required upstream patch file is missing"}
	}
	return nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("not a regular file")
	}
	return os.ReadFile(path)
}

func checkoutPath(checkout, sourcePath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(sourcePath))
	if clean == "." {
		return checkout, nil
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("repository source path %q escapes checkout", sourcePath)
	}
	return filepath.Join(checkout, clean), nil
}
