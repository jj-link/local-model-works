package backend

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/traces"
)

func exportTestModule(t *testing.T) (*Module, *sql.DB, *db.Queries) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := db.New(sqlDB)
	traceService := traces.New(sqlDB, q, traces.NewRedactor("export-secret"))
	return &Module{env: &moduleapi.Env{DB: sqlDB, Q: q, Traces: traceService}}, sqlDB, q
}

func createTraceFixture(t *testing.T, module *Module, q *db.Queries, outcome string) string {
	t.Helper()
	ctx := context.Background()
	runID, _ := id.New()
	if err := q.CreateRun(ctx, db.CreateRunParams{ID: runID, Module: "coding-traces", Kind: "swe-gym-rollout", State: "running", Resources: "{}", Input: "{}"}); err != nil {
		t.Fatal(err)
	}
	recorder, err := module.env.Traces.Start(ctx, traces.StartInput{RunID: runID, TaskID: "task-1", Problem: "repair the repository", Repository: "owner/repo", BaseRevision: "abc", ModelSource: "lmw_deployment", Model: "model", Sampling: map[string]any{"temperature": 0}})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Append(ctx, traces.Event{Kind: "message", Payload: json.RawMessage(`{"role":"assistant","content":"inspect"}`), InputTokens: 10, OutputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	if outcome == "interrupted" {
		if err := recorder.Interrupt(ctx, "runtime_disconnected"); err != nil {
			t.Fatal(err)
		}
		return recorder.TraceID()
	}
	status := "unresolved"
	if outcome == "success" {
		status = "resolved"
	}
	exit := int64(0)
	if err := recorder.Finalize(ctx, traces.FinalizeInput{FinalDiff: "diff --git a/a b/a", Verification: traces.Verification{Command: "/tmp/eval.sh", TimeoutSeconds: 60, ExitStatus: &exit, Status: status, FailureKind: map[bool]string{true: "", false: "tests_failed"}[outcome == "success"], FailToPassReport: map[string]any{"passed": outcome == "success"}, PassToPassReport: map[string]any{"passed": true}}}); err != nil {
		t.Fatal(err)
	}
	return recorder.TraceID()
}

func runExportFixture(t *testing.T, module *Module, q *db.Queries, workspace string, traceIDs []string, exportID string) string {
	t.Helper()
	ctx := context.Background()
	runID, _ := id.New()
	if err := q.CreateRun(ctx, db.CreateRunParams{ID: runID, Module: "coding-traces", Kind: "trace-export", State: "running", Resources: "{}", Input: "{}"}); err != nil {
		t.Fatal(err)
	}
	selection := map[string]any{"trace_ids": traceIDs, "tokenizer": "cl100k_base", "max_context_tokens": 32768.0, "success_cap_per_task": 2.0, "seed": 7.0}
	selectionJSON, _ := json.Marshal(selection)
	if err := q.CreateCodingTraceExport(ctx, db.CreateCodingTraceExportParams{ID: exportID, RunID: runID, Selection: string(selectionJSON), Seed: 7}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	job := &jobs.Context{RunID: runID, Workspace: workspace, Input: map[string]any{"export_id": exportID, "selection": selection}, PublishArtifact: func(kind, path string) (jobs.PublishedArtifact, error) {
		info, err := os.Stat(filepath.Join(workspace, path))
		return jobs.PublishedArtifact{ID: "artifact", Kind: kind, Path: path, Size: info.Size()}, err
	}}
	out, err := module.runExport(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	artifact := out["artifact"].(jobs.PublishedArtifact)
	return filepath.Join(workspace, artifact.Path)
}

func TestExportDatasetEligibilityBalanceAndDeterminism(t *testing.T) {
	module, _, q := exportTestModule(t)
	success := createTraceFixture(t, module, q, "success")
	unresolved := createTraceFixture(t, module, q, "unresolved")
	interrupted := createTraceFixture(t, module, q, "interrupted")
	ids := []string{success, unresolved, interrupted}
	first := runExportFixture(t, module, q, filepath.Join(t.TempDir(), "one"), ids, "export-one")
	second := runExportFixture(t, module, q, filepath.Join(t.TempDir(), "two"), ids, "export-two")
	firstData, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	secondData, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(firstData) != sha256.Sum256(secondData) {
		t.Fatal("identical selections produced different archives")
	}
	entries := readArchive(t, first)
	if got := lineCount(entries["canonical.jsonl"]); got != 3 {
		t.Fatalf("canonical rows=%d", got)
	}
	if got := lineCount(entries["policy_sft.jsonl"]); got != 1 {
		t.Fatalf("policy rows=%d", got)
	}
	if got := lineCount(entries["verifier.jsonl"]); got != 2 {
		t.Fatalf("verifier rows=%d", got)
	}
	if string(entries["canonical.jsonl"]) == "" || string(entries["manifest.json"]) == "" {
		t.Fatal("required export files are empty")
	}
}

func readArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	out := map[string][]byte{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		out[header.Name] = data
	}
	return out
}
func lineCount(data []byte) int {
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func TestPaperSamplingMatrices(t *testing.T) {
	tests := []struct {
		name, dataset    string
		wantModels       []string
		wantTemperatures []float64
	}{{"paper-d0", "lite", []string{"gpt-4o-2024-05-13"}, []float64{0}}, {"paper-d1", "lite", []string{"gpt-4o-2024-05-13", "gpt-4o-2024-05-13", "gpt-4o-2024-05-13", "gpt-4o-2024-05-13", "gpt-4o-2024-05-13"}, []float64{.2, .3, .4, .5, .8}}, {"paper-d2", "full", []string{"gpt-4o-2024-05-13", "gpt-4o-2024-05-13"}, []float64{0, 1}}, {"paper-d2", "lite", []string{"gpt-4o-2024-05-13", "claude-3-5-sonnet-20241022"}, []float64{0, 0}}}
	for _, test := range tests {
		t.Run(test.name+test.dataset, func(t *testing.T) {
			matrix, err := samplingMatrix(sweGymConfig{Preset: test.name, Dataset: test.dataset, RolloutsPerTask: 1})
			if err != nil {
				t.Fatal(err)
			}
			models := make([]string, len(matrix))
			temperatures := make([]float64, len(matrix))
			for i, item := range matrix {
				models[i] = item.Model
				temperatures[i] = item.Temperature
			}
			if !reflect.DeepEqual(models, test.wantModels) || !reflect.DeepEqual(temperatures, test.wantTemperatures) {
				t.Fatalf("matrix=%+v", matrix)
			}
		})
	}
}

func TestFilterRowsRejectsMissingTask(t *testing.T) {
	rows := []sweGymRow{{InstanceID: "known", Repo: "owner/repo"}}
	_, err := filterRows(rows, []string{"missing"}, nil, 0)
	if err == nil {
		t.Fatal("missing task accepted")
	}
	selected, err := filterRows(rows, []string{"known"}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].InstanceID < selected[j].InstanceID })
}
