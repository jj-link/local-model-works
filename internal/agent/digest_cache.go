package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Keep verification results only in this agent process. A restart requires a
// fresh checksum; on filesystems without a change timestamp, reuse is disabled.
// The bounded index contains metadata and digests, never checkpoint contents.
var fileDigests = struct {
	sync.Mutex
	entries map[string]*fileDigestEntry
}{entries: make(map[string]*fileDigestEntry)}

type fileDigestEntry struct {
	lock   chan struct{}
	info   os.FileInfo
	digest string
}

func sameFileVersion(a, b os.FileInfo) bool {
	if a == nil || b == nil || !os.SameFile(a, b) || a.Size() != b.Size() || a.Mode() != b.Mode() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	ac, aok := fileChangeTime(a)
	bc, bok := fileChangeTime(b)
	return aok && bok && ac == bc
}

func digestFile(ctx context.Context, path string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", 0, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", 0, err
	}
	fileDigests.Lock()
	entry := fileDigests.entries[resolved]
	if entry == nil {
		if len(fileDigests.entries) >= 8192 {
			clear(fileDigests.entries)
		}
		entry = &fileDigestEntry{lock: make(chan struct{}, 1)}
		fileDigests.entries[resolved] = entry
	}
	fileDigests.Unlock()
	select {
	case entry.lock <- struct{}{}:
		defer func() { <-entry.lock }()
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("checksum source is not a regular file: %s", path)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if sameFileVersion(entry.info, info) {
		return entry.digest, info.Size(), nil
	}
	entry.info = nil
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return "", 0, fmt.Errorf("checksum source changed before reading: %s", path)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader{ctx: ctx, reader: file})
	if err != nil {
		return "", 0, err
	}
	after, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	current, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	// Even without reusable change metadata, reject observable changes during
	// the read. Never publish a checksum for a replacement or a partial write.
	if size != before.Size() || !os.SameFile(before, current) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", 0, fmt.Errorf("checksum source changed while reading: %s", path)
	}
	if _, supported := fileChangeTime(before); supported && (!sameFileVersion(before, after) || !sameFileVersion(after, current)) {
		return "", 0, fmt.Errorf("checksum source changed while reading: %s", path)
	}
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	entry.info, entry.digest = after, digest
	return digest, size, nil
}
