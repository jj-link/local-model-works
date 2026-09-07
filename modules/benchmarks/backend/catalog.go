package backend

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/runtime"
)

const (
	terminalBenchDatasetLocator = "terminal-bench/terminal-bench@3.0.0"
	terminalBenchReferenceURL   = "https://www.tbench.ai/news/terminal-bench-3-0"
	terminalBenchSourceChecksum = "2b0442c3c583b710ca8da14c8e601b99f2f1f244"
	defaultBenchmarkConcurrency = 4
)

//go:embed terminal_bench_3_tasks.json
var terminalBenchTaskManifest []byte

var terminalBenchTaskIDs = mustTerminalBenchTaskIDs()

func mustTerminalBenchTaskIDs() []string {
	var taskIDs []string
	if err := json.Unmarshal(terminalBenchTaskManifest, &taskIDs); err != nil {
		panic(err)
	}
	if len(taskIDs) != 74 || !slices.IsSorted(taskIDs) {
		panic("terminal-bench 3.0.0 manifest must contain 74 sorted task IDs")
	}
	for index := 1; index < len(taskIDs); index++ {
		if taskIDs[index] == taskIDs[index-1] {
			panic("terminal-bench 3.0.0 manifest contains duplicate task IDs")
		}
	}
	return taskIDs
}

type benchmarkCatalogHarness struct {
	ID                           string `json:"id"`
	Title                        string `json:"title"`
	RequiresGenerationDeployment bool   `json:"requires_generation_deployment"`
	AcceptsOpenAICompatible      bool   `json:"accepts_openai_compatible"`
}

type benchmarkCatalogEntry struct {
	BenchmarkID                string                    `json:"benchmark_id"`
	Version                    string                    `json:"version"`
	Title                      string                    `json:"title"`
	Harnesses                  []benchmarkCatalogHarness `json:"harnesses"`
	DatasetLocator             string                    `json:"dataset_locator,omitempty"`
	SourceChecksum             string                    `json:"source_checksum,omitempty"`
	TaskCount                  int                       `json:"task_count"`
	ReferenceURL               string                    `json:"reference_url,omitempty"`
	Languages                  []string                  `json:"languages,omitempty"`
	TaskIDs                    []string                  `json:"task_ids,omitempty"`
	SupportsTaskFilters        bool                      `json:"supports_task_filters"`
	SupportsRepeatedCandidates bool                      `json:"supports_repeated_candidates"`
	SupportsVerifier           bool                      `json:"supports_verifier"`
}

type benchmarkRunnerAvailability struct {
	Configured     bool   `json:"configured"`
	Online         bool   `json:"online"`
	NodeID         string `json:"node_id,omitempty"`
	MaxConcurrency int    `json:"max_concurrency"`
}

type benchmarkCatalogResponse struct {
	Benchmarks []benchmarkCatalogEntry     `json:"benchmarks"`
	Runner     benchmarkRunnerAvailability `json:"runner"`
}

func staticBenchmarkCatalog() []benchmarkCatalogEntry {
	return []benchmarkCatalogEntry{
		{
			BenchmarkID: "lmw-code-generation",
			Version:     "1",
			Title:       "LMW code generation",
			Harnesses: []benchmarkCatalogHarness{{
				ID: "lmw-oneshot", Title: "LMW one-shot grader", RequiresGenerationDeployment: true, AcceptsOpenAICompatible: true,
			}},
			TaskCount: len(supportedLanguages),
			Languages: append([]string(nil), supportedLanguages...),
		},
		{
			BenchmarkID:    "terminal-bench",
			Version:        "3.0.0",
			Title:          "Terminal-Bench 3.0",
			DatasetLocator: terminalBenchDatasetLocator,
			SourceChecksum: terminalBenchSourceChecksum,
			TaskCount:      len(terminalBenchTaskIDs),
			ReferenceURL:   terminalBenchReferenceURL,
			Harnesses: []benchmarkCatalogHarness{
				{ID: "oracle", Title: "Oracle", RequiresGenerationDeployment: false, AcceptsOpenAICompatible: false},
				{ID: "codex", Title: "Codex", RequiresGenerationDeployment: true, AcceptsOpenAICompatible: true},
				{ID: "mini-swe-agent", Title: "mini-SWE-agent", RequiresGenerationDeployment: true, AcceptsOpenAICompatible: true},
			},
			TaskIDs:                    append([]string(nil), terminalBenchTaskIDs...),
			SupportsTaskFilters:        true,
			SupportsRepeatedCandidates: true,
			SupportsVerifier:           true,
		},
	}
}

func (m *Module) benchmarkRunnerAvailability(ctx context.Context) (benchmarkRunnerAvailability, error) {
	availability := benchmarkRunnerAvailability{MaxConcurrency: defaultBenchmarkConcurrency}
	settings, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return availability, err
	}
	imageConfigured := runnerImageConfigured(settings)
	if raw, ok := settings["max_concurrency"].(float64); ok && raw >= 1 && raw <= 32 {
		availability.MaxConcurrency = int(raw)
	}
	nodeID, resolveErr := nodes.ApprovedLocalRunnerNodeID(ctx, m.env.Q, m.env.Nodes)
	if resolveErr == nil {
		availability.NodeID = nodeID
		availability.Online = m.env.Nodes != nil && m.env.Nodes.Online(nodeID)
	}
	availability.Configured = imageConfigured && resolveErr == nil
	return availability, nil
}

// runnerImageConfigured reports whether the module settings carry a
// content-addressed runner image: a digest-pinned registry reference or a
// bare image ID for an already-loaded local image.
func runnerImageConfigured(values map[string]any) bool {
	raw, ok := values["harbor_runner_image"].(string)
	return ok && runtime.IsContentAddressedImage(strings.TrimSpace(raw))
}

func (m *Module) catalog(w http.ResponseWriter, r *http.Request) {
	runner, err := m.benchmarkRunnerAvailability(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, benchmarkCatalogResponse{Benchmarks: staticBenchmarkCatalog(), Runner: runner})
}
