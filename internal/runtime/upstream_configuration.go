package runtime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

// Only bytes written by this adapter may be replaced on a later configuration.
// Lifecycle fingerprints separately preserve unrelated upstream installation edits.
type upstreamConfigurationState struct {
	Original string `json:"original"`
	Result   string `json:"result"`
	// Applied is the adapter-only rendering, never an authored lifecycle result.
	// It lets future bindings change while retaining authenticated installer edits.
	Applied []byte `json:"applied"`
}

func sourceBytesHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// configurationPath rejects even in-tree symlinks: the pinned Git blob must refer
// to precisely this regular file, not an authored alias to a different target.
func configurationPath(repository, relative string) (string, error) {
	if !sourceconfig.SafePath(relative) {
		return "", fmt.Errorf("upstream.configuration_path_invalid: %s", relative)
	}
	current := repository
	for _, part := range strings.Split(relative, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("upstream.configuration_symlink: %s", relative)
		}
	}
	resolved, err := upstreamPath(repository, relative, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > sourceconfig.MaxSourceBytes {
		return "", fmt.Errorf("upstream.configuration_file_invalid: %s", relative)
	}
	return resolved, nil
}

func pinnedConfigurationBytes(ctx context.Context, repository, revision, relative string) ([]byte, error) {
	// ls-tree verifies a regular tracked blob rather than following a symlink or
	// reading untracked working bytes. --literal-pathspecs prevents path globs.
	entry, err := exec.CommandContext(ctx, "git", "--literal-pathspecs", "-C", repository, "ls-tree", "-z", revision, "--", relative).Output()
	if err != nil {
		return nil, fmt.Errorf("upstream.configuration_tracked_source: %w", err)
	}
	metadata, name, ok := strings.Cut(strings.TrimSuffix(string(entry), "\x00"), "\t")
	fields := strings.Fields(metadata)
	if !ok || name != relative || len(fields) != 3 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
		return nil, fmt.Errorf("upstream.configuration_not_tracked_regular_file: %s", relative)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repository, "cat-file", "blob", fields[2])
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(pipe, sourceconfig.MaxSourceBytes+1))
	if readErr != nil || len(data) > sourceconfig.MaxSourceBytes {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("upstream.configuration_source_too_large_or_unreadable: %s", relative)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("upstream.configuration_source_read: %w", err)
	}
	return data, nil
}

func upstreamConfiguredBaseline(ctx context.Context, spec *ContainerSpec, repository, relative string, original, current []byte, prior upstreamConfigurationState) ([]byte, error) {
	if prior.Original != sourceBytesHash(original) {
		return nil, fmt.Errorf("upstream.configuration_original_changed: %s", relative)
	}
	if prior.Applied != nil && sourceBytesHash(prior.Applied) == prior.Result {
		return prior.Applied, nil
	}
	// Older records stored only hashes. Recover only a provable rendering, not
	// arbitrary working bytes, and persist full adapter ownership on next apply.
	if sourceBytesHash(current) == prior.Result {
		return current, nil
	}
	for _, file := range spec.Upstream.Configuration {
		if filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, file.Path)) != relative {
			continue
		}
		applied, err := sourceconfig.Apply(original, file.Edits)
		if err != nil {
			return nil, err
		}
		if sourceBytesHash(applied) == prior.Result {
			return applied, nil
		}
	}
	return nil, fmt.Errorf("upstream.configuration_ownership_unavailable: %s", relative)
}

func configureUpstreamSource(ctx context.Context, spec *ContainerSpec, repository string) error {
	if err := sourceconfig.ValidateResolved(spec.Upstream.Configuration); err != nil {
		return err
	}
	if err := verifyUpstreamSource(ctx, spec, repository); err != nil {
		return err
	}
	statePath := filepath.Join(filepath.Dir(repository), "configuration-state.json")
	previous := make(map[string]upstreamConfigurationState)
	if err := readUpstreamJSON(statePath, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	requested := make(map[string]sourceconfig.ResolvedFile, len(spec.Upstream.Configuration))
	for _, file := range spec.Upstream.Configuration {
		relative := filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, file.Path))
		if !sourceconfig.SafePath(relative) {
			return fmt.Errorf("upstream.configuration_path_invalid: %s", relative)
		}
		requested[relative] = file
	}
	paths := make([]string, 0, len(previous)+len(requested))
	for relative := range previous {
		paths = append(paths, relative)
	}
	for relative := range requested {
		if _, exists := previous[relative]; !exists {
			paths = append(paths, relative)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	sort.Strings(paths)
	type pendingFile struct {
		path  string
		bytes []byte
		mode  os.FileMode
	}
	var pending []pendingFile
	next := make(map[string]upstreamConfigurationState, len(requested))
	// Validate every source, range, fingerprint and target before the first write.
	for _, relative := range paths {
		path, err := configurationPath(repository, relative)
		missing := errors.Is(err, os.ErrNotExist)
		if err != nil && !missing {
			return err
		}
		original, err := pinnedConfigurationBytes(ctx, repository, spec.Upstream.Revision, relative)
		if err != nil {
			return err
		}
		originalHash := sourceBytesHash(original)
		file, adapting := requested[relative]
		if adapting && !strings.EqualFold(originalHash, file.SHA256) {
			return fmt.Errorf("upstream.configuration_hash_mismatch: %s", relative)
		}
		prior, configured := previous[relative]
		if configured && !strings.EqualFold(prior.Original, originalHash) {
			return fmt.Errorf("upstream.configuration_original_changed: %s", relative)
		}
		var current []byte
		if !missing {
			current, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		baseline := original
		if configured {
			baseline, err = upstreamConfiguredBaseline(ctx, spec, repository, relative, original, current, prior)
			if err != nil {
				return err
			}
		}
		result := original
		if adapting {
			result, err = sourceconfig.Apply(original, file.Edits)
			if err != nil {
				return fmt.Errorf("upstream.configuration_edit_invalid: %s: %w", relative, err)
			}
			next[relative] = upstreamConfigurationState{Original: originalHash, Result: sourceBytesHash(result), Applied: result}
		}
		if missing {
			// An authenticated authored deletion is not an invitation to recreate
			// the source file. Bindings were still validated against the new blob.
			continue
		}
		result, err = sourceconfig.MergeLocalChanges(ctx, filepath.Dir(repository), baseline, current, result)
		if err != nil {
			return fmt.Errorf("upstream.configuration_preservation: %s: %w", relative, err)
		}
		if !bytes.Equal(current, result) {
			info, err := os.Stat(path)
			if err != nil {
				return err
			}
			pending = append(pending, pendingFile{path: path, bytes: result, mode: info.Mode().Perm()})
		}
	}
	for _, file := range pending {
		if err := replaceConfigurationFile(file.path, file.bytes, file.mode); err != nil {
			return err
		}
	}
	if err := writeUpstreamJSON(statePath, next); err != nil {
		return err
	}
	return recordUpstreamSource(ctx, spec, repository)
}

func replaceConfigurationFile(path string, data []byte, mode os.FileMode) (result error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".lmw-configuration-*")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close(); _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
