package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"

	"github.com/jj-link/local-model-works/internal/cjson"
)

// hashLimitBytes: files at or below this size carry a content digest in
// the source manifest; larger ones rely on size + mtime.
const hashLimitBytes = 256 << 10

// FileEntry is one scanned source file.
type FileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	MTimeNS int64  `json:"mtime_ns"`
	SHA256  string `json:"sha256,omitempty"`
}

// SourceManifest is the cheap pre/post proof that the import left every
// source file untouched: per-file count + size + mtime, plus content
// digests for small files.
type SourceManifest struct {
	Entries []FileEntry
	Digest  string
}

// ManifestRoot is one scanned tree: an absolute root plus its label.
type ManifestRoot struct {
	Label string
	Root  string
}

// ScanInputs returns the source trees a migration scan/import reads.
func ScanInputs(opts ScanOptions) []ManifestRoot {
	roots := []ManifestRoot{
		{"legacy/control", opts.LegacyDir + "/control"},
		{"legacy/dgx_dashboard", opts.LegacyDir + "/dgx_dashboard"},
		{"state/runs", opts.StateDir + "/runs"},
		{"state/benchmark-results", opts.StateDir + "/benchmark-results"},
		{"state/aider-benchmarks", opts.StateDir + "/aider-benchmarks"},
	}
	if opts.StateDir+"/result-index.json" != "" {
		roots = append(roots, ManifestRoot{"state/result-index.json", opts.StateDir + "/result-index.json"})
	}
	if opts.INIPath != "" {
		roots = append(roots, ManifestRoot{"state/config.ini", opts.INIPath})
	}
	return roots
}

// ManifestTree snapshots every file under the given roots (read-only).
func ManifestTree(roots []ManifestRoot) (*SourceManifest, error) {
	m := &SourceManifest{}
	for _, r := range roots {
		st, err := os.Stat(r.Root)
		if err != nil {
			if os.IsNotExist(err) {
				continue // absent optional input
			}
			return nil, fmt.Errorf("manifest: %w", err)
		}
		if !st.IsDir() {
			m.Entries = append(m.Entries, manifestFile(r.Label, r.Root, r.Root))
			continue
		}
		err = filepath.WalkDir(r.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(r.Root, p)
			if rerr != nil {
				return nil
			}
			m.Entries = append(m.Entries, manifestFile(r.Label, filepath.Join(r.Root, rel), p))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("manifest walk: %w", err)
		}
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	b, err := cjson.Marshal(m.Entries)
	if err != nil {
		return nil, fmt.Errorf("manifest digest: %w", err)
	}
	sum := sha256.Sum256(b)
	m.Digest = "sha256:" + hex.EncodeToString(sum[:])
	return m, nil
}

func manifestFile(label, path, full string) FileEntry {
	e := FileEntry{Path: label + "/" + path}
	st, err := os.Lstat(full)
	if err != nil {
		return e
	}
	e.Size = st.Size()
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		e.MTimeNS = sys.Mtim.Nano()
	}
	if e.Size > 0 && e.Size <= hashLimitBytes {
		if raw, rerr := os.ReadFile(full); rerr == nil {
			e.SHA256 = sha256Hex(raw)
		}
	}
	return e
}
