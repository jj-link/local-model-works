package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/hf"
	"github.com/jj-link/local-model-works/internal/recipe"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type hfModelInfo struct {
	Siblings []struct {
		Name string `json:"rfilename"`
		Size int64  `json:"size"`
		LFS  *struct {
			SHA256 string `json:"sha256"`
			Size   int64  `json:"size"`
		} `json:"lfs"`
	} `json:"siblings"`
}

func (a *Agent) handleArtifact(ctx context.Context, command *agentv1.ArtifactCommand) {
	if command.GetOp() != agentv1.ArtifactOp_ARTIFACT_OP_FETCH && command.GetOp() != agentv1.ArtifactOp_ARTIFACT_OP_VALIDATE {
		a.result(command.GetCommandId(), false, 0, "artifact.unsupported_operation", "", "")
		return
	}
	if strings.HasPrefix(command.GetArtifactIdentity(), "recipe://") {
		if err := a.fetchRecipePackage(ctx, command.GetArtifactIdentity()); err != nil {
			a.result(command.GetCommandId(), false, 0, err.Error(), "", "")
			return
		}
		a.result(command.GetCommandId(), true, 0, "", "", "")
		return
	}
	parsed, err := artifactidentity.Parse(command.GetArtifactIdentity())
	if err != nil {
		a.result(command.GetCommandId(), false, 0, err.Error(), "", "")
		return
	}
	if parsed.Kind != "model" {
		a.result(command.GetCommandId(), false, 0, "artifact.origin_unsupported", "", "")
		return
	}
	cacheRoot := command.GetCacheRoot()
	if cacheRoot == "" && len(a.cfg.CacheRoots) > 0 {
		cacheRoot = a.cfg.CacheRoots[0]
	}
	if cacheRoot == "" || !filepath.IsAbs(cacheRoot) {
		a.result(command.GetCommandId(), false, 0, "artifact.cache_root_unavailable", "", "")
		return
	}
	if command.GetOp() == agentv1.ArtifactOp_ARTIFACT_OP_FETCH {
		err = fetchHFSnapshot(ctx, command.GetArtifactIdentity(), cacheRoot, command.GetBearerToken())
	}
	candidate, validateErr := validateHFIdentity(ctx, command.GetArtifactIdentity(), cacheRoot)
	if err == nil {
		err = validateErr
	}
	if candidate.Identity != "" {
		a.sendPlacement(candidate)
	}
	if err != nil {
		a.result(command.GetCommandId(), false, 0, err.Error(), "", "")
		return
	}
	// bearer_token is intentionally not retained beyond this call.
	a.result(command.GetCommandId(), true, 0, "", "", "")
}

func validateHFIdentity(ctx context.Context, identity, cacheRoot string) (placementCandidate, error) {
	base, revision, ok := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
	if !ok {
		return placementCandidate{}, fmt.Errorf("invalid HF identity")
	}
	owner, repo, ok := strings.Cut(base, "/")
	if !ok {
		return placementCandidate{}, fmt.Errorf("invalid HF repository")
	}
	modelRoot := filepath.Join(cacheRoot, "hub", "models--"+owner+"--"+repo)
	if _, err := os.Stat(modelRoot); os.IsNotExist(err) {
		modelRoot = filepath.Join(cacheRoot, "models--"+owner+"--"+repo)
	}
	snapshot := filepath.Join(modelRoot, "snapshots", revision)
	diagnostics := hf.ValidateSnapshot(snapshot, modelRoot)
	candidate := placementCandidate{Identity: identity, Path: modelRoot, State: "valid", Size: regularTreeSize(ctx, modelRoot)}
	for _, diagnostic := range diagnostics {
		candidate.Diagnostics = append(candidate.Diagnostics, &agentv1.Diagnostic{
			Code: diagnostic.Code, Severity: diagnostic.Severity, Message: diagnostic.Message, Resource: diagnostic.Path,
		})
	}
	if len(candidate.Diagnostics) > 0 {
		candidate.State = "invalid"
		return candidate, fmt.Errorf("downloaded snapshot failed validation")
	}
	return candidate, nil
}

func fetchHFSnapshot(ctx context.Context, identity, cacheRoot, token string) error {
	base, revision, ok := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
	if !ok {
		return fmt.Errorf("invalid HF identity")
	}
	owner, repo, ok := strings.Cut(base, "/")
	if !ok {
		return fmt.Errorf("invalid HF repository")
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	apiURL := "https://huggingface.co/api/models/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/revision/" + revision
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	request.Header.Set("Accept", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("HF metadata HTTP %d", response.StatusCode)
	}
	var info hfModelInfo
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&info); err != nil {
		return err
	}
	modelRoot := filepath.Join(cacheRoot, "hub", "models--"+owner+"--"+repo)
	blobRoot := filepath.Join(modelRoot, "blobs")
	snapshotRoot := filepath.Join(modelRoot, "snapshots", revision)
	partialRoot := filepath.Join(modelRoot, ".downloads", revision)
	for _, sibling := range info.Siblings {
		rel, err := safeRelativePath(sibling.Name)
		if err != nil {
			return err
		}
		expectedSize, expectedDigest := sibling.Size, ""
		if sibling.LFS != nil {
			expectedSize, expectedDigest = sibling.LFS.Size, "sha256:"+sibling.LFS.SHA256
		}
		if expectedSize < 0 || expectedSize > 1<<40 {
			return fmt.Errorf("HF file %s has invalid size", sibling.Name)
		}
		partial := filepath.Join(partialRoot, rel+".part")
		if err := os.MkdirAll(filepath.Dir(partial), 0o750); err != nil {
			return err
		}
		downloadURL := "https://huggingface.co/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/resolve/" + revision + "/" + strings.ReplaceAll(url.PathEscape(filepath.ToSlash(rel)), "%2F", "/")
		if err := resumeHTTPFile(ctx, client, downloadURL, token, partial, expectedSize); err != nil {
			return err
		}
		digest, size, err := digestFile(partial)
		if err != nil || size != expectedSize || (expectedDigest != "" && digest != expectedDigest) {
			return fmt.Errorf("HF file %s digest or size mismatch", sibling.Name)
		}
		blob := filepath.Join(blobRoot, strings.TrimPrefix(digest, "sha256:"))
		if err := os.MkdirAll(blobRoot, 0o750); err != nil {
			return err
		}
		if _, err := os.Stat(blob); os.IsNotExist(err) {
			if err := os.Rename(partial, blob); err != nil {
				return err
			}
		} else {
			_ = os.Remove(partial)
		}
		link := filepath.Join(snapshotRoot, rel)
		if err := os.MkdirAll(filepath.Dir(link), 0o750); err != nil {
			return err
		}
		target, _ := filepath.Rel(filepath.Dir(link), blob)
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}

func resumeHTTPFile(ctx context.Context, client *http.Client, sourceURL, token, destination string, expectedSize int64) error {
	offset := int64(0)
	if info, err := os.Stat(destination); err == nil {
		offset = info.Size()
		if offset > expectedSize {
			if err := os.Remove(destination); err != nil {
				return err
			}
			offset = 0
		}
	}
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if offset > 0 {
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 && response.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else if response.StatusCode == http.StatusOK {
		flags |= os.O_TRUNC
		offset = 0
	} else {
		return fmt.Errorf("download HTTP %d", response.StatusCode)
	}
	file, err := os.OpenFile(destination, flags, 0o640)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(response.Body, expectedSize-offset+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if offset+written != expectedSize {
		return fmt.Errorf("download size %d, want %d", offset+written, expectedSize)
	}
	return nil
}

func digestFile(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (a *Agent) fetchRecipePackage(ctx context.Context, identity string) error {
	manifestDigest := strings.TrimPrefix(identity, "recipe://")
	if !strings.HasPrefix(manifestDigest, "sha256:") || len(manifestDigest) != 71 {
		return fmt.Errorf("invalid recipe package identity")
	}
	transport, err := a.clientTransport()
	if err != nil {
		return err
	}
	client := httpClient(transport)
	endpoint := strings.TrimSuffix(a.cfg.ServerURL, "/") + "/packages/" + url.PathEscape(manifestDigest) + "/layer"
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("package layer HTTP %d", response.StatusCode)
	}
	layer, err := io.ReadAll(io.LimitReader(response.Body, recipe.MaxCompressedLayerBytes+1))
	if err != nil || len(layer) > recipe.MaxCompressedLayerBytes {
		return fmt.Errorf("package layer exceeds limit")
	}
	sum := sha256.Sum256(layer)
	layerDigest := "sha256:" + hex.EncodeToString(sum[:])
	if response.Header.Get("X-LMW-Layer-Digest") != layerDigest {
		return fmt.Errorf("package layer digest mismatch")
	}
	root := filepath.Join(a.cfg.StateRoot, "recipes")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return err
	}
	final := filepath.Join(root, strings.TrimPrefix(manifestDigest, "sha256:"))
	if _, err := os.Stat(final); err == nil {
		a.sendPlacement(placementCandidate{Identity: identity, Path: final, State: "valid", Size: int64(len(layer))})
		return nil
	}
	staging, err := os.MkdirTemp(root, ".package-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	assets := filepath.Join(staging, "assets")
	if err := os.MkdirAll(assets, 0o750); err != nil {
		return err
	}
	if err := recipe.UnpackLayer(layer, assets); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	a.sendPlacement(placementCandidate{Identity: identity, Path: final, State: "valid", Size: int64(len(layer))})
	return nil
}
