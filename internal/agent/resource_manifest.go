package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// HF resources contain one pinned snapshot, its exact immutable blobs and its
// verification record, never unrelated revisions or another attempt's staging.
func collectResourceManifest(ctx context.Context, identity, root string) ([]*agentv1.FileEntry, uint64, string, error) {
	if !strings.HasPrefix(identity, "hf://") {
		return collectManifest(ctx, root, root)
	}
	_, revision, ok := strings.Cut(identity, "@")
	if !ok {
		return nil, 0, "", fmt.Errorf("download.identity_invalid")
	}
	recordPath := hfSnapshotManifestPath(root, revision)
	raw, err := os.ReadFile(recordPath)
	if err != nil || len(raw) > 16<<20 {
		return nil, 0, "", fmt.Errorf("download.snapshot_record_unavailable")
	}
	var record hfSnapshotManifest
	if json.Unmarshal(raw, &record) != nil || record.Version != 2 || record.Identity != identity {
		return nil, 0, "", fmt.Errorf("download.snapshot_record_invalid")
	}
	entries := map[string]*agentv1.FileEntry{}
	addFile := func(path string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := safeDestination(path); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !pathWithin(path, root) {
			return fmt.Errorf("download.snapshot_escape")
		}
		rel = filepath.ToSlash(rel)
		if entries[rel] != nil {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("download.snapshot_file_missing")
		}
		digest, size, err := digestFile(ctx, path)
		if err != nil {
			return err
		}
		entries[rel] = &agentv1.FileEntry{Path: rel, Size: uint64(size), Mode: uint32(info.Mode().Perm()), Sha256: digest}
		return nil
	}
	if err := addFile(recordPath); err != nil {
		return nil, 0, "", err
	}
	for _, file := range record.Files {
		rel, err := safeRelativePath(file.Path)
		if err != nil {
			return nil, 0, "", err
		}
		path := filepath.Join(root, "snapshots", revision, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, 0, "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			if err := addFile(path); err != nil {
				return nil, 0, "", err
			}
			continue
		}
		target, err := os.Readlink(path)
		if err != nil || filepath.IsAbs(target) {
			return nil, 0, "", fmt.Errorf("download.snapshot_link_invalid")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || (!pathWithin(resolved, filepath.Join(root, "blobs")) && !pathWithin(resolved, filepath.Join(root, "snapshots", revision))) {
			return nil, 0, "", fmt.Errorf("download.snapshot_link_escape")
		}
		if err := addFile(resolved); err != nil {
			return nil, 0, "", err
		}
		relative, _ := filepath.Rel(root, path)
		entries[filepath.ToSlash(relative)] = &agentv1.FileEntry{Path: filepath.ToSlash(relative), Mode: uint32(os.ModeSymlink), SymlinkTarget: filepath.ToSlash(target)}
	}
	sorted := make([]*agentv1.FileEntry, 0, len(entries))
	for _, entry := range entries {
		sorted = append(sorted, entry)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	hash := sha256.New()
	var total uint64
	for _, entry := range sorted {
		total += entry.Size
		fmt.Fprintf(hash, "%s\x00%d\x00%s\x00%s\n", entry.Path, entry.Size, entry.Sha256, entry.SymlinkTarget)
	}
	return sorted, total, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
