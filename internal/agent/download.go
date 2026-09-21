package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/errdefs"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func (a *Agent) downloadResult(id string, output []byte, err error) {
	result := &agentv1.CommandResult{CommandId: id, Ok: err == nil, OutputJson: output}
	if err != nil {
		result.Error = err.Error()
	}
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_CommandResult{CommandResult: result}})
}
func (a *Agent) handleDownload(ctx context.Context, command *agentv1.DownloadCommand) {
	if command == nil || command.GetCommandId() == "" || command.GetItemId() == "" {
		a.downloadResult(command.GetCommandId(), nil, fmt.Errorf("download.command_invalid"))
		return
	}
	if command.GetOp() == agentv1.DownloadOp_DOWNLOAD_OP_CANCEL {
		at, err := a.cancelAcquisition(ctx, command.GetTargetCommandId())
		if err != nil {
			a.downloadResult(command.GetCommandId(), nil, err)
			return
		}
		a.downloadResult(command.GetCommandId(), at.Output, nil)
		return
	}
	if command.GetOp() != agentv1.DownloadOp_DOWNLOAD_OP_INSPECT && command.GetOp() != agentv1.DownloadOp_DOWNLOAD_OP_FETCH {
		a.downloadResult(command.GetCommandId(), nil, fmt.Errorf("download.operation_invalid"))
		return
	}
	decode := downloads.DecodeResourceSpec
	if command.GetOp() == agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
		decode = downloads.DecodeInspectionSpec
	}
	spec, err := decode(command.GetResourceJson())
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	credential, err := downloadCredential(spec, command.GetCredentialJson())
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	cacheRoot, err := a.authorizeDownload(ctx, spec)
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	sum := sha256.Sum256(command.GetResourceJson())
	binding := fmt.Sprintf("%s:%d:%x", command.GetItemId(), command.GetOp(), sum)
	commandCtx, at, fresh, err := a.beginAcquisition(ctx, command.GetCommandId(), spec.Identity, spec.Destination, binding)
	if err != nil {
		a.downloadResult(command.GetCommandId(), nil, err)
		return
	}
	if !fresh {
		select {
		case <-at.done:
		case <-ctx.Done():
			return
		}
		var replayErr error
		if at.Error != "" {
			replayErr = fmt.Errorf("%s", at.Error)
		}
		a.downloadResult(command.GetCommandId(), at.Output, replayErr)
		return
	}
	if command.GetOp() == agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
		timeout := 2 * time.Minute
		if spec.Kind == downloads.ResourceArtifact && spec.SizeBytes != nil && *spec.SizeBytes > 0 {
			seconds := min(*spec.SizeBytes/(256<<20), int64((30*time.Minute-timeout)/time.Second))
			timeout += time.Duration(seconds) * time.Second
		} else if spec.Kind == downloads.ResourceArtifact {
			timeout = 30 * time.Minute
		}
		var cancel context.CancelFunc
		commandCtx, cancel = context.WithTimeout(commandCtx, timeout)
		defer cancel()
	}
	report := func(p artifactDownloadProgress) {
		if commandCtx.Err() != nil {
			return
		}
		if strings.HasPrefix(command.GetItemId(), "legacy-artifact:") {
			a.sendArtifactProgress(&agentv1.ArtifactCommand{CommandId: command.GetCommandId(), ArtifactIdentity: spec.Identity}, p)
		}
		var bytesTotal *uint64
		var filesTotal *uint32
		if p.BytesTotal > 0 {
			bytesTotal = &p.BytesTotal
		}
		if p.FilesTotal > 0 {
			filesTotal = &p.FilesTotal
		}
		a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_DownloadProgress{DownloadProgress: &agentv1.DownloadProgress{
			ItemId: command.GetItemId(), CommandId: command.GetCommandId(), Phase: p.Phase, CurrentFile: p.CurrentFile,
			BytesDone: p.BytesDone, BytesTotal: bytesTotal, FilesDone: p.FilesDone, FilesTotal: filesTotal,
		}}})
	}
	output, err := a.inspectDownload(commandCtx, spec, cacheRoot, credential, report)
	output.ItemID = command.GetItemId()
	if err == nil && command.GetOp() == agentv1.DownloadOp_DOWNLOAD_OP_FETCH && output.State != downloads.ResourceAvailable {
		if output.State == downloads.ResourceInvalid {
			err = fmt.Errorf("download.active_file_invalid: existing immutable content is corrupt; no in-place repair")
		} else {
			if spec.Source.Type == downloads.SourceFile && spec.SizeBytes == nil {
				spec.SizeBytes = output.SizeBytes
			}
			err = a.fetchDownload(commandCtx, spec, cacheRoot, credential, report)
			if err == nil {
				output, err = a.inspectDownload(commandCtx, spec, cacheRoot, credential, report)
				output.ItemID = command.GetItemId()
			}
			if err == nil && output.State != downloads.ResourceAvailable {
				err = fmt.Errorf("download.verification_failed")
			}
		}
	}
	if err == nil {
		err = commandCtx.Err()
	}
	if err != nil && credential != nil {
		err = redactDownloadError(err, credential)
	}
	if err == nil && output.State == downloads.ResourceAvailable && spec.Kind != downloads.ResourceImage {
		size := int64(0)
		if output.SizeBytes != nil {
			size = *output.SizeBytes
		}
		a.sendPlacement(placementCandidate{Identity: spec.Identity, Path: spec.Destination, State: "valid", Size: size})
	}
	raw, _ := json.Marshal(output)
	a.finishAcquisition(at, raw, err)
	if at.Error != "" {
		err = fmt.Errorf("%s", at.Error)
	}
	a.downloadResult(command.GetCommandId(), raw, err)
}

func downloadCredential(spec downloads.ResourceSpec, raw []byte) (*downloads.CredentialMaterial, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > downloads.MaxCredentialJSONBytes {
		return nil, fmt.Errorf("download.credential_invalid")
	}
	var material downloads.CredentialMaterial
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&material) != nil || decoder.Decode(new(any)) != io.EOF || material.Value == "" {
		return nil, fmt.Errorf("download.credential_invalid")
	}
	expectedHost, purpose := "", ""
	switch spec.Source.Type {
	case downloads.SourceHuggingFace:
		expectedHost, purpose = "huggingface.co", "huggingface"
	case downloads.SourceOCI:
		expectedHost, purpose = strings.Split(spec.Source.Reference, "/")[0], "registry"
	default:
		return nil, fmt.Errorf("download.credential_scope_invalid")
	}
	if material.Host != expectedHost || material.Purpose != purpose {
		return nil, fmt.Errorf("download.credential_scope_invalid")
	}
	return &material, nil
}
func (a *Agent) authorizeDownload(ctx context.Context, spec downloads.ResourceSpec) (string, error) {
	if err := safeDestination(spec.Destination); err != nil {
		return "", err
	}
	if spec.Kind == downloads.ResourceImage {
		storage, err := a.rt.ImageStorage(ctx)
		if err != nil {
			return "", fmt.Errorf("download.image_storage_unavailable")
		}
		if storage.Root != spec.Destination {
			return "", fmt.Errorf("download.destination_unconfigured")
		}
		return "", nil
	}
	if spec.Kind == downloads.ResourceRecipe {
		expected := filepath.Join(a.cfg.StateRoot, "recipes", strings.TrimPrefix(spec.Source.Digest, "sha256:"))
		if expected == spec.Destination {
			return "", nil
		}
	} else {
		for _, root := range a.cfg.CacheRoots {
			switch spec.Source.Type {
			case downloads.SourceHuggingFace:
				name := "models--" + strings.ReplaceAll(spec.Source.Reference, "/", "--")
				if spec.Destination == filepath.Join(root, "hub", name) || spec.Destination == filepath.Join(root, name) {
					return root, nil
				}
			case downloads.SourceFile, downloads.SourceLocal:
				if spec.Destination == filepath.Join(root, "files", strings.TrimPrefix(spec.Source.Digest, "sha256:")) {
					return root, nil
				}
			case downloads.SourceOCI:
				if spec.Destination == filepath.Join(root, "oci", strings.TrimPrefix(spec.Source.Digest, "sha256:")) {
					return root, nil
				}
			}
		}
	}
	return "", fmt.Errorf("download.destination_unconfigured")
}
func storageObservation(info *runtime.ImageStorageInfo) *downloads.StorageObservation {
	if info == nil {
		return nil
	}
	free, total := int64(info.FreeBytes), int64(info.TotalBytes)
	return &downloads.StorageObservation{Filesystem: info.Filesystem, Destination: info.Root, AvailableBytes: &free, TotalBytes: &total}
}
func (a *Agent) inspectDownload(ctx context.Context, spec downloads.ResourceSpec, cacheRoot string, credential *downloads.CredentialMaterial, report artifactProgressReporter) (downloads.CommandOutput, error) {
	out := downloads.CommandOutput{Identity: spec.Identity, Path: spec.Destination, Platform: spec.Platform, State: downloads.ResourceMissing, SizeBytes: spec.SizeBytes}
	report(artifactDownloadProgress{Phase: "verifying"})
	if spec.Kind == downloads.ResourceImage {
		out.SizeBytes = nil // Compressed registry descriptors do not establish engine capacity.
		storage, err := a.rt.ImageStorage(ctx)
		if err != nil {
			return out, fmt.Errorf("download.image_storage_unavailable")
		}
		out.Storage = storageObservation(storage)
		pinned := spec.IndexDigest
		if pinned == "" {
			pinned = spec.ManifestDigest
		}
		reference := spec.Source.Reference + "@" + pinned
		info, err := a.rt.InspectImage(ctx, reference, spec.Platform)
		if err != nil {
			if errdefs.IsNotFound(err) || strings.Contains(err.Error(), "image.not_found") || strings.Contains(err.Error(), "image.platform_content_missing") {
				return out, nil
			}
			return out, fmt.Errorf("download.image_inspection_failed: %s", err)
		}
		if info.Digest != pinned || info.Platform != spec.Platform {
			out.State = downloads.ResourceInvalid
			return out, nil
		}
		if info.ManifestDigest == "" && spec.ManifestDigest != "" {
			// Classic Docker stores do not expose index descriptors. Verify the
			// separately named child as well; never infer its presence from an index.
			child := info
			if spec.ManifestDigest != pinned {
				child, err = a.rt.InspectImage(ctx, spec.Source.Reference+"@"+spec.ManifestDigest, spec.Platform)
				if err != nil {
					if errdefs.IsNotFound(err) || strings.Contains(err.Error(), "image.not_found") {
						return out, nil
					}
					return out, fmt.Errorf("download.image_inspection_failed: %s", err)
				}
			}
			if child.Digest != spec.ManifestDigest || child.Platform != spec.Platform {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			out.IndexDigest, out.ManifestDigest = pinned, child.Digest
			out.SizeBytes = &child.SizeBytes
			out.State = downloads.ResourceAvailable
		} else {
			if info.IndexDigest != pinned || info.ManifestDigest == "" {
				return out, fmt.Errorf("download.image_manifest_observation_unsupported")
			}
			if spec.ManifestDigest != "" && info.ManifestDigest != spec.ManifestDigest {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			out.IndexDigest, out.ManifestDigest = info.IndexDigest, info.ManifestDigest
			out.SizeBytes = &info.SizeBytes
			out.State = downloads.ResourceAvailable
		}
	} else {
		storage, err := runtime.InspectStorage(spec.Destination)
		if err != nil {
			return out, err
		}
		out.Storage = storageObservation(storage)
		switch spec.Source.Type {
		case downloads.SourceHuggingFace:
			state, size, err := inspectHFSnapshot(ctx, spec, credential)
			out.State, out.SizeBytes = state, size
			if err != nil {
				return out, err
			}
		case downloads.SourceRecipe:
			if _, err := os.Stat(filepath.Join(spec.Destination, "oci-layout")); os.IsNotExist(err) {
				manifestJSON, _, _, err := a.readRecipeTransport(ctx, spec.Source.Digest, false)
				if err != nil {
					return out, err
				}
				manifest, err := decodePackageManifest(manifestJSON, spec.Source.Digest)
				if err != nil {
					return out, err
				}
				// Asset extraction has a strict format bound; include that upper bound
				// instead of treating a compressed layer size as expanded disk usage.
				size := manifest.Config.Size + manifest.Layers[0].Size + int64(len(manifestJSON)) + recipe.MaxExtractedAssetBytes
				out.SizeBytes = &size
				if _, err := os.Stat(spec.Destination); err == nil {
					out.State = downloads.ResourcePartial
				}
				return out, nil
			}
			packed, err := recipe.ReadLayout(spec.Destination)
			if err != nil || packed.ManifestDigest != spec.Source.Digest {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			if err := recipe.VerifyExtractedAssets(spec.Destination); err != nil {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			size := regularTreeSize(ctx, spec.Destination)
			out.SizeBytes = &size
			out.State = downloads.ResourceAvailable
		case downloads.SourceFile, downloads.SourceLocal:
			info, err := os.Lstat(spec.Destination)
			if os.IsNotExist(err) {
				if spec.Source.URL != "" && out.SizeBytes == nil {
					size, err := inspectHTTPSFileSize(ctx, spec.Source.URL)
					if err != nil {
						return out, err
					}
					out.SizeBytes = size
				}
				return out, nil
			}
			if err != nil {
				return out, err
			}
			if !info.Mode().IsRegular() {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			digest, size, err := digestFile(ctx, spec.Destination)
			out.SizeBytes = &size
			if err != nil || digest != spec.Source.Digest || (spec.SizeBytes != nil && size != *spec.SizeBytes) {
				out.State = downloads.ResourceInvalid
				return out, nil
			}
			out.State = downloads.ResourceAvailable
		case downloads.SourceOCI:
			return out, fmt.Errorf("download.oci_layout_unsupported: no declared artifact mount layout")
		}
	}
	if out.State == downloads.ResourceAvailable {
		if spec.Kind != downloads.ResourceImage {
			_, treeSize, tree, err := collectResourceManifest(ctx, spec.Identity, spec.Destination)
			if err != nil {
				return out, err
			}
			out.TreeDigest = tree
			size := int64(treeSize)
			out.TreeSizeBytes = &size
		}
		now := time.Now().UTC()
		out.VerifiedAt = &now
	}
	return out, ctx.Err()
}
func (a *Agent) fetchDownload(ctx context.Context, spec downloads.ResourceSpec, cacheRoot string, credential *downloads.CredentialMaterial, report artifactProgressReporter) error {
	switch {
	case spec.Kind == downloads.ResourceImage:
		var auth *runtime.Auth
		if credential != nil {
			if strings.ContainsAny(credential.Value, "\r\n") || strings.HasPrefix(strings.TrimSpace(credential.Value), "{") {
				return fmt.Errorf("download.credential_format_invalid")
			}
			auth = &runtime.Auth{RegistryToken: credential.Value, ServerAddress: credential.Host}
		}
		pinned := spec.IndexDigest
		if pinned == "" {
			pinned = spec.ManifestDigest
		}
		layers := map[string]runtime.ImagePullProgress{}
		pull := &runtime.PullSpec{Reference: spec.Source.Reference + "@" + pinned, Platform: spec.Platform, Auth: auth, Progress: func(p runtime.ImagePullProgress) {
			if p.BytesTotal > 0 && !strings.EqualFold(p.Phase, "Extracting") {
				layers[p.Layer] = p
			}
			var done, total uint64
			for _, layer := range layers {
				done += uint64(max(0, layer.BytesDone))
				total += uint64(max(0, layer.BytesTotal))
			}
			report(artifactDownloadProgress{Phase: p.Phase, CurrentFile: p.Layer, BytesDone: done, BytesTotal: total})
		}}
		if err := a.rt.Pull(ctx, pull); err != nil {
			return err
		}
		if spec.ManifestDigest != pinned {
			info, err := a.rt.InspectImage(ctx, pull.Reference, spec.Platform)
			if err != nil {
				return err
			}
			if info.ManifestDigest == "" {
				pull.Reference = spec.Source.Reference + "@" + spec.ManifestDigest
				return a.rt.Pull(ctx, pull)
			}
		}
		return nil
	case spec.Source.Type == downloads.SourceHuggingFace:
		token := ""
		if credential != nil {
			token = credential.Value
		}
		return fetchHFSnapshot(ctx, spec.Identity, cacheRoot, token, report)
	case spec.Source.Type == downloads.SourceRecipe:
		return a.fetchRecipePackage(ctx, spec.Identity)
	case spec.Source.Type == downloads.SourceFile && spec.Source.URL != "":
		if spec.SizeBytes == nil {
			return fmt.Errorf("download.size_unknown")
		}
		if err := os.MkdirAll(filepath.Dir(spec.Destination), 0755); err != nil {
			return err
		}
		partial := spec.Destination + ".part"
		if err := safeDestination(partial); err != nil {
			return err
		}
		client := &http.Client{Timeout: 2 * time.Hour, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 10 || req.URL.Scheme != "https" || req.URL.User != nil {
				return fmt.Errorf("download.redirect_unsafe")
			}
			req.Header.Del("Authorization")
			return nil
		}}
		if info, err := os.Stat(partial); err == nil && info.Size() == *spec.SizeBytes && !existingSnapshotFile(ctx, partial, *spec.SizeBytes, spec.Source.Digest) {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(partial); err != nil {
				return err
			}
		}
		for range 2 {
			if err := resumeHTTPFile(ctx, client, spec.Source.URL, "", partial, *spec.SizeBytes, func(done int64) {
				report(artifactDownloadProgress{Phase: "downloading", BytesDone: uint64(done), BytesTotal: uint64(*spec.SizeBytes)})
			}); err != nil {
				return err
			}
			if existingSnapshotFile(ctx, partial, *spec.SizeBytes, spec.Source.Digest) {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := os.Link(partial, spec.Destination); err != nil {
					return err
				}
				return os.Remove(partial)
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := os.Remove(partial); err != nil {
				return err
			}
		}
		return fmt.Errorf("download.checksum_mismatch")
	default:
		return fmt.Errorf("download.source_unavailable")
	}
}

func inspectHFSnapshot(ctx context.Context, spec downloads.ResourceSpec, credential *downloads.CredentialMaterial) (downloads.ResourceState, *int64, error) {
	recordPath := hfSnapshotManifestPath(spec.Destination, spec.Source.Revision)
	if data, err := os.ReadFile(recordPath); err == nil && len(data) <= 16<<20 {
		var record hfSnapshotManifest
		if json.Unmarshal(data, &record) == nil && record.Version == 2 && record.Identity == spec.Identity && len(record.Files) > 0 {
			var size int64
			for _, file := range record.Files {
				size += file.Size
			}
			if len(hfSnapshotDiagnostics(ctx, spec.Identity, spec.Destination, filepath.Join(spec.Destination, "snapshots", spec.Source.Revision))) == 0 {
				return downloads.ResourceAvailable, &size, nil
			}
		}
	}
	if spec.SizeBytes != nil {
		if _, err := os.Stat(filepath.Join(spec.Destination, "snapshots", spec.Source.Revision)); os.IsNotExist(err) {
			return downloads.ResourceMissing, spec.SizeBytes, nil
		} else if err != nil {
			return downloads.ResourceUnknown, spec.SizeBytes, err
		}
	}
	token := ""
	if credential != nil {
		token = credential.Value
	}
	info, err := readHFMetadata(ctx, spec.Source.Reference, spec.Source.Revision, token)
	if err != nil {
		return downloads.ResourceUnknown, nil, err
	}
	snapshot := filepath.Join(spec.Destination, "snapshots", spec.Source.Revision)
	var total int64
	files := make([]hfSnapshotFile, 0, len(info.Siblings))
	state := downloads.ResourceAvailable
	verifiedPartial := false
	seen := map[string]bool{}
	for _, sibling := range info.Siblings {
		rel, err := safeRelativePath(sibling.Name)
		if err != nil {
			return downloads.ResourceUnknown, nil, err
		}
		if seen[rel] {
			return downloads.ResourceUnknown, nil, fmt.Errorf("download.metadata_duplicate_path")
		}
		seen[rel] = true
		size, digest := sibling.Size, "git-sha1:"+sibling.BlobID
		if sibling.LFS != nil {
			size, digest = sibling.LFS.Size, "sha256:"+sibling.LFS.SHA256
		}
		if size < 0 || size > 1<<40 || !validHFFileDigest(digest) {
			return downloads.ResourceUnknown, nil, fmt.Errorf("download.metadata_unverifiable")
		}
		total += size
		path := filepath.Join(snapshot, rel)
		if resolved, err := filepath.EvalSymlinks(path); err == nil && !pathWithin(resolved, spec.Destination) {
			return downloads.ResourceInvalid, &total, fmt.Errorf("download.snapshot_symlink_escape")
		}
		if !existingSnapshotFile(ctx, path, size, digest) {
			if _, err := os.Lstat(path); err == nil {
				state = downloads.ResourceInvalid
			} else if state != downloads.ResourceInvalid {
				state = downloads.ResourceMissing
			}
			partial := filepath.Join(spec.Destination, ".downloads", spec.Source.Revision, rel+".part")
			if err := safeDestination(partial); err != nil {
				return downloads.ResourceUnknown, nil, err
			}
			if existingSnapshotFile(ctx, partial, size, digest) {
				verifiedPartial = true
			}
		} else {
			verifiedPartial = true
		}
		files = append(files, hfSnapshotFile{Path: filepath.ToSlash(rel), Size: size, Digest: digest})
	}
	if state == downloads.ResourceMissing && verifiedPartial {
		state = downloads.ResourcePartial
	}
	if state == downloads.ResourceAvailable {
		if err := ctx.Err(); err != nil {
			return downloads.ResourceUnknown, &total, err
		}
		if err := writeHFCompletionManifest(spec.Destination, spec.Source.Revision, spec.Identity, files); err != nil {
			return downloads.ResourceUnknown, &total, err
		}
	}
	return state, &total, nil
}
func readHFMetadata(ctx context.Context, repository, revision, token string) (*hfModelInfo, error) {
	target := *hfBaseURL
	target.Path = "/api/models/" + repository + "/revision/" + revision
	target.RawQuery = "blobs=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || req.URL.Scheme != hfBaseURL.Scheme || req.URL.Host != hfBaseURL.Host {
			return fmt.Errorf("download.metadata_redirect_rejected")
		}
		return nil
	}}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download.metadata_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download.metadata_http_%d", response.StatusCode)
	}
	var info hfModelInfo
	if json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&info) != nil || info.SHA != revision || len(info.Siblings) == 0 {
		return nil, fmt.Errorf("download.metadata_revision_invalid")
	}
	return &info, nil
}
