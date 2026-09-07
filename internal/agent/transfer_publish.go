package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// publishHFTransfer adds immutable files to an existing model cache without
// replacing any blob or snapshot that may be mounted. Completion is last.
func publishHFTransfer(ctx context.Context, staging, final string, entries []*agentv1.FileEntry) error {
	if err := safeDestination(final); err != nil {
		return err
	}
	var completion *agentv1.FileEntry
	publish := func(entry *agentv1.FileEntry) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := safeRelativePath(entry.Path)
		if err != nil {
			return err
		}
		source, destination := filepath.Join(staging, rel), filepath.Join(final, rel)
		if err := safeDestination(filepath.Dir(destination)); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
			return err
		}
		if info, err := os.Lstat(destination); err == nil {
			if entry.SymlinkTarget != "" {
				target, err := os.Readlink(destination)
				if err != nil || target != filepath.FromSlash(entry.SymlinkTarget) {
					return fmt.Errorf("download.active_file_invalid")
				}
			} else if !info.Mode().IsRegular() || uint64(info.Size()) != entry.Size || !matchesFileDigest(ctx, destination, entry.Sha256) {
				return fmt.Errorf("download.active_file_invalid")
			}
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if entry.SymlinkTarget != "" {
			return os.Symlink(filepath.FromSlash(entry.SymlinkTarget), destination)
		}
		// Staging shares the destination filesystem. Hard-linking verified immutable
		// bytes atomically avoids a second full copy and cannot overwrite a racer.
		return os.Link(source, destination)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Path, ".lmw/snapshots/") {
			completion = entry
			continue
		}
		if err := publish(entry); err != nil {
			return err
		}
	}
	if completion == nil {
		return fmt.Errorf("download.snapshot_record_missing")
	}
	if err := publish(completion); err != nil {
		return err
	}
	return os.RemoveAll(staging)
}
