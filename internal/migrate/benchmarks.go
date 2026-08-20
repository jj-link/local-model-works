package migrate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ScanBenchmarks inventories the legacy benchmark state: the result index
// (entries the importer must reproduce as benchmark_results rows) and the
// raw file trees whose counts the importer must reproduce exactly.
func ScanBenchmarks(stateDir string) (BenchmarkPlan, error) {
	var bp BenchmarkPlan
	bp.ResultsDir = filepath.Join(stateDir, "benchmark-results")
	bp.AiderDir = filepath.Join(stateDir, "aider-benchmarks")

	if n, err := countFiles(bp.ResultsDir); err == nil {
		bp.ResultsFiles = n
	}
	if n, err := countFiles(bp.AiderDir); err == nil {
		bp.AiderFiles = n
	}

	indexPath := filepath.Join(stateDir, "result-index.json")
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return bp, nil
		}
		return bp, fmt.Errorf("read result-index.json: %w", err)
	}
	var idx struct {
		Version int                            `json:"version"`
		Files   map[string]legacyIndexEntryRaw `json:"files"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		return bp, fmt.Errorf("parse result-index.json: %w", err)
	}
	bp.IndexVersion = idx.Version
	bp.IndexEntries = len(idx.Files)
	for name, meta := range idx.Files {
		e := IndexEntry{File: name, Size: meta.Size, MTimeNS: meta.MTimeNS}
		if meta.Summary != nil {
			e.Summary = &IndexSummary{
				RunID:     meta.Summary.RunID,
				Language:  meta.Summary.Language,
				Served:    meta.Summary.Served,
				Kind:      meta.Summary.Kind,
				Quant:     meta.Summary.Quant,
				Reasoning: meta.Summary.Reasoning,
			}
		}
		bp.Index = append(bp.Index, e)
	}
	return bp, nil
}

type legacyIndexEntryRaw struct {
	Size    int64               `json:"size"`
	MTimeNS int64               `json:"mtime_ns"`
	Summary *legacyIndexSummary `json:"summary"`
}

type legacyIndexSummary struct {
	RunID     string `json:"run_id"`
	Language  string `json:"language"`
	Served    string `json:"served"`
	Kind      string `json:"kind"`
	Quant     string `json:"quant"`
	Reasoning string `json:"reasoning"`
}

// countFiles counts regular files under root at any depth. A missing root
// counts as zero with an error (absent benchmark state is legal).
func countFiles(root string) (int, error) {
	st, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	if !st.IsDir() {
		return 0, fmt.Errorf("not a directory: %s", root)
	}
	n := 0
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n, err
}
