package recipe

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

// PrepareRepositoryUpdate checks upstream and saves an immutable package without
// contacting devices. The selected saved manifest, not a shipped default, is the
// customization baseline. Saving uses the existing compare-and-select transaction.
func (s *Service) PrepareRepositoryUpdate(ctx context.Context, repositoryID string) (Recipe, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	status, err := s.checkRepositoryUpdates(ctx, repositoryID, make(map[string]gitHead))
	if err != nil {
		return Recipe{}, err
	}
	if status.State == "error" {
		return Recipe{}, &PackError{Code: "recipe.update_check_failed", Message: status.Error}
	}
	if !sha40.MatchString(status.CandidateRevision) {
		return Recipe{}, &PackError{Code: "recipe.update_check_failed", Message: "upstream did not provide an immutable target revision"}
	}
	repository, err := s.q.GetRecipeRepository(ctx, repositoryID)
	if err != nil {
		return Recipe{}, err
	}
	previous, err := s.Get(ctx, repository.CurrentDigest.String)
	if err != nil {
		return Recipe{}, err
	}
	currentVersion, err := s.q.GetRecipeRepositoryVersionByDigest(ctx, previous.Digest)
	if err != nil {
		return Recipe{}, err
	}
	if !strings.EqualFold(currentVersion.CommitSha, status.InstalledRevision) {
		return Recipe{}, &PackError{Code: "recipe.update_stale", Message: "saved source changed during the upstream check"}
	}
	if strings.EqualFold(status.InstalledRevision, status.CandidateRevision) {
		return previous.Recipe, nil
	}
	if s.repositoryCompilers == nil {
		return Recipe{}, &PackError{Code: RepositoryUnsupportedCode, Message: "no deterministic repository compiler is configured"}
	}
	if err := os.MkdirAll(s.packageRoot, 0o755); err != nil {
		return Recipe{}, err
	}
	checkout, err := os.MkdirTemp(s.packageRoot, ".update-source-")
	if err != nil {
		return Recipe{}, err
	}
	defer os.RemoveAll(checkout)
	if err := runGit(ctx, checkout, "clone", "--no-checkout", "--", repository.SourceUrl, checkout); err != nil {
		return Recipe{}, fmt.Errorf("update source checkout: %w", err)
	}
	compile := func(commit string, baseline *RecipeDetail) (*PackResult, string, error) {
		if err := runGit(ctx, checkout, "checkout", "--detach", "--force", commit); err != nil {
			return nil, "", err
		}
		actual, err := runGitOutput(ctx, checkout, "rev-parse", "HEAD")
		if err != nil || actual != commit {
			return nil, "", fmt.Errorf("update source revision mismatch: got %s, expected %s: %v", actual, commit, err)
		}
		tree, err := runGitOutput(ctx, checkout, "rev-parse", "HEAD^{tree}")
		if err != nil {
			return nil, "", err
		}
		source := RepositorySource{RepositoryID: repositoryID, URL: repository.SourceUrl, Path: repository.SourcePath, CommitSHA: commit, TreeSHA: tree}
		compiler, ok := s.repositoryCompilers.Lookup(source, checkout)
		if !ok {
			return nil, "", &PackError{Code: RepositoryUnsupportedCode, Message: "the source has no native recipe bundle or supported deterministic compiler"}
		}
		packed, err := compiler.Compile(ctx, source, checkout, baseline)
		return packed, tree, err
	}
	base, _, err := compile(status.InstalledRevision, nil)
	if err != nil {
		return Recipe{}, fmt.Errorf("saved source baseline: %w", err)
	}
	target, tree, err := compile(status.CandidateRevision, &previous)
	if err != nil {
		return Recipe{}, err
	}
	saved, err := ReadLayout(filepath.Join(s.packageRoot, strings.TrimPrefix(previous.Digest, "sha256:")))
	if err != nil {
		return Recipe{}, err
	}
	var savedDocument, recordDocument any
	if err := json.Unmarshal(saved.ConfigJSON, &savedDocument); err != nil {
		return Recipe{}, err
	}
	if err := json.Unmarshal(previous.Manifest, &recordDocument); err != nil {
		return Recipe{}, err
	}
	if !reflect.DeepEqual(savedDocument, recordDocument) {
		return Recipe{}, &PackError{Code: "recipe.update_stale", Message: "saved recipe manifest differs from its immutable package"}
	}
	packed, err := preserveSavedPackage(ctx, checkout, base, saved, target)
	if err != nil {
		return Recipe{}, err
	}
	return s.storeReviewedPack(ctx, packed, RecipeSource{Type: "git", Remote: repository.SourceUrl,
		Path: repository.SourcePath, Revision: status.CandidateRevision, Tree: tree, TrackingRef: repository.TrackingRef},
		&reviewedSelection{repositoryID: repositoryID, expectedDigest: previous.Digest})
}

// preserveSavedValue reapplies only saved differences from the original source.
// Explicit local values win conflicts; unchanged fields follow the new source.
func preserveSavedValue(ctx context.Context, staging string, base, saved, target any) (any, error) {
	if reflect.DeepEqual(base, saved) {
		return target, nil
	}
	baseMap, baseOK := base.(map[string]any)
	savedMap, savedOK := saved.(map[string]any)
	targetMap, targetOK := target.(map[string]any)
	if baseOK && savedOK && targetOK {
		for key, baseValue := range baseMap {
			savedValue, exists := savedMap[key]
			if !exists {
				delete(targetMap, key)
				continue
			}
			if !reflect.DeepEqual(baseValue, savedValue) {
				value, err := preserveSavedValue(ctx, staging, baseValue, savedValue, targetMap[key])
				if err != nil {
					return nil, err
				}
				targetMap[key] = value
			}
		}
		for key, savedValue := range savedMap {
			if _, exists := baseMap[key]; !exists {
				targetMap[key] = savedValue
			}
		}
		return targetMap, nil
	}
	baseArray, baseOK := base.([]any)
	savedArray, savedOK := saved.([]any)
	targetArray, targetOK := target.([]any)
	if baseOK && savedOK && targetOK {
		// Named recipe entries retain their identity when upstream inserts or
		// reorders parameters, artifacts, variants, or source files.
		for _, key := range []string{"name", "path"} {
			indexes := [3]map[string]any{}
			valid := true
			for i, values := range [][]any{baseArray, savedArray, targetArray} {
				indexes[i] = make(map[string]any, len(values))
				for _, value := range values {
					entry, ok := value.(map[string]any)
					name, named := entry[key].(string)
					if !ok || !named || name == "" {
						valid = false
						break
					}
					if _, duplicate := indexes[i][name]; duplicate {
						valid = false
						break
					}
					indexes[i][name] = value
				}
			}
			if !valid {
				continue
			}
			merged, err := preserveSavedValue(ctx, staging, indexes[0], indexes[1], indexes[2])
			if err != nil {
				return nil, err
			}
			remaining := merged.(map[string]any)
			result := make([]any, 0, len(remaining))
			for _, values := range [][]any{targetArray, savedArray} {
				for _, value := range values {
					name := value.(map[string]any)[key].(string)
					if entry, exists := remaining[name]; exists {
						result = append(result, entry)
						delete(remaining, name)
					}
				}
			}
			return result, nil
		}
		if len(baseArray) == len(savedArray) && len(baseArray) == len(targetArray) {
			for index := range baseArray {
				value, err := preserveSavedValue(ctx, staging, baseArray[index], savedArray[index], targetArray[index])
				if err != nil {
					return nil, err
				}
				targetArray[index] = value
			}
			return targetArray, nil
		}
		var encoded [3][]byte
		for i, values := range [][]any{baseArray, savedArray, targetArray} {
			for _, value := range values {
				line, err := json.Marshal(value)
				if err != nil {
					return nil, err
				}
				encoded[i] = append(encoded[i], line...)
				encoded[i] = append(encoded[i], '\n')
			}
		}
		merged, err := sourceconfig.MergeLocalChanges(ctx, staging, encoded[0], encoded[1], encoded[2])
		if err != nil {
			return nil, fmt.Errorf("preserve saved recipe entries: %w", err)
		}
		result := make([]any, 0)
		decoder := json.NewDecoder(bytes.NewReader(merged))
		for {
			var value any
			if err := decoder.Decode(&value); err == io.EOF {
				return result, nil
			} else if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
	}
	return saved, nil
}

func preserveSavedPackage(ctx context.Context, staging string, base, saved, target *PackResult) (*PackResult, error) {
	var baseDoc, savedDoc, targetDoc map[string]any
	for _, input := range []struct {
		raw []byte
		out *map[string]any
	}{
		{base.ConfigJSON, &baseDoc}, {saved.ConfigJSON, &savedDoc}, {target.ConfigJSON, &targetDoc},
	} {
		if err := json.Unmarshal(input.raw, input.out); err != nil {
			return nil, err
		}
	}
	// Provenance and version belong to the newly compiled immutable source.
	targetMetadata, ok := targetDoc["metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("compiled recipe has no metadata")
	}
	source, err := json.Marshal(targetMetadata["source"])
	if err != nil {
		return nil, err
	}
	version := targetMetadata["version"]
	value, err := preserveSavedValue(ctx, staging, baseDoc, savedDoc, targetDoc)
	if err != nil {
		return nil, err
	}
	merged := value.(map[string]any)
	metadata, ok := merged["metadata"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("saved recipe has no metadata")
	}
	var immutableSource any
	if err := json.Unmarshal(source, &immutableSource); err != nil {
		return nil, err
	}
	metadata["source"], metadata["version"] = immutableSource, version
	document, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	baseAssets, err := updatePackageAssets(base)
	if err != nil {
		return nil, err
	}
	savedAssets, err := updatePackageAssets(saved)
	if err != nil {
		return nil, err
	}
	targetAssets, err := updatePackageAssets(target)
	if err != nil {
		return nil, err
	}
	for name, content := range baseAssets {
		local, exists := savedAssets[name]
		if !exists {
			delete(targetAssets, name)
		} else if !bytes.Equal(content, local) {
			merged, err := sourceconfig.MergeLocalChanges(ctx, staging, content, local, targetAssets[name])
			if err != nil {
				return nil, fmt.Errorf("preserve saved asset %s: %w", name, err)
			}
			targetAssets[name] = merged
		}
	}
	for name, content := range savedAssets {
		if _, exists := baseAssets[name]; !exists {
			targetAssets[name] = content
		}
	}
	manifest, err := Parse(document)
	if err != nil {
		return nil, err
	}
	assets := make(map[string][]byte, len(manifest.Assets))
	for _, name := range manifest.Assets {
		content, exists := targetAssets[name]
		if !exists {
			return nil, fmt.Errorf("saved recipe asset %s is unavailable in updated package", name)
		}
		assets[name] = content
	}
	return PackManifest(document, assets, nil)
}

func updatePackageAssets(packed *PackResult) (map[string][]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(packed.layerBytes))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	archive := tar.NewReader(reader)
	assets := map[string][]byte{}
	for {
		header, err := archive.Next()
		if err == io.EOF {
			return assets, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		content, err := io.ReadAll(io.LimitReader(archive, MaxAssetFileBytes+1))
		if err != nil {
			return nil, err
		}
		if len(content) > MaxAssetFileBytes {
			return nil, fmt.Errorf("recipe asset exceeds package size limit")
		}
		assets[header.Name] = content
	}
}
