package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

const (
	hfDownloadConcurrency = 16
	hfDownloadAttempts    = 5
)

type hfDownloadJob struct {
	index          int
	name           string
	rel            string
	expectedSize   int64
	expectedDigest string
	link           string
	partial        string
	downloadURL    string
}

type artifactDownloadProgress struct {
	Phase       string
	CurrentFile string
	BytesDone   uint64
	BytesTotal  uint64
	FilesDone   uint32
	FilesTotal  uint32
}

type hfSnapshotFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

type hfSnapshotManifest struct {
	Identity string           `json:"identity"`
	Files    []hfSnapshotFile `json:"files"`
}

func hfSnapshotManifestPath(modelRoot, revision string) string {
	return filepath.Join(modelRoot, ".lmw", "snapshots", revision+".json")
}

func writeHFCompletionManifest(modelRoot, revision, identity string, files []hfSnapshotFile) error {
	path := hfSnapshotManifestPath(modelRoot, revision)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(hfSnapshotManifest{Identity: identity, Files: files})
	if err != nil {
		return err
	}
	partial := path + ".part"
	if err := os.WriteFile(partial, data, 0o644); err != nil {
		return err
	}
	return os.Rename(partial, path)
}

func hfSnapshotDiagnostics(ctx context.Context, identity, modelRoot, snapshot string) []*agentv1.Diagnostic {
	var out []*agentv1.Diagnostic
	for _, diagnostic := range hf.ValidateSnapshot(snapshot, modelRoot) {
		out = append(out, &agentv1.Diagnostic{
			Code: diagnostic.Code, Severity: diagnostic.Severity,
			Message: diagnostic.Message, Resource: diagnostic.Path,
		})
	}
	_, revision, ok := strings.Cut(identity, "@")
	if !ok {
		return append(out, &agentv1.Diagnostic{
			Code: "artifact.snapshot_manifest_invalid", Severity: "error",
			Message: "snapshot identity has no immutable revision", Resource: snapshot,
		})
	}
	path := hfSnapshotManifestPath(modelRoot, revision)
	file, err := os.Open(path)
	if err != nil {
		return append(out, &agentv1.Diagnostic{
			Code: "artifact.snapshot_manifest_missing", Severity: "error",
			Message: "snapshot has not completed a managed fetch", Resource: path,
		})
	}
	defer file.Close()
	var manifest hfSnapshotManifest
	if err := json.NewDecoder(io.LimitReader(file, 16<<20)).Decode(&manifest); err != nil ||
		manifest.Identity != identity || len(manifest.Files) == 0 {
		return append(out, &agentv1.Diagnostic{
			Code: "artifact.snapshot_manifest_invalid", Severity: "error",
			Message: "snapshot completion manifest is invalid", Resource: path,
		})
	}
	for _, expected := range manifest.Files {
		if ctx.Err() != nil {
			break
		}
		rel, err := safeRelativePath(expected.Path)
		_, digestErr := artifactidentity.Parse("file://" + expected.Digest)
		if err != nil || digestErr != nil || expected.Size < 0 ||
			!existingSnapshotFile(filepath.Join(snapshot, rel), expected.Size, expected.Digest) {
			out = append(out, &agentv1.Diagnostic{
				Code: "artifact.snapshot_file_invalid", Severity: "error",
				Message:  "snapshot file is missing or does not match the completed fetch",
				Resource: expected.Path,
			})
		}
	}
	return out
}

type artifactProgressReporter func(artifactDownloadProgress)

func (a *Agent) handleArtifact(ctx context.Context, command *agentv1.ArtifactCommand) {
	if command.GetOp() == agentv1.ArtifactOp_ARTIFACT_OP_CANCEL {
		target := command.GetTargetCommandId()
		a.artifactMu.Lock()
		cancel := a.artifactCancels[target]
		a.artifactMu.Unlock()
		if target == "" || cancel == nil {
			a.result(command.GetCommandId(), false, 0, "artifact.cancel_target_unknown", "", "")
			return
		}
		cancel()
		a.result(command.GetCommandId(), true, 0, "", "", "")
		return
	}
	if command.GetOp() != agentv1.ArtifactOp_ARTIFACT_OP_FETCH && command.GetOp() != agentv1.ArtifactOp_ARTIFACT_OP_VALIDATE {
		a.result(command.GetCommandId(), false, 0, "artifact.unsupported_operation", "", "")
		return
	}
	commandCtx, cancel := context.WithCancel(ctx)
	a.artifactMu.Lock()
	if previous := a.artifactCancels[command.GetCommandId()]; previous != nil {
		previous()
	}
	a.artifactCancels[command.GetCommandId()] = cancel
	a.artifactMu.Unlock()
	defer func() {
		cancel()
		a.artifactMu.Lock()
		delete(a.artifactCancels, command.GetCommandId())
		a.artifactMu.Unlock()
	}()
	ctx = commandCtx
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
	var last artifactDownloadProgress
	report := func(progress artifactDownloadProgress) {
		last = progress
		a.sendArtifactProgress(command, progress)
	}
	if command.GetOp() == agentv1.ArtifactOp_ARTIFACT_OP_FETCH {
		err = fetchHFSnapshot(ctx, command.GetArtifactIdentity(), cacheRoot, command.GetBearerToken(), report)
	} else {
		report(artifactDownloadProgress{Phase: "validating"})
	}
	candidate, validateErr := validateHFIdentity(ctx, command.GetArtifactIdentity(), cacheRoot)
	if err == nil {
		err = validateErr
	} else if validateErr == nil {
		// A complete managed snapshot is usable even if a redundant refresh
		// lost its remote connection.
		err = nil
	}
	if candidate.Identity != "" {
		a.sendPlacement(candidate)
	}
	if err != nil {
		a.result(command.GetCommandId(), false, 0, err.Error(), "", "")
		return
	}
	// bearer_token is intentionally not retained beyond this call.
	last.Phase = "complete"
	last.CurrentFile = ""
	a.sendArtifactProgress(command, last)
	a.result(command.GetCommandId(), true, 0, "", "", "")
}

func (a *Agent) sendArtifactProgress(command *agentv1.ArtifactCommand, progress artifactDownloadProgress) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_ArtifactProgress{
		ArtifactProgress: &agentv1.ArtifactProgress{
			CommandId: command.GetCommandId(), ArtifactIdentity: command.GetArtifactIdentity(),
			Phase: progress.Phase, CurrentFile: progress.CurrentFile,
			BytesDone: progress.BytesDone, BytesTotal: progress.BytesTotal,
			FilesDone: progress.FilesDone, FilesTotal: progress.FilesTotal,
		},
	}})
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
	candidate := placementCandidate{
		Identity: identity, Path: modelRoot, State: "valid",
		Size:        regularTreeSize(ctx, modelRoot),
		Diagnostics: hfSnapshotDiagnostics(ctx, identity, modelRoot, snapshot),
	}
	if len(candidate.Diagnostics) > 0 {
		candidate.State = "invalid"
		return candidate, fmt.Errorf("downloaded snapshot failed validation")
	}
	return candidate, nil
}

func fetchHFSnapshot(ctx context.Context, identity, cacheRoot, token string, reporters ...artifactProgressReporter) error {
	report := func(artifactDownloadProgress) {}
	if len(reporters) > 0 && reporters[0] != nil {
		report = reporters[0]
	}
	baseReport := report
	var reportMu sync.Mutex
	report = func(progress artifactDownloadProgress) {
		reportMu.Lock()
		defer reportMu.Unlock()
		baseReport(progress)
	}
	report(artifactDownloadProgress{Phase: "metadata"})
	base, revision, ok := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
	if !ok {
		return fmt.Errorf("invalid HF identity")
	}
	owner, repo, ok := strings.Cut(base, "/")
	if !ok {
		return fmt.Errorf("invalid HF repository")
	}
	client := &http.Client{Timeout: 2 * time.Hour}
	baseURL := &url.URL{Scheme: hfBaseURL.Scheme, Host: hfBaseURL.Host, Path: "/api/models/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/revision/" + revision}
	// ?blobs=true is required so Hugging Face populates size and lfs.
	baseURL.RawQuery = url.Values{"blobs": {"true"}}.Encode()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL.String(), nil)
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

	totalBytes := int64(0)
	for _, sibling := range info.Siblings {
		size := sibling.Size
		if sibling.LFS != nil {
			size = sibling.LFS.Size
		}
		if size < 0 || size > 1<<40 {
			return fmt.Errorf("HF file %s has invalid size", sibling.Name)
		}
		totalBytes += size
	}
	totalFiles := uint32(len(info.Siblings))
	report(artifactDownloadProgress{Phase: "downloading", BytesTotal: uint64(totalBytes), FilesTotal: totalFiles})

	modelRoot := filepath.Join(cacheRoot, "hub", "models--"+owner+"--"+repo)
	blobRoot := filepath.Join(modelRoot, "blobs")
	snapshotRoot := filepath.Join(modelRoot, "snapshots", revision)
	partialRoot := filepath.Join(modelRoot, ".downloads", revision)
	for _, dir := range []string{modelRoot, blobRoot, snapshotRoot, partialRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := makeModelTreeReadable(modelRoot); err != nil {
		return err
	}

	completedBytes := int64(0)
	filesDone := uint32(0)
	manifestFiles := make([]hfSnapshotFile, len(info.Siblings))
	pending := make([]hfDownloadJob, 0, len(info.Siblings))
	seenPaths := make(map[string]bool, len(info.Siblings))
	for index, sibling := range info.Siblings {
		rel, err := safeRelativePath(sibling.Name)
		if err != nil {
			return err
		}
		relPath := filepath.ToSlash(rel)
		if seenPaths[relPath] {
			return fmt.Errorf("HF metadata contains duplicate file %s", sibling.Name)
		}
		seenPaths[relPath] = true
		expectedSize, expectedDigest := sibling.Size, ""
		if sibling.LFS != nil {
			expectedSize, expectedDigest = sibling.LFS.Size, "sha256:"+sibling.LFS.SHA256
		}
		link := filepath.Join(snapshotRoot, rel)
		if existingSnapshotFile(link, expectedSize, expectedDigest) {
			actualDigest := expectedDigest
			if actualDigest == "" {
				var size int64
				actualDigest, size, err = digestFile(link)
				if err != nil || size != expectedSize {
					return fmt.Errorf("HF file %s digest or size mismatch", sibling.Name)
				}
			}
			manifestFiles[index] = hfSnapshotFile{
				Path: relPath, Size: expectedSize, Digest: actualDigest,
			}
			completedBytes += expectedSize
			filesDone++
			report(artifactDownloadProgress{
				Phase: "downloading", CurrentFile: relPath,
				BytesDone: uint64(completedBytes), BytesTotal: uint64(totalBytes),
				FilesDone: filesDone, FilesTotal: totalFiles,
			})
			continue
		}

		partial := filepath.Join(partialRoot, rel+".part")
		if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
			return err
		}
		pending = append(pending, hfDownloadJob{
			index: index, name: sibling.Name, rel: relPath,
			expectedSize: expectedSize, expectedDigest: expectedDigest,
			link: link, partial: partial,
			downloadURL: fmt.Sprintf(
				"%s://%s/%s/%s/resolve/%s/%s",
				hfBaseURL.Scheme,
				hfBaseURL.Host,
				url.PathEscape(owner),
				url.PathEscape(repo),
				revision,
				strings.ReplaceAll(url.PathEscape(relPath), "%2F", "/"),
			),
		})
	}

	if len(pending) > 0 {
		downloadCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		jobs := make(chan hfDownloadJob, len(pending))
		for _, job := range pending {
			jobs <- job
		}
		close(jobs)

		var (
			workers  sync.WaitGroup
			stateMu  sync.Mutex
			errorMu  sync.Mutex
			firstErr error
			active   = make(map[int]int64, hfDownloadConcurrency)
		)
		recordError := func(err error) {
			errorMu.Lock()
			if firstErr == nil {
				firstErr = err
				cancel()
			}
			errorMu.Unlock()
		}
		updateProgress := func(job hfDownloadJob, fileBytes int64, completed *hfSnapshotFile) {
			stateMu.Lock()
			if completed == nil {
				active[job.index] = fileBytes
			} else {
				delete(active, job.index)
				completedBytes += job.expectedSize
				filesDone++
				manifestFiles[job.index] = *completed
			}
			bytesDone := completedBytes
			for _, activeBytes := range active {
				bytesDone += activeBytes
			}
			report(artifactDownloadProgress{
				Phase: "downloading", CurrentFile: job.rel,
				BytesDone: uint64(bytesDone), BytesTotal: uint64(totalBytes),
				FilesDone: filesDone, FilesTotal: totalFiles,
			})
			stateMu.Unlock()
		}
		downloadOne := func(job hfDownloadJob) (hfSnapshotFile, error) {
			if err := retryResumeHTTPFile(downloadCtx, client, job.downloadURL, token, job.partial, job.expectedSize, func(fileBytes int64) {
				updateProgress(job, fileBytes, nil)
			}); err != nil {
				return hfSnapshotFile{}, err
			}
			digest, size, err := digestFile(job.partial)
			if err != nil || size != job.expectedSize || (job.expectedDigest != "" && digest != job.expectedDigest) {
				return hfSnapshotFile{}, fmt.Errorf("HF file %s digest or size mismatch", job.name)
			}
			blob := filepath.Join(blobRoot, strings.TrimPrefix(digest, "sha256:"))
			if _, err := os.Stat(blob); err == nil {
				if err := os.Remove(job.partial); err != nil {
					return hfSnapshotFile{}, err
				}
			} else if os.IsNotExist(err) {
				if err := os.Rename(job.partial, blob); err != nil {
					return hfSnapshotFile{}, err
				}
			} else {
				return hfSnapshotFile{}, err
			}
			if err := os.MkdirAll(filepath.Dir(job.link), 0o755); err != nil {
				return hfSnapshotFile{}, err
			}
			target, _ := filepath.Rel(filepath.Dir(job.link), blob)
			if err := os.Remove(job.link); err != nil && !os.IsNotExist(err) {
				return hfSnapshotFile{}, err
			}
			if err := os.Symlink(target, job.link); err != nil {
				return hfSnapshotFile{}, err
			}
			return hfSnapshotFile{Path: job.rel, Size: job.expectedSize, Digest: digest}, nil
		}

		workerCount := min(hfDownloadConcurrency, len(pending))
		workers.Add(workerCount)
		for range workerCount {
			go func() {
				defer workers.Done()
				for job := range jobs {
					if downloadCtx.Err() != nil {
						return
					}
					completed, err := downloadOne(job)
					if err != nil {
						recordError(fmt.Errorf("download HF file %s: %w", job.name, err))
						return
					}
					updateProgress(job, job.expectedSize, &completed)
				}
			}()
		}
		workers.Wait()
		errorMu.Lock()
		err := firstErr
		errorMu.Unlock()
		if err != nil {
			return err
		}
	}
	if err := writeHFCompletionManifest(modelRoot, revision, identity, manifestFiles); err != nil {
		return fmt.Errorf("write HF completion manifest: %w", err)
	}
	report(artifactDownloadProgress{
		Phase: "validating", BytesDone: uint64(completedBytes), BytesTotal: uint64(totalBytes),
		FilesDone: filesDone, FilesTotal: totalFiles,
	})
	return nil
}

func existingSnapshotFile(link string, expectedSize int64, expectedDigest string) bool {
	info, err := os.Stat(link)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return false
	}
	if expectedDigest == "" {
		return true
	}
	if target, err := os.Readlink(link); err == nil &&
		filepath.Base(target) == strings.TrimPrefix(expectedDigest, "sha256:") {
		return true
	}
	digest, size, err := digestFile(link)
	return err == nil && size == expectedSize && digest == expectedDigest
}

type downloadProgressWriter struct {
	writer     io.Writer
	done       int64
	lastDone   int64
	lastReport time.Time
	report     func(int64)
}

func (w *downloadProgressWriter) Write(data []byte) (int, error) {
	n, err := w.writer.Write(data)
	w.done += int64(n)
	if w.report != nil && (w.done-w.lastDone >= 64<<20 || time.Since(w.lastReport) >= time.Second || err != nil) {
		w.report(w.done)
		w.lastDone = w.done
		w.lastReport = time.Now()
	}
	return n, err
}

type downloadHTTPStatusError struct {
	status int
}

func (e *downloadHTTPStatusError) Error() string {
	return fmt.Sprintf("download HTTP %d", e.status)
}

func retryResumeHTTPFile(
	ctx context.Context,
	client *http.Client,
	sourceURL, token, destination string,
	expectedSize int64,
	reporters ...func(int64),
) error {
	var lastErr error
	for attempt := 1; attempt <= hfDownloadAttempts; attempt++ {
		lastErr = resumeHTTPFile(ctx, client, sourceURL, token, destination, expectedSize, reporters...)
		if lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var statusErr *downloadHTTPStatusError
		if errors.As(lastErr, &statusErr) &&
			statusErr.status != http.StatusRequestTimeout &&
			statusErr.status != http.StatusTooManyRequests &&
			statusErr.status < http.StatusInternalServerError {
			return lastErr
		}
		if attempt == hfDownloadAttempts {
			break
		}
		timer := time.NewTimer(time.Second << (attempt - 1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("download failed after %d attempts: %w", hfDownloadAttempts, lastErr)
}

func resumeHTTPFile(ctx context.Context, client *http.Client, sourceURL, token, destination string, expectedSize int64, reporters ...func(int64)) error {
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
	if offset == expectedSize {
		return os.Chmod(destination, 0o644)
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
		return &downloadHTTPStatusError{status: response.StatusCode}
	}
	// The blob is bind-mounted into the workload container, which runs as
	// root with all capabilities dropped (no CAP_DAC_OVERRIDE). It must be
	// world-readable (0644), not the OS default 0640, or the container gets
	// "Permission denied" on config.json / weight files.
	file, err := os.OpenFile(destination, flags, 0o644)
	if err != nil {
		return err
	}
	var report func(int64)
	if len(reporters) > 0 {
		report = reporters[0]
	}
	if report != nil {
		report(offset)
	}
	writer := &downloadProgressWriter{
		writer: file, done: offset, lastDone: offset, lastReport: time.Now(), report: report,
	}
	written, copyErr := io.Copy(writer, io.LimitReader(response.Body, expectedSize-offset+1))
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
	if report != nil {
		report(offset + written)
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
		if err := ensurePackageAssetsDir(final); err != nil {
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
	if err := ensurePackageAssetsDir(staging); err != nil {
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

// ensurePackageAssetsDir creates the assets bind-source directory when a
// package carries zero assets: the workload create always mounts
// <package>/assets read-only, so an empty layer must still leave the
// directory behind.
func ensurePackageAssetsDir(pkgDir string) error {
	if err := os.MkdirAll(filepath.Join(pkgDir, "assets"), 0o755); err != nil {
		return err
	}
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
