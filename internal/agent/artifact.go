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

// hfBaseURL is the Hugging Face API/download origin. Overridden in tests.
var hfBaseURL = &url.URL{Scheme: "https", Host: "huggingface.co"}

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
	baseURL := &url.URL{Scheme: hfBaseURL.Scheme, Host: hfBaseURL.Host, Path: "/api/models/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/revision/" + revision}
	// ?blobs=true is required so Hugging Face populates `size` and `lfs` on
	// each sibling. Without it siblings carry only `rfilename`, which decode
	// to size 0 / nil LFS and make resumeHTTPFile read 1 byte against a
	// "want 0" expectation.
	baseURL.RawQuery = url.Values{"blobs": {"true"}}.Encode()
	apiURL := baseURL.String()
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
	// The model tree (blobs + snapshot symlinks) is bind-mounted into the
	// workload container, which runs as root with all capabilities dropped
	// (no CAP_DAC_OVERRIDE). Directories must be 0755 (world-traversable),
	// not the 0750/0700 defaults, or the container cannot reach the files.
	for _, dir := range []string{modelRoot, blobRoot, snapshotRoot, partialRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// Normalize a model tree fetched by an older agent build (0750 dirs /
	// 0640 blobs) so a cache hit is also container-readable.
	if err := makeModelTreeReadable(modelRoot); err != nil {
		return err
	}
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
		if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
			return err
		}
		downloadURL := fmt.Sprintf("%s://%s/%s/%s/resolve/%s/%s", hfBaseURL.Scheme, hfBaseURL.Host, url.PathEscape(owner), url.PathEscape(repo), revision, strings.ReplaceAll(url.PathEscape(filepath.ToSlash(rel)), "%2F", "/"))
		if err := resumeHTTPFile(ctx, client, downloadURL, token, partial, expectedSize); err != nil {
			return err
		}
		digest, size, err := digestFile(partial)
		if err != nil || size != expectedSize || (expectedDigest != "" && digest != expectedDigest) {
			return fmt.Errorf("HF file %s digest or size mismatch", sibling.Name)
		}
		blob := filepath.Join(blobRoot, strings.TrimPrefix(digest, "sha256:"))
		if err := os.MkdirAll(blobRoot, 0o755); err != nil {
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
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
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
	// The blob is bind-mounted into the workload container, which runs as
	// root with all capabilities dropped (no CAP_DAC_OVERRIDE). It must be
	// world-readable (0644), not the OS default 0640, or the container gets
	// "Permission denied" on config.json / weight files.
	file, err := os.OpenFile(destination, flags, 0o644)
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
	// A pre-existing .part/.resume target may already exist at 0640;
	// re-chmod so a cache hit also ends up world-readable.
	if err := os.Chmod(destination, 0o644); err != nil {
		return err
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
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	// MkdirAll only sets the mode when creating; a recipes directory already
	// present as 0750 from an older agent build must be re-chmodded here so
	// the upgraded agent fixes live installs, and so the bind-source chain
	// stays traversable by the container's non-agent UID.
	if err := os.Chmod(root, 0o755); err != nil {
		return err
	}
	final := filepath.Join(root, strings.TrimPrefix(manifestDigest, "sha256:"))
	if _, err := os.Stat(final); err == nil {
		// A package from an older agent build is cached here with the
		// 0700/0750 directory modes that block the container's non-agent UID;
		// normalize it so a retried deploy of the same digest works.
		if err := makePackageTraversable(final); err != nil {
			return err
		}
		a.sendPlacement(placementCandidate{Identity: identity, Path: final, State: "valid", Size: int64(len(layer))})
		return nil
	}
	staging, err := os.MkdirTemp(root, ".package-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	// Unpack assets into the staging tree, then normalize the whole package
	// directory chain to 0755 so the bind-mounted assets are traversable by
	// the container's non-agent UID.
	assets := filepath.Join(staging, "assets")
	if err := recipe.UnpackLayer(layer, assets); err != nil {
		return err
	}
	if err := makePackageTraversable(staging); err != nil {
		return err
	}
	if err := os.Rename(staging, final); err != nil {
		return err
	}
	a.sendPlacement(placementCandidate{Identity: identity, Path: final, State: "valid", Size: int64(len(layer))})
	return nil
}

// makePackageTraversable walks a recipe package directory and sets every
// directory to 0755 (world-traversable). The package and its assets subtree
// are bind-mounted into the workload container, which runs as a non-agent
// UID; the 0700/0750 modes the OS defaults leave on the staging/recipes
// directories block that UID from reaching /lmw/assets/serve.sh, which the
// container reports as "Permission denied". Files keep their packed modes
// (0555), so only directory modes are normalized here.
func makePackageTraversable(pkgDir string) error {
	return filepath.WalkDir(pkgDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return os.Chmod(p, 0o755)
	})
}

// makeModelTreeReadable normalizes a Hugging Face cache model tree so the
// workload container (root, no CAP_DAC_OVERRIDE) can read it: every directory
// to 0755 (world-traversable) and every regular file to 0644 (world-readable).
// Symlinks are left untouched — they are the HF snapshot -> blobs links. This
// fixes model trees fetched by an older agent build that created 0750
// directories and 0640 blobs, which a cache hit would otherwise serve up
// still unreadable.
func makeModelTreeReadable(modelRoot string) error {
	if _, err := os.Stat(modelRoot); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(modelRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.Chmod(p, 0o755)
		case info.Mode().IsRegular():
			return os.Chmod(p, 0o644)
		default:
			return nil // symlinks and other special files: leave as-is
		}
	})
}
