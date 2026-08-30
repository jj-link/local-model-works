package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	"gopkg.in/yaml.v3"
)

const (
	maxDraftFiles = 10_000
	maxDraftFile  = 16 << 20
	maxDraftTotal = 128 << 20
)

type draftCandidate struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type draftDiagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}

type draftReport struct {
	Remote      string            `json:"remote"`
	Revision    string            `json:"revision"`
	Tree        string            `json:"tree"`
	Path        string            `json:"path"`
	Candidates  []draftCandidate  `json:"candidates"`
	Diagnostics []draftDiagnostic `json:"diagnostics"`
}

func runRecipe(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lmw recipe validate|pack|init ...")
	}
	switch args[0] {
	case "validate":
		return runRecipeValidate(args[1:], os.Stdout)
	case "pack":
		return runRecipePack(args[1:], os.Stdout)
	case "init":
		return runRecipeInit(args[1:], os.Stdout)
	default:
		return fmt.Errorf("unknown recipe action %q", args[0])
	}
}

func runRecipeValidate(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("recipe validate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: lmw recipe validate <dir>")
	}
	validator, err := recipe.NewValidator()
	if err != nil {
		return err
	}
	manifest, packed, err := recipe.PackFromDir(fs.Arg(0), validator)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"valid": true, "name": manifest.Metadata.Name, "version": manifest.Metadata.Version,
		"manifest_digest": packed.ManifestDigest,
	})
}

func runRecipePack(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("recipe pack", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	output := fs.String("output", "", "output OCI layout directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || *output == "" {
		return fmt.Errorf("usage: lmw recipe pack <dir> --output <oci-layout>")
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("output already exists: %s", *output)
	} else if !os.IsNotExist(err) {
		return err
	}
	validator, err := recipe.NewValidator()
	if err != nil {
		return err
	}
	_, packed, err := recipe.PackFromDir(fs.Arg(0), validator)
	if err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Clean(*output))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".lmw-pack-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := recipe.WriteLayout(tmp, packed); err != nil {
		return err
	}
	if err := os.Rename(tmp, *output); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"manifest_digest": packed.ManifestDigest, "output": *output})
}

func runRecipeInit(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("recipe init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	remote := fs.String("from-git", "", "Git remote")
	revision := fs.String("revision", "", "branch, tag, or commit to resolve")
	subpath := fs.String("path", "", "subdirectory containing the recipe source")
	output := fs.String("output", "", "draft output directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *remote == "" || *revision == "" || *output == "" {
		return fmt.Errorf("usage: lmw recipe init --from-git <url> --revision <ref> [--path <path>] --output <dir>")
	}
	if _, err := os.Stat(*output); err == nil {
		return fmt.Errorf("output already exists: %s", *output)
	} else if !os.IsNotExist(err) {
		return err
	}
	report, err := inspectGitDraft(context.Background(), *remote, *revision, *subpath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*output, 0o750); err != nil {
		return err
	}
	manifest := map[string]any{
		"apiVersion": recipe.APIVersion,
		"kind":       "Recipe",
		"metadata": map[string]any{
			"name": "draft-recipe", "version": "0.1.0",
			"description": "Inspect and complete this declarative recipe before installation.",
			"license":     "NOASSERTION",
			"source":      map[string]any{"url": report.Remote, "revision": report.Revision, "path": report.Path},
		},
		"compatibility": map[string]any{"nodeCount": 1},
		"artifacts":     []any{},
		"workloads":     []any{},
	}
	yamlBytes, err := yaml.Marshal(manifest)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*output, "recipe.yaml"), yamlBytes, 0o640); err != nil {
		return err
	}
	reportJSON, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(*output, "recipe.draft.json"), append(reportJSON, '\n'), 0o640); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(report)
}

func inspectGitDraft(ctx context.Context, remote, revision, subpath string) (*draftReport, error) {
	tmp, err := os.MkdirTemp("", "lmw-recipe-init-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := runRecipeGit(ctx, tmp, "clone", "--no-checkout", remote, tmp); err != nil {
		return nil, err
	}
	if err := runRecipeGit(ctx, tmp, "checkout", "--quiet", revision); err != nil {
		return nil, err
	}
	commit, err := recipeGitOutput(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	tree, err := recipeGitOutput(ctx, tmp, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	rel := filepath.Clean(subpath)
	if rel == "." {
		rel = ""
	}
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("--path escapes the checkout")
	}
	root := filepath.Join(tmp, rel)
	var candidates []draftCandidate
	var total int64
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == filepath.Join(tmp, ".git") || strings.HasPrefix(path, filepath.Join(tmp, ".git")+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > maxDraftFile {
			return nil
		}
		total += info.Size()
		if total > maxDraftTotal || len(candidates) >= maxDraftFiles {
			return fmt.Errorf("repository candidate inventory exceeds limits")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		candidatePath, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		candidates = append(candidates, draftCandidate{Path: filepath.ToSlash(candidatePath), Size: info.Size(), SHA256: hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Path < candidates[j].Path })
	return &draftReport{
		Remote: remote, Revision: commit, Tree: tree, Path: filepath.ToSlash(rel), Candidates: candidates,
		Diagnostics: []draftDiagnostic{
			{Code: "recipe.draft_incomplete", Path: "/metadata", Message: "confirm name, version, display name, description, and SPDX license"},
			{Code: "recipe.draft_incomplete", Path: "/compatibility", Message: "declare hardware and fabric requirements"},
			{Code: "recipe.draft_incomplete", Path: "/artifacts", Message: "declare immutable artifact identities"},
			{Code: "recipe.draft_incomplete", Path: "/workloads", Message: "author at least one digest-pinned declarative workload"},
		},
	}, nil
}

func runRecipeGit(ctx context.Context, dir string, args ...string) error {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func recipeGitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}
