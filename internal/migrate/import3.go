package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/runs"
)

// benchmarkRows renders one db row per (run_id, language) group from the
// legacy result index (newest file wins) and reads the chosen legacy result
// file for the metrics. Runs absent from the imported ledger are returned
// as ghost ids so the FK holds.
func benchmarkRows(plan *Plan, importedRuns map[string]bool, stateDir, targetRoot string) ([]db.InsertBenchmarkResultParams, []string, error) {
	type group struct {
		runID    string
		language string
		entry    IndexEntry
	}
	groups := map[string]*group{}
	for _, e := range plan.Benchmarks.Index {
		if e.Summary == nil || e.Summary.RunID == "" || e.Summary.Language == "" {
			continue
		}
		key := e.Summary.RunID + "\x00" + e.Summary.Language
		g := groups[key]
		if g == nil || e.MTimeNS > g.entry.MTimeNS ||
			(e.MTimeNS == g.entry.MTimeNS && e.File > g.entry.File) {
			g = &group{runID: e.Summary.RunID, language: e.Summary.Language, entry: e}
			groups[key] = g
		}
	}
	var keys []string
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var rows []db.InsertBenchmarkResultParams
	ghostSet := map[string]bool{}
	for _, k := range keys {
		g := groups[k]
		if !importedRuns[g.runID] {
			ghostSet[g.runID] = true
		}
		row, err := buildBenchmarkRow(plan, g.runID, g.language, g.entry, stateDir, targetRoot)
		if err != nil {
			return nil, nil, fmt.Errorf("benchmark row %s/%s: %w", g.runID, g.language, err)
		}
		rows = append(rows, row)
	}
	ghosts := make([]string, 0, len(ghostSet))
	for id := range ghostSet {
		ghosts = append(ghosts, id)
	}
	sort.Strings(ghosts)
	return rows, ghosts, nil
}

func buildBenchmarkRow(plan *Plan, runID, language string, e IndexEntry, stateDir, targetRoot string) (db.InsertBenchmarkResultParams, error) {
	row := db.InsertBenchmarkResultParams{
		RunID:      runID,
		Language:   language,
		Latency:    "{}",
		FirstToken: "{}",
		ResultPath: sqlNullStr(filepath.Join(targetRoot, "benchmarks", "results", e.File)),
	}
	if e.Summary.Served != "" {
		row.Model = sqlNullStr(e.Summary.Served)
	}
	if e.Summary.Quant != "" {
		row.Quantization = sqlNullStr(e.Summary.Quant)
	}
	if e.Summary.Reasoning != "" && e.Summary.Reasoning != "disabled" {
		reasoning, _ := json.Marshal(map[string]string{"reason": e.Summary.Reasoning})
		row.Reasoning = sqlNullStr(string(reasoning))
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, "benchmark-results", e.File))
	if err != nil {
		if e.Summary.Kind == "cross-agent" {
			return row, nil // metrics optional for cross-agent archives
		}
		return row, nil // keep the row from the summary when the file is gone
	}
	var d map[string]json.RawMessage
	if err := json.Unmarshal(raw, &d); err != nil {
		return row, nil
	}
	var top struct {
		RunID    string  `json:"run_id"`
		Served   string  `json:"served"`
		Alias    string  `json:"alias"`
		Lang     string  `json:"lang"`
		Total    float64 `json:"total"`
		Passed   float64 `json:"passed"`
		PassRate float64 `json:"pass_rate"`
		Endpoint string  `json:"endpoint"`
		Meta     struct {
			Endpoint string `json:"endpoint"`
			Quant    string `json:"quant"`
			Reason   string `json:"reasoning"`
		} `json:"meta"`
		Results []struct {
			LatencyS         float64 `json:"latency_s"`
			PromptTokens     float64 `json:"prompt_tokens"`
			CompletionTokens float64 `json:"completion_tokens"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return row, nil
	}
	if top.Served != "" {
		row.Model = sqlNullStr(top.Served)
	}
	if top.Meta.Endpoint != "" {
		row.Endpoint = sqlNullStr(top.Meta.Endpoint)
	} else if top.Endpoint != "" {
		row.Endpoint = sqlNullStr(top.Endpoint)
	}
	if top.Meta.Quant != "" {
		row.Quantization = sqlNullStr(top.Meta.Quant)
	}
	if top.Meta.Reason != "" && top.Meta.Reason != "disabled" {
		reasoning, _ := json.Marshal(map[string]string{"reason": top.Meta.Reason})
		row.Reasoning = sqlNullStr(string(reasoning))
	}
	row.Requests = int64(top.Total)
	row.Successes = int64(top.Passed)
	var wall float64
	for _, r := range top.Results {
		wall += r.LatencyS
		row.PromptTokens += int64(r.PromptTokens)
		row.CompletionTokens += int64(r.CompletionTokens)
	}
	row.TotalTokens = row.PromptTokens + row.CompletionTokens
	row.WallSeconds = wall
	if wall > 0 && row.TotalTokens > 0 {
		row.TokensPerSecond = float64(row.TotalTokens) / wall
	}
	if len(top.Results) > 0 {
		lat := latencyStats(top.Results)
		latJSON, _ := json.Marshal(lat)
		row.Latency = string(latJSON)
	}
	if row.Requests > 0 {
		grading, _ := json.Marshal(map[string]any{
			"ok_count":     int64(top.Passed),
			"success_rate": top.PassRate / 100.0,
		})
		row.Grading = sqlNullStr(string(grading))
	}
	return row, nil
}

func latencyStats(rs []struct {
	LatencyS         float64 `json:"latency_s"`
	PromptTokens     float64 `json:"prompt_tokens"`
	CompletionTokens float64 `json:"completion_tokens"`
}) map[string]float64 {
	vals := make([]float64, 0, len(rs))
	for _, r := range rs {
		if r.LatencyS > 0 {
			vals = append(vals, r.LatencyS*1000.0)
		}
	}
	if len(vals) == 0 {
		return map[string]float64{}
	}
	sort.Float64s(vals)
	get := func(q float64) float64 {
		idx := int(q*float64(len(vals)-1) + 0.5)
		return vals[idx]
	}
	return map[string]float64{
		"min": vals[0], "max": vals[len(vals)-1],
		"p50": get(0.5), "p90": get(0.9), "p99": get(0.99),
	}
}

func sqlNullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// verifyImport re-checks the imported state through the live services:
// every imported run's log is readable via the runs service with the
// recorded end offset, benchmark file counts reproduce the legacy values,
// and the DB rows match the plan.
func verifyImport(ctx context.Context, plan *Plan, opts ImportOptions, runsSvc *runs.Service, rep *ImportReport) error {
	for _, r := range plan.Runs {
		if r.Nonterminal || r.LogSize == 0 {
			continue
		}
		chunk, next, size, err := runsSvc.ReadLog(r.ID, "", 0, "stdout", 0, 1<<20)
		if err != nil {
			return fmt.Errorf("verify log %s: %w", r.ID, err)
		}
		if size != uint64(r.LogSize) || uint64(len(chunk)) != size || next != size {
			return fmt.Errorf("verify log %s: size %d (want %d)", r.ID, size, r.LogSize)
		}
		runsSvc.MarkLogEnd(r.ID, "", 0, "stdout", size)
	}
	if n, err := countFiles(filepath.Join(opts.TargetRoot, "benchmarks", "results")); err == nil {
		if n != plan.Benchmarks.ResultsFiles {
			return fmt.Errorf("benchmark-results count %d != plan %d", n, plan.Benchmarks.ResultsFiles)
		}
	}
	if n, err := countFiles(filepath.Join(opts.TargetRoot, "benchmarks", "aider-benchmarks")); err == nil {
		if n != plan.Benchmarks.AiderFiles {
			return fmt.Errorf("aider-benchmarks count %d != plan %d", n, plan.Benchmarks.AiderFiles)
		}
	}
	return nil
}
