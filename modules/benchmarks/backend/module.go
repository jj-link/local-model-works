// Package backend implements the benchmarks first-party module: the
// operator's launch point for model throughput benchmarks against a
// running deployment. A benchmark run executes one digest-pinned grader
// container per language, sequentially, on the deployment's rank-0 node
// through the agent workload protocol, and records one benchmark_results
// row per language.
package backend

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the benchmarks backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// RegisterJobs declares the benchmark job kind: one digest-pinned grader
// container per language against the deployment's rank-0 endpoint.
func (m *Module) RegisterJobs(reg *jobs.Registry) {
	if err := reg.Register("benchmarks", jobs.Spec{
		Kind:         "benchmark",
		Title:        "Benchmark",
		InputSchema:  inputSchema,
		OutputSchema: outputSchema,
		Executor:     m.runBenchmark,
	}); err != nil {
		panic(err) // wiring error: duplicate kind or malformed schema
	}
}

// RegisterSettings declares the module's operator settings (validated
// against the manifest's settingsSchema).
func (m *Module) RegisterSettings(reg *settings.Registry) {
	if err := reg.Register(descriptor.ID, descriptor.SettingsSchema); err != nil {
		panic(err) // wiring error: malformed schema
	}
}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	r.Post("/benchmarks", m.create)
	r.Get("/benchmarks", m.list)
	r.Get("/benchmarks/results", m.results)
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isSupportedLanguage(l string) bool {
	for _, s := range supportedLanguages {
		if l == s {
			return true
		}
	}
	return false
}

// benchmarkCreate is the fragment's BenchmarkCreate request body.
type benchmarkCreate struct {
	DeploymentID       string   `json:"deployment_id"`
	Languages          []string `json:"languages"`
	PromptsPerLanguage *int     `json:"prompts_per_language,omitempty"`
	MaxTokens          *int     `json:"max_tokens,omitempty"`
	Temperature        *float64 `json:"temperature,omitempty"`
	ModelName          string   `json:"model_name,omitempty"`
	Quantization       string   `json:"quantization,omitempty"`
	Reason             string   `json:"reason,omitempty"`
}

// create — POST /benchmarks: validate the request, submit the benchmark
// job, and return the created run. The heavy preconditions (endpoint,
// node online) are the executor's: a benchmark submitted while its node
// is offline fails as a run, which is the operator-visible unit of work.
func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	var req benchmarkCreate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if !uuidPattern.MatchString(req.DeploymentID) {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "deployment_id must be a UUID")
		return
	}
	if len(req.Languages) == 0 {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "languages is required")
		return
	}
	seen := map[string]bool{}
	for _, l := range req.Languages {
		if !isSupportedLanguage(l) || seen[l] {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable",
				"languages must be a nonempty subset of "+strings.Join(supportedLanguages, ", "))
			return
		}
		seen[l] = true
	}
	// The fragment's documented defaults; the job input always carries
	// explicit values, and the executor's own fallback chain (module
	// settings) applies only to direct submissions.
	prompts := 8
	if req.PromptsPerLanguage != nil {
		prompts = *req.PromptsPerLanguage
	}
	maxTokens := 512
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	temperature := 0.0
	if req.Temperature != nil {
		temperature = *req.Temperature
	}

	if _, err := m.env.Deploy.Get(r.Context(), req.DeploymentID); err != nil {
		if errors.Is(err, deploy.ErrUnknown) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", err.Error())
			return
		}
		httpx.HandleErr(w, err)
		return
	}

	input := map[string]any{
		"deployment_id":        req.DeploymentID,
		"languages":            req.Languages,
		"prompts_per_language": prompts,
		"max_tokens":           maxTokens,
		"temperature":          temperature,
	}
	if req.ModelName != "" {
		input["model_name"] = req.ModelName
	}
	if req.Quantization != "" {
		input["quantization"] = req.Quantization
	}
	if req.Reason != "" {
		input["reason"] = req.Reason
	}

	runID, err := m.env.Jobs.Submit(r.Context(), "benchmark", input)
	if err != nil {
		if errors.Is(err, jobs.ErrInput) {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	run, err := m.env.Runs.Get(r.Context(), runID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, run)
}

// list — GET /benchmarks: this module's runs, newest first.
func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	mod := "benchmarks"
	items, err := m.env.Runs.List(r.Context(), runs.Filter{Module: &mod})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

// resultView is the fragment's BenchmarkResult shape; the db row's
// latency/first_token/grading/reasoning columns are JSON documents.
type resultView struct {
	RunID            string             `json:"run_id"`
	Language         string             `json:"language"`
	Endpoint         *string            `json:"endpoint,omitempty"`
	Model            *string            `json:"model,omitempty"`
	Requests         int64              `json:"requests"`
	Successes        int64              `json:"successes"`
	PromptTokens     int64              `json:"prompt_tokens"`
	CompletionTokens int64              `json:"completion_tokens"`
	TotalTokens      int64              `json:"total_tokens"`
	WallSeconds      float64            `json:"wall_seconds"`
	TokensPerSecond  float64            `json:"tokens_per_second"`
	LatencyMS        map[string]float64 `json:"latency_ms,omitempty"`
	FirstTokenMS     map[string]float64 `json:"first_token_ms,omitempty"`
	Grading          map[string]any     `json:"grading,omitempty"`
	Quantization     *string            `json:"quantization,omitempty"`
	Reasoning        map[string]any     `json:"reasoning,omitempty"`
	ResultPath       *string            `json:"result_path,omitempty"`
	CreatedAt        string             `json:"created_at"`
}

func nullStrPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func jsonNumbers(s string) map[string]float64 {
	if s == "" {
		return nil
	}
	var out map[string]float64
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func jsonObject(s sql.NullString) map[string]any {
	if !s.Valid {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s.String), &out); err != nil {
		return nil
	}
	return out
}

// results — GET /benchmarks/results: per-language results across runs,
// newest first.
func (m *Module) results(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListBenchmarkResults(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]resultView, 0, len(rows))
	for i := range rows {
		out = append(out, resultView{
			RunID:            rows[i].RunID,
			Language:         rows[i].Language,
			Endpoint:         nullStrPtr(rows[i].Endpoint),
			Model:            nullStrPtr(rows[i].Model),
			Requests:         rows[i].Requests,
			Successes:        rows[i].Successes,
			PromptTokens:     rows[i].PromptTokens,
			CompletionTokens: rows[i].CompletionTokens,
			TotalTokens:      rows[i].TotalTokens,
			WallSeconds:      rows[i].WallSeconds,
			TokensPerSecond:  rows[i].TokensPerSecond,
			LatencyMS:        jsonNumbers(rows[i].Latency),
			FirstTokenMS:     jsonNumbers(rows[i].FirstToken),
			Grading:          jsonObject(rows[i].Grading),
			Quantization:     nullStrPtr(rows[i].Quantization),
			Reasoning:        jsonObject(rows[i].Reasoning),
			ResultPath:       nullStrPtr(rows[i].ResultPath),
			CreatedAt:        rows[i].CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
