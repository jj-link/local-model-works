package agent

import (
	"context"
	"crypto/sha1"
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
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/hf"
	"github.com/jj-link/local-model-works/internal/recipe"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// hfBaseURL is the Hugging Face API/download origin. Overridden in tests.
var hfBaseURL = &url.URL{Scheme: "https", Host: "huggingface.co"}

type hfModelInfo struct {
	SHA      string `json:"sha"`
	Siblings []struct {
		Name   string `json:"rfilename"`
		BlobID string `json:"blobId"`
		Size   int64  `json:"size"`
		LFS    *struct {
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
	Version  int              `json:"version"`
	Identity string           `json:"identity"`
	Files    []hfSnapshotFile `json:"files"`
}

func hfSnapshotManifestPath(modelRoot, revision string) string {
	return filepath.Join(modelRoot, ".lmw", "snapshots", revision+".json")
}

func writeHFCompletionManifest(modelRoot, revision, identity string, files []hfSnapshotFile) error {
	path := hfSnapshotManifestPath(modelRoot, revision)
	if err := safeDestination(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(hfSnapshotManifest{Version: 2, Identity: identity, Files: files})
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
		manifest.Version != 2 || manifest.Identity != identity || len(manifest.Files) == 0 {
		return append(out, &agentv1.Diagnostic{
			Code: "artifact.snapshot_manifest_invalid", Severity: "error",
			Message: "snapshot completion manifest is invalid", Resource: path,
		})
	}
	for _, expected := range manifest.Files {
		if ctx.Err() != nil {
			return append(out, &agentv1.Diagnostic{Code: "artifact.verification_interrupted", Severity: "error", Message: "Snapshot verification was interrupted", Resource: snapshot})
		}
		rel, err := safeRelativePath(expected.Path)
		if err != nil || !validHFFileDigest(expected.Digest) || expected.Size < 0 ||
			!existingSnapshotFile(ctx, filepath.Join(snapshot, rel), expected.Size, expected.Digest) {
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
	next := &agentv1.DownloadCommand{CommandId: command.GetCommandId(), ItemId: "legacy-artifact:" + command.GetCommandId()}
	switch command.GetOp() {
	case agentv1.ArtifactOp_ARTIFACT_OP_CANCEL:
		next.Op = agentv1.DownloadOp_DOWNLOAD_OP_CANCEL
		next.TargetCommandId = command.GetTargetCommandId()
		a.handleDownload(ctx, next)
		return
	case agentv1.ArtifactOp_ARTIFACT_OP_FETCH:
		next.Op = agentv1.DownloadOp_DOWNLOAD_OP_FETCH
	case agentv1.ArtifactOp_ARTIFACT_OP_VALIDATE:
		next.Op = agentv1.DownloadOp_DOWNLOAD_OP_INSPECT
	default:
		a.result(command.GetCommandId(), false, 0, "artifact.unsupported_operation", "", "")
		return
	}
	spec, err := a.legacyResource(command.GetArtifactIdentity(), command.GetCacheRoot())
	if err != nil {
		a.result(command.GetCommandId(), false, 0, err.Error(), "", "")
		return
	}
	next.ResourceJson, _ = json.Marshal(spec)
	if command.GetBearerToken() != "" {
		next.CredentialJson, _ = json.Marshal(downloads.CredentialMaterial{Purpose: "huggingface", Host: "huggingface.co", Value: command.GetBearerToken()})
	}
	a.handleDownload(ctx, next)
}

func (a *Agent) legacyResource(identity, cacheRoot string) (downloads.ResourceSpec, error) {
	spec := downloads.ResourceSpec{Kind: downloads.ResourceArtifact, Identity: identity}
	parsed, err := artifactidentity.Parse(identity)
	if err != nil {
		return spec, err
	}
	if strings.HasPrefix(identity, "recipe://") {
		spec.Kind = downloads.ResourceRecipe
		spec.Source = downloads.SourceSpec{Type: downloads.SourceRecipe, Digest: parsed.Digest}
		spec.Destination = filepath.Join(a.cfg.StateRoot, "recipes", strings.TrimPrefix(parsed.Digest, "sha256:"))
		return spec, nil
	}
	if cacheRoot == "" && len(a.cfg.CacheRoots) > 0 {
		cacheRoot = a.cfg.CacheRoots[0]
	}
	if !contains(a.cfg.CacheRoots, cacheRoot) {
		return spec, fmt.Errorf("download.destination_unconfigured")
	}
	if parsed.Kind == "model" {
		repository, revision, _ := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
		spec.Source = downloads.SourceSpec{Type: downloads.SourceHuggingFace, Reference: repository, Revision: revision}
		name := "models--" + strings.ReplaceAll(repository, "/", "--")
		spec.Destination = filepath.Join(cacheRoot, "hub", name)
		if _, err := os.Stat(spec.Destination); os.IsNotExist(err) {
			if _, err := os.Stat(filepath.Join(cacheRoot, name)); err == nil {
				spec.Destination = filepath.Join(cacheRoot, name)
			}
		}
		return spec, nil
	}
	return spec, fmt.Errorf("download.source_unavailable")
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
	client := &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) > 10 || request.URL.User != nil || request.URL.Scheme != hfBaseURL.Scheme {
			return fmt.Errorf("download.redirect_unsafe")
		}
		if request.URL.Host != hfBaseURL.Host {
			request.Header.Del("Authorization")
		}
		return nil
	}}
	info, err := readHFMetadata(ctx, base, revision, token)
	if err != nil {
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
	if _, err := os.Stat(modelRoot); os.IsNotExist(err) {
		if _, err := os.Stat(filepath.Join(cacheRoot, "models--"+owner+"--"+repo)); err == nil {
			modelRoot = filepath.Join(cacheRoot, "models--"+owner+"--"+repo)
		}
	}
	blobRoot := filepath.Join(modelRoot, "blobs")
	snapshotRoot := filepath.Join(modelRoot, "snapshots", revision)
	partialRoot := filepath.Join(modelRoot, ".downloads", revision)
	for _, dir := range []string{modelRoot, blobRoot, snapshotRoot, partialRoot} {
		if err := safeDestination(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
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
		expectedSize, expectedDigest := sibling.Size, "git-sha1:"+sibling.BlobID
		if sibling.LFS != nil {
			expectedSize, expectedDigest = sibling.LFS.Size, "sha256:"+sibling.LFS.SHA256
		}
		if !validHFFileDigest(expectedDigest) {
			return fmt.Errorf("HF file %s has no supported upstream checksum", sibling.Name)
		}
		link := filepath.Join(snapshotRoot, rel)
		if err := safeDestination(filepath.Dir(link)); err != nil {
			return err
		}
		if resolved, err := filepath.EvalSymlinks(link); err == nil && !pathWithin(resolved, modelRoot) {
			return fmt.Errorf("download.snapshot_symlink_escape")
		}
		if existingSnapshotFile(ctx, link, expectedSize, expectedDigest) {
			actualDigest := expectedDigest
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
		if _, err := os.Lstat(link); err == nil {
			return fmt.Errorf("download.active_file_invalid: existing snapshot file %s must not be overwritten", sibling.Name)
		} else if !os.IsNotExist(err) {
			return err
		}

		partial := filepath.Join(partialRoot, rel+".part")
		if err := safeDestination(partial); err != nil {
			return err
		}
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
			if info, err := os.Stat(job.partial); err == nil && info.Size() == job.expectedSize &&
				!existingSnapshotFile(downloadCtx, job.partial, job.expectedSize, job.expectedDigest) {
				if err := downloadCtx.Err(); err != nil {
					return hfSnapshotFile{}, err
				}
				if err := os.Remove(job.partial); err != nil {
					return hfSnapshotFile{}, err
				}
			}
			if err := retryResumeHTTPFile(downloadCtx, client, job.downloadURL, token, job.partial, job.expectedSize, func(fileBytes int64) {
				updateProgress(job, fileBytes, nil)
			}); err != nil {
				return hfSnapshotFile{}, err
			}
			if !existingSnapshotFile(downloadCtx, job.partial, job.expectedSize, job.expectedDigest) {
				// Retained prefixes are untrusted until the complete checksum verifies.
				if err := downloadCtx.Err(); err != nil {
					return hfSnapshotFile{}, err
				}
				if removeErr := os.Remove(job.partial); removeErr != nil {
					return hfSnapshotFile{}, removeErr
				}
				if err := retryResumeHTTPFile(downloadCtx, client, job.downloadURL, token, job.partial, job.expectedSize); err != nil {
					return hfSnapshotFile{}, err
				}
				if !existingSnapshotFile(downloadCtx, job.partial, job.expectedSize, job.expectedDigest) {
					return hfSnapshotFile{}, fmt.Errorf("HF file %s digest mismatch", job.name)
				}
			}
			if err := downloadCtx.Err(); err != nil {
				return hfSnapshotFile{}, err
			}
			_, blobName, _ := strings.Cut(job.expectedDigest, ":")
			blob := filepath.Join(blobRoot, blobName)
			if err := safeDestination(blob); err != nil {
				return hfSnapshotFile{}, err
			}
			if _, err := os.Stat(blob); err == nil {
				if !existingSnapshotFile(downloadCtx, blob, job.expectedSize, job.expectedDigest) {
					return hfSnapshotFile{}, fmt.Errorf("download.active_file_invalid: existing immutable blob is corrupt")
				}
				if err := os.Remove(job.partial); err != nil {
					return hfSnapshotFile{}, err
				}
			} else if os.IsNotExist(err) {
				if err := os.Link(job.partial, blob); err != nil {
					return hfSnapshotFile{}, err
				}
				if err := os.Remove(job.partial); err != nil {
					return hfSnapshotFile{}, err
				}
			} else {
				return hfSnapshotFile{}, err
			}
			if err := os.MkdirAll(filepath.Dir(job.link), 0o755); err != nil {
				return hfSnapshotFile{}, err
			}
			target, _ := filepath.Rel(filepath.Dir(job.link), blob)
			if err := safeDestination(filepath.Dir(job.link)); err != nil {
				return hfSnapshotFile{}, err
			}
			if err := os.Symlink(target, job.link); err != nil {
				return hfSnapshotFile{}, err
			}
			return hfSnapshotFile{Path: job.rel, Size: job.expectedSize, Digest: job.expectedDigest}, nil
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
	if err := ctx.Err(); err != nil {
		return err
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

func existingSnapshotFile(ctx context.Context, link string, expectedSize int64, expectedDigest string) bool {
	info, err := os.Stat(link)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expectedSize {
		return false
	}
	if !validHFFileDigest(expectedDigest) {
		return false
	}
	if strings.HasPrefix(expectedDigest, "git-sha1:") {
		file, err := os.Open(link)
		if err != nil {
			return false
		}
		defer file.Close()
		hash := sha1.New()
		_, _ = fmt.Fprintf(hash, "blob %d\x00", expectedSize)
		size, err := io.Copy(hash, contextReader{ctx: ctx, reader: file})
		return err == nil && size == expectedSize && "git-sha1:"+hex.EncodeToString(hash.Sum(nil)) == expectedDigest
	}
	digest, size, err := digestFile(ctx, link)
	return err == nil && size == expectedSize && digest == expectedDigest
}

func validHFFileDigest(digest string) bool {
	prefix, value, ok := strings.Cut(digest, ":")
	if !ok || (prefix != "sha256" || len(value) != 64) && (prefix != "git-sha1" || len(value) != 40) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) > 0 && strings.ToLower(value) == value
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
		want := fmt.Sprintf("bytes %d-%d/%d", offset, expectedSize-1, expectedSize)
		if response.Header.Get("Content-Range") != want {
			return fmt.Errorf("download.content_range_invalid")
		}
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

func digestFile(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), size, nil
}

func (a *Agent) fetchRecipePackage(ctx context.Context, identity string) error {
	digest := strings.TrimPrefix(identity, "recipe://")
	manifest, config, layer, err := a.readRecipeTransport(ctx, digest, true)
	if err != nil {
		return err
	}
	root := filepath.Join(a.cfg.StateRoot, "recipes")
	final := filepath.Join(root, strings.TrimPrefix(digest, "sha256:"))
	if err := safeDestination(final); err != nil {
		return err
	}
	if _, err := os.Stat(final); err == nil {
		// Existing mounted helpers are immutable, including legacy asset-only trees.
		if err := verifyRecipeBytes(ctx, final, digest, manifest, config, layer); err != nil {
			return err
		}
		// Complete missing package metadata only after matching every existing
		// helper byte. Existing assets and conflicting metadata are never changed.
		return writeReceivedLayout(final, digest, manifest, config, layer)
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(root, ".package-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	if err := writeReceivedLayout(staging, digest, manifest, config, layer); err != nil {
		return err
	}
	packed, err := recipe.ReadLayout(staging)
	if err != nil {
		return err
	}
	path, _, err := recipe.PersistPackage(root, packed)
	if err != nil {
		return err
	}
	if err := makePackageTraversable(path); err != nil {
		return err
	}
	a.sendPlacement(placementCandidate{Identity: identity, Path: path, State: "valid", Size: regularTreeSize(ctx, path)})
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
