// Package hf validates Hugging Face snapshot directories using the legacy
// validate-hf-snapshot.py behavior as the oracle: symlinks must resolve
// inside the cache root, no file may be an unexpanded git-lfs pointer, and
// every shard named by a safetensors index must exist and stay in-snapshot.
package hf

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Diagnostic mirrors recipe.Diagnostic without an import cycle.
type Diagnostic struct {
	Code     string
	Severity string
	Message  string
	Path     string
}

func diag(code, msg, p string) Diagnostic {
	return Diagnostic{Code: code, Severity: "error", Message: msg, Path: p}
}

const lfsHeader = "version https://git-lfs.github.com"

// ValidateSnapshot walks the snapshot directory and returns diagnostics.
// cacheRoot is the largest root a symlink may escape to (the HF hub root or
// an explicit cache root); the final canonical target of every symlink must
// stay within it.
func ValidateSnapshot(snapshotDir, cacheRoot string) []Diagnostic {
	var diags []Diagnostic
	snap, err := filepath.Abs(snapshotDir)
	if err != nil {
		return []Diagnostic{diag("hf.snapshot-missing", "snapshot dir: "+err.Error(), snapshotDir)}
	}
	snap, err = filepath.EvalSymlinks(snap)
	if err != nil {
		return []Diagnostic{diag("hf.snapshot-missing", "snapshot dir missing or not a directory: "+err.Error(), snapshotDir)}
	}
	root := snapshotDir
	if cacheRoot != "" {
		if r, err := filepath.EvalSymlinks(cacheRoot); err == nil {
			root = r
		}
	}

	err = filepath.WalkDir(snap, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			diags = append(diags, diag("hf.walk", err.Error(), full))
			return nil
		}
		rel, _ := filepath.Rel(snap, full)
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				diags = append(diags, diag("hf.symlink-unreadable", err.Error(), rel))
				return nil
			}
			var resolved string
			if strings.HasPrefix(target, "/") {
				resolved = target
			} else {
				resolved = filepath.Join(filepath.Dir(full), target)
			}
			canonical, err := filepath.EvalSymlinks(resolved)
			if err != nil {
				diags = append(diags, diag("hf.symlink-dangling", fmt.Sprintf("symlink %s -> %s does not resolve (%v)", rel, target, err), rel))
				return nil
			}
			if !within(canonical, root) && !within(canonical, snap) {
				diags = append(diags, diag("hf.symlink-escape", fmt.Sprintf("symlink %s -> %s escapes cache root (canonical %s)", rel, target, canonical), rel))
				return nil
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			diags = append(diags, diag("hf.stat", err.Error(), rel))
			return nil
		}
		if isLFSPointer(full) {
			diags = append(diags, diag("hf.lfs-pointer", fmt.Sprintf("%s is an unexpanded git-lfs pointer", rel), rel))
			return nil
		}
		_ = info
		return nil
	})
	if err != nil {
		return append(diags, diag("hf.walk", err.Error(), snapshotDir))
	}

	if idx, err := findSafetensorsIndex(snap); err == nil {
		diags = append(diags, validateSafetensorsIndex(idx, snap)...)
	}
	return diags
}

func findSafetensorsIndex(snap string) (string, error) {
	for _, name := range []string{"model.safetensors.index.json", "diffusion_pytorch_model.safetensors.index.json"} {
		p := filepath.Join(snap, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

func validateSafetensorsIndex(indexPath, snapRoot string) []Diagnostic {
	var diags []Diagnostic
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		return []Diagnostic{diag("hf.index-unreadable", err.Error(), indexPath)}
	}
	var idx struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return []Diagnostic{diag("hf.index-invalid", "not a valid safetensors index: "+err.Error(), indexPath)}
	}
	for tensor, shard := range idx.WeightMap {
		if filepath.IsAbs(shard) || hasDotDotSegment(shard) {
			diags = append(diags, diag("hf.index-shard-escape", fmt.Sprintf("index contains unsafe weight file reference %s (tensor %s)", shard, tensor), shard))
			continue
		}
		full := filepath.Join(snapRoot, filepath.Clean(shard))
		st, err := os.Stat(full)
		if err != nil {
			diags = append(diags, diag("hf.index-shard-missing", fmt.Sprintf("index references missing shard %s (tensor %s)", shard, tensor), shard))
			continue
		}
		if st.IsDir() {
			diags = append(diags, diag("hf.index-shard-missing", fmt.Sprintf("index references directory %s (tensor %s)", shard, tensor), shard))
		}
	}
	return diags
}

// hasDotDotSegment reports whether a reference path escapes its base
// directory lexically (matches the legacy snapshot validator's
// "unsafe weight file reference" check; symlink targets are validated
// separately by the snapshot walk).
func hasDotDotSegment(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func isLFSPointer(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(lfsHeader)+4)
	n, _ := f.Read(buf)
	return n >= len(lfsHeader) && strings.HasPrefix(string(buf[:len(lfsHeader)]), lfsHeader)
}

// within reports whether p is inside or equal to root (both canonical).
func within(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
