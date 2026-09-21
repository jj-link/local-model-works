// Package repositorycompiler contains deterministic, non-executing compilers
// for native recipe bundles and explicitly supported third-party repositories.
package repositorycompiler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	recipeassets "github.com/jj-link/local-model-works/recipes"
)

const (
	QwenRepositoryURL          = "https://github.com/MiaAI-Lab/Qwen3.8-27B-RTX-6000-PRO-SGLang-DSpark"
	DeepSeekRepositoryURL      = "https://github.com/MiaAI-Lab/DeepSeek-v4-Flash-DSpark-2x-DGX-Spark"
	GLM53RepositoryURL         = "https://github.com/MiaAI-Lab/GLM-5.3-Flash-EXL3-2x-DGX-Sparks"
	GLM53NVFP4RepositoryURL    = "https://github.com/tonyd2wild/GLM-5.3-Flash-NVFP4-DFlash2-2x-DGX-Spark"
	QwenDGXSparkRepositoryURL  = "https://github.com/MiaAI-Lab/Qwen3.8-27B-SGLang-DGX-Spark"
	QwenFlashNextRepositoryURL = "https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Dual-DGX-Sparks"
)

const QwenFlashNextSingleRepositoryURL = "https://github.com/MiaAI-Lab/Qwen3.8-Flash-Next-Single-DGX-Spark"

type Compiler = recipe.RepositoryCompiler

// Registry prefers upstream-authored native bundles, then reviewed source procedures.
type Registry struct {
	validator *recipe.Validator
	drivers   map[string]Compiler
}

func NewRegistry(validator *recipe.Validator) *Registry {
	registry := &Registry{validator: validator, drivers: map[string]Compiler{}}
	for _, entry := range []struct{ url, template string }{
		{QwenRepositoryURL, "qwen38-27b-rtx6000pro-dflash2"},
		{QwenDGXSparkRepositoryURL, "qwen38-27b-dgx-spark-mtp"},
		{QwenFlashNextSingleRepositoryURL, "qwen38-flash-next-spark-tp1"},
		{DeepSeekRepositoryURL, "deepseek-v4-flash-vision-exp-dspark-tp2"},
		{QwenFlashNextRepositoryURL, "qwen38-flash-next-dspark-tp2"},
		{GLM53RepositoryURL, "glm53-flash-exl3-dflash2-spark-tp2"},
	} {
		repositoryID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: entry.url, Path: "."})
		registry.drivers[repositoryID] = &UpstreamCompiler{validator: validator, template: entry.template}
	}
	nvfp4ID, _, _, _ := recipe.RepositoryIdentity(recipe.Source{URL: GLM53NVFP4RepositoryURL, Path: "."})
	registry.drivers[nvfp4ID] = &UpstreamCompiler{blocked: "The reviewed NVFP4 source at 050081dc41ce6edd4d3f15fa19dc3410ba4210e3 has no authored TP2 stop procedure and hardcodes its author's fabric addresses and port 8000. Review an upstream revision with a complete applicable lifecycle; LMW will not substitute a generated launcher or edit those constants."}
	return registry
}

func (r *Registry) Lookup(source recipe.RepositorySource, checkout string) (Compiler, bool) {
	sourceRoot, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, false
	}
	for _, name := range []string{"recipe.yaml", "recipe.json"} {
		if info, statErr := os.Stat(filepath.Join(sourceRoot, name)); statErr == nil && info.Mode().IsRegular() {
			return &NativeBundleCompiler{validator: r.validator}, true
		}
	}
	compiler, ok := r.drivers[source.RepositoryID]
	return compiler, ok
}

func (r *Registry) SupportsRepository(repositoryID string) bool {
	_, ok := r.drivers[repositoryID]
	return ok
}

// LookupUpstream resolves only a shipped reviewed procedure, without probing a checkout.
func (r *Registry) LookupUpstream(source recipe.RepositorySource) (*UpstreamCompiler, bool) {
	compiler, ok := r.drivers[source.RepositoryID].(*UpstreamCompiler)
	return compiler, ok
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

// UpstreamCompiler verifies the maintained authored lifecycle against target
// source and binds configuration to those exact bytes without executing it.
type UpstreamCompiler struct {
	validator *recipe.Validator
	template  string
	blocked   string
}

func (c *UpstreamCompiler) Compile(ctx context.Context, source recipe.RepositorySource, checkout string, previous *recipe.RecipeDetail) (*recipe.PackResult, error) {
	root, err := checkoutPath(checkout, source.Path)
	if err != nil {
		return nil, err
	}
	return c.CompileRetained(ctx, source, func(name string) ([]byte, error) {
		return readRegular(filepath.Join(root, filepath.FromSlash(name)))
	}, previous)
}

// CompileRetained verifies a maintained procedure against retained source files.
// readSource must return verified original bytes at source-directory-relative paths.
func (c *UpstreamCompiler) CompileRetained(_ context.Context, source recipe.RepositorySource, readSource func(string) ([]byte, error), previous *recipe.RecipeDetail) (*recipe.PackResult, error) {
	if c.blocked != "" {
		return nil, &recipe.PackError{Code: "recipe.upstream_review_required", Message: c.blocked}
	}
	manifestBytes, err := recipeassets.Templates.ReadFile(c.template + "/recipe.yaml")
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
	if manifest.Metadata.Source == nil {
		return nil, fmt.Errorf("maintained upstream procedure has no source identity")
	}
	expectedRepository, _, _, err := recipe.RepositoryIdentity(*manifest.Metadata.Source)
	if err != nil {
		return nil, err
	}
	actualRepository, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: source.URL, Path: source.Path})
	if err != nil {
		return nil, err
	}
	if actualRepository != expectedRepository {
		return nil, layoutError(source.Path, fmt.Errorf("maintained lifecycle belongs to a different repository or source directory"))
	}
	if readSource == nil {
		return nil, fmt.Errorf("verified source reader is required")
	}
	for _, workload := range manifest.Workloads {
		if workload.Upstream == nil {
			return nil, fmt.Errorf("reviewed upstream procedure has no authored lifecycle")
		}
		required := []string{workload.Upstream.Start[0], workload.Upstream.Stop[0]}
		for _, command := range workload.Upstream.Install {
			required = append(required, command[0])
		}
		for _, commands := range workload.Upstream.InstallByRank {
			for _, command := range commands {
				required = append(required, command[0])
			}
		}
		if workload.Upstream.EnvTemplate != "" {
			required = append(required, "./"+workload.Upstream.EnvTemplate)
		}
		for _, name := range required {
			if !strings.HasPrefix(name, "./") {
				continue
			}
			if _, err := readSource(strings.TrimPrefix(name, "./")); err != nil {
				return nil, &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: name, Message: err.Error()}
			}
		}
	}
	if err := verifyLifecycle(manifest, readSource); err != nil {
		return nil, err
	}
	if err := compileConfiguration(manifest, readSource); err != nil {
		return nil, err
	}
	manifest.Metadata.Version, err = nextManagedVersion(manifest.Metadata.Version, previous)
	if err != nil {
		return nil, err
	}
	manifest.Metadata.Source.URL = source.URL
	manifest.Metadata.Source.Path = source.Path
	manifest.Metadata.Source.Revision = source.CommitSHA
	if manifest.Metadata.Source.Path == "" {
		manifest.Metadata.Source.Path = "."
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	_, diagnostics, err := c.validator.ValidateStrict(canonical)
	if err != nil {
		return nil, err
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" {
			return nil, fmt.Errorf("recipe validation: %s", diagnostic.Message)
		}
	}
	return recipe.PackManifest(canonical, nil, map[string]string{
		"localmodelworks.repository.commit": source.CommitSHA,
		"localmodelworks.repository.tree":   source.TreeSHA,
		"localmodelworks.compiler":          "upstream-procedure@5",
	})
}

func nextManagedVersion(templateVersion string, previous *recipe.RecipeDetail) (string, error) {
	template, err := parseManagedVersion(templateVersion)
	if err != nil {
		return "", fmt.Errorf("parse managed recipe template version %q: %w", templateVersion, err)
	}
	// Compiled source adaptations have their own release floor: unchanged
	// upstream templates must not keep imports on a superseded launch contract.
	minimum := [3]int{2, 2, 0}
	if compareManagedVersions(template, minimum) < 0 {
		template, templateVersion = minimum, "2.2.0"
	}
	if previous == nil {
		return templateVersion, nil
	}
	current, err := parseManagedVersion(previous.Version)
	if err != nil {
		return "", fmt.Errorf("parse previous managed recipe version %q: %w", previous.Version, err)
	}
	current[2]++
	if compareManagedVersions(template, current) > 0 {
		current = template
	}
	return fmt.Sprintf("%d.%d.%d", current[0], current[1], current[2]), nil
}

func parseManagedVersion(version string) ([3]int, error) {
	var parsed [3]int
	core := strings.SplitN(strings.SplitN(version, "+", 2)[0], "-", 2)[0]
	parts := strings.Split(core, ".")
	if len(parts) != len(parsed) {
		return parsed, fmt.Errorf("expected major.minor.patch")
	}
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			return parsed, fmt.Errorf("invalid numeric component %q", part)
		}
		parsed[index] = value
	}
	return parsed, nil
}

func compareManagedVersions(a, b [3]int) int {
	for index := range a {
		if a[index] < b[index] {
			return -1
		}
		if a[index] > b[index] {
			return 1
		}
	}
	return 0
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
