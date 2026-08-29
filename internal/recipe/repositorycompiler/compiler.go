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
	QwenRepositoryURL     = "https://github.com/MiaAI-Lab/Qwen3.8-27B-RTX-6000-PRO-SGLang-DSpark"
	DeepSeekRepositoryURL = "https://github.com/MiaAI-Lab/DeepSeek-v4-Flash-DSpark-2x-DGX-Spark"
)

const qwenManagedLicense = "patch/sglang/LICENSE"

type Compiler = recipe.RepositoryCompiler

// Registry selects an explicit driver before falling back to a native bundle.
type Registry struct {
	validator *recipe.Validator
	drivers   map[string]Compiler
}

func NewRegistry(validator *recipe.Validator) *Registry {
	qwenID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: QwenRepositoryURL, Path: "."})
	deepSeekID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: DeepSeekRepositoryURL, Path: "."})
	return &Registry{
		validator: validator,
		drivers: map[string]Compiler{
			qwenID:     &MiaQwenSGLangCompiler{validator: validator},
			deepSeekID: &MiaDeepSeekDSparkCompiler{validator: validator},
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

func compileManaged(source recipe.RepositorySource, checkout, template string, upstreamPatches bool, validator *recipe.Validator) (*recipe.PackResult, error) {
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
		if upstreamPatches && strings.HasPrefix(asset, "patch/sglang/") && asset != qwenManagedLicense {
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
	if upstreamPatches {
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
		for _, diagnostic := range diagnostics {
			if diagnostic.Severity == "error" {
				return nil, fmt.Errorf("recipe validation: %s", diagnostic.Message)
			}
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
