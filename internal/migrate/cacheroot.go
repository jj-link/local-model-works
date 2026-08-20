package migrate

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/hf"
)

// legacyNodeIDs are the nodes the migration seeds placements for.
var legacyNodeIDs = []string{"local", "spark1", "spark2", "spark3"}

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// INICacheRoots extracts [cache] node=path bindings from the production INI
// when the section is present (the current production INI has none).
func INICacheRoots(iniPath string) (map[string][]string, error) {
	raw, err := os.ReadFile(iniPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read INI: %w", err)
	}
	out := map[string][]string{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.Trim(line, "[]")
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section != "cache" {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = append(out[k], v)
	}
	return out, nil
}

// defaultCacheRoots are the legacy run-single.sh defaults per node:
// rtx6000 (local) uses $HOME/.cache/huggingface, spark uses $HOME/models.
func defaultCacheRoots() map[string][]string {
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	return map[string][]string{
		"local":  {filepath.Join(home, ".cache", "huggingface")},
		"spark1": {filepath.Join(home, "models")},
		"spark2": {filepath.Join(home, "models")},
		"spark3": {filepath.Join(home, "models")},
	}
}

// resolveCacheRoots merges explicit flags, INI [cache] entries, and the
// legacy per-node defaults (explicit wins; duplicates drop).
func resolveCacheRoots(opts ScanOptions) map[string][]string {
	merged := map[string][]string{}
	add := func(node, path string) {
		seen := false
		for _, p := range merged[node] {
			if p == path {
				seen = true
				break
			}
		}
		if !seen {
			merged[node] = append(merged[node], path)
		}
	}
	defs := defaultCacheRoots()
	for _, n := range legacyNodeIDs {
		for _, p := range defs[n] {
			add(n, p)
		}
	}
	if opts.INIPath != "" {
		if ini, err := INICacheRoots(opts.INIPath); err == nil {
			for _, n := range legacyNodeIDs {
				for _, p := range ini[n] {
					add(n, p)
				}
			}
		}
	}
	for _, spec := range opts.CacheRoots {
		if spec.Node == "" || spec.Path == "" {
			continue
		}
		add(spec.Node, spec.Path)
	}
	return merged
}

// ScanCacheRoots resolves and lists every node's cache roots. Read-only.
func ScanCacheRoots(opts ScanOptions) []PlanNode {
	merged := resolveCacheRoots(opts)
	out := make([]PlanNode, 0, len(legacyNodeIDs))
	for _, node := range legacyNodeIDs {
		pn := PlanNode{ID: node}
		for _, path := range merged[node] {
			pn.CacheRoots = append(pn.CacheRoots, scanOneCacheRoot(node, path))
		}
		out = append(out, pn)
	}
	return out
}

func scanOneCacheRoot(node, root string) CacheRoot {
	cr := CacheRoot{Path: root, Backend: detectBackend(root)}
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		cr.Placements = []Placement{}
		return cr
	}
	cr.Exists = true

	hub := root
	entries, err := os.ReadDir(root)
	if err == nil {
		var hasModels bool
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "models--") {
				hasModels = true
				break
			}
		}
		if !hasModels {
			if sub, err := os.Stat(filepath.Join(root, "hub")); err == nil && sub.IsDir() {
				hub = filepath.Join(root, "hub")
			}
		}
	}
	cr.Repositories = listRepositories(hub)
	cr.Placements = scanPlacements(node, root, hub)
	return cr
}

// detectBackend mirrors the agent's cache-root classification.
func detectBackend(root string) string {
	clean := filepath.Clean(root)
	base := strings.ToLower(filepath.Base(clean))
	if base == "huggingface" {
		return "huggingface"
	}
	lower := strings.ToLower(clean)
	if strings.HasSuffix(lower, "/models/hub") || strings.HasSuffix(lower, "/.cache/huggingface") {
		return "huggingface"
	}
	return "local"
}

// listRepositories mirrors the agent's repository listing: two levels for
// hub layouts, one for plain local roots.
func listRepositories(hub string) []string {
	entries, err := os.ReadDir(hub)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), "models--") {
			rest := strings.TrimPrefix(e.Name(), "models--")
			if org, repo, ok := strings.Cut(rest, "--"); ok && org != "" && repo != "" {
				repos = append(repos, org+"/"+repo)
				continue
			}
		}
		sub := filepath.Join(hub, e.Name())
		subEntries, err := os.ReadDir(sub)
		if err == nil && len(subEntries) > 0 {
			for _, se := range subEntries {
				if se.IsDir() {
					repos = append(repos, e.Name()+"/"+se.Name())
				}
			}
		} else {
			repos = append(repos, e.Name())
		}
	}
	sort.Strings(repos)
	return repos
}

// scanPlacements lists the reportable model trees under one hub: every
// concrete HF snapshot (validated with the product's hf.ValidateSnapshot,
// the Go port of the legacy validate-hf-snapshot.py oracle) and, for plain
// local roots, each direct subdirectory.
func scanPlacements(node, root, hub string) []Placement {
	entries, err := os.ReadDir(hub)
	if err != nil {
		return []Placement{}
	}
	out := []Placement{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(hub, e.Name())
		if strings.HasPrefix(e.Name(), "models--") {
			_ = root
			rest := strings.TrimPrefix(e.Name(), "models--")
			org, repo, ok := strings.Cut(rest, "--")
			if !ok || org == "" || repo == "" {
				continue
			}
			snapRoot := filepath.Join(full, "snapshots")
			snaps, err := os.ReadDir(snapRoot)
			if err != nil {
				continue
			}
			for _, s := range snaps {
				if !s.IsDir() || !sha40Re.MatchString(s.Name()) {
					continue
				}
				snap := filepath.Join(snapRoot, s.Name())
				diags := hf.ValidateSnapshot(snap, full)
				size := dirSize(snap, full)
				p := Placement{
					Node:      node,
					Identity:  "huggingface://" + org + "/" + repo,
					Path:      snap,
					Revision:  s.Name(),
					SizeBytes: size,
				}
				if len(diags) > 0 {
					p.State = "failed"
					for _, d := range diags {
						p.Diagnostics = append(p.Diagnostics, d.Code+": "+d.Message)
					}
				} else {
					p.State = "verified"
				}
				out = append(out, p)
			}
			continue
		}
		// Plain local root: each direct subdirectory is one placement.
		size := dirSize(full, full)
		out = append(out, Placement{
			Node:      node,
			Identity:  "local" + "://" + e.Name(),
			Path:      full,
			SizeBytes: size,
			State:     "verified",
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Identity != out[j].Identity {
			return out[i].Identity < out[j].Identity
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// dirSize totals regular files under root, dereferencing symlinks that
// stay inside contain (HF snapshots symlink into sibling blobs/).
func dirSize(root, contain string) int64 {
	contain = filepath.Clean(contain)
	var size int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		target := p
		if d.Type()&fs.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil {
				return nil
			}
			target = resolved
		}
		rl := filepath.Clean(target)
		if rl != contain && !strings.HasPrefix(rl, contain+string(filepath.Separator)) {
			return nil
		}
		if fi, ierr := os.Stat(rl); ierr == nil && fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return size
}
