// Package backend implements the benchmarks first-party module: the
// operator's launch point for model throughput benchmarks against a
// running deployment. A benchmark run executes one digest-pinned grader
// container per language, sequentially, on the deployment's rank-0 node
// through the agent workload protocol, and records one benchmark_results
// row per language.
package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the benchmarks backend.
type Module struct {
	env *moduleapi.Env
	// overrideCleanupDispatch is a test-only seam injected by reconciliation
	// tests to avoid a real coordinator dispatch.
	overrideCleanupDispatch cleanupDispatchFn
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// RegisterJobs declares the benchmark job kind: one digest-pinned grader
// container per language against the deployment's rank-0 endpoint.
func (m *Module) RegisterJobs(reg *jobs.Registry) {
	if err := reg.Register("benchmarks", jobs.Spec{
		Kind:          "benchmark",
		Title:         "Benchmark",
		InputSchema:   inputSchema,
		OutputSchema:  outputSchema,
		ArtifactKinds: []string{"benchmark-summary", "benchmark-result-bundle"},
		LeaseResources: func(input map[string]any) []string {
			// The coordinator owns the host Docker socket; serializing all
			// benchmark jobs on one lease keeps two Harbor runs from racing
			// cleanup on the same workstation.
			return []string{"benchmarks:harbor-coordinator"}
		},
		Executor: m.runBenchmark,
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
	HandlerFromMux(m, r)
}

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func isSupportedLanguage(language string) bool {
	return slices.Contains(supportedLanguages, language)
}

type benchmarkOriginInput struct {
	RunID     string `json:"run_id"`
	ProjectID string `json:"project_id"`
}

type benchmarkOriginView struct {
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	RunID      string `json:"run_id,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
}

type benchmarkCreate struct {
	BenchmarkID              string                `json:"benchmark_id"`
	Version                  string                `json:"version"`
	Harness                  string                `json:"harness"`
	GenerationDeploymentID   string                `json:"generation_deployment_id,omitempty"`
	GenerationModel          *string               `json:"generation_model,omitempty"`
	VerificationDeploymentID string                `json:"verification_deployment_id,omitempty"`
	VerificationModel        *string               `json:"verification_model,omitempty"`
	TaskIDs                  []string              `json:"task_ids,omitempty"`
	CandidateCount           *int                  `json:"candidate_count,omitempty"`
	Concurrency              *int                  `json:"concurrency,omitempty"`
	Seed                     *int                  `json:"seed,omitempty"`
	VerifierRepetitions      *int                  `json:"verifier_repetitions,omitempty"`
	VerifierPivots           *int                  `json:"verifier_pivots,omitempty"`
	Languages                []string              `json:"languages,omitempty"`
	PromptsPerLanguage       *int                  `json:"prompts_per_language,omitempty"`
	MaxTokens                *int                  `json:"max_tokens,omitempty"`
	Temperature              *float64              `json:"temperature,omitempty"`
	Reason                   string                `json:"reason,omitempty"`
	Origin                   *benchmarkOriginInput `json:"origin,omitempty"`
}

type benchmarkCreateError struct {
	status  int
	code    string
	message string
}

func (e *benchmarkCreateError) Error() string { return e.message }

func invalidBenchmarkCreate(message string) error {
	return &benchmarkCreateError{status: http.StatusUnprocessableEntity, code: "resource.unprocessable", message: message}
}

func benchmarkInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func (m *Module) maxBenchmarkConcurrency(ctx context.Context) (int, error) {
	values, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return 0, err
	}
	maxConcurrency := defaultBenchmarkConcurrency
	if value, ok := values["max_concurrency"].(float64); ok && value >= 1 && value <= 32 {
		maxConcurrency = int(value)
	}
	return maxConcurrency, nil
}

func (m *Module) requireBenchmarkRunner(ctx context.Context) (string, error) {
	availability, err := m.benchmarkRunnerAvailability(ctx)
	if err != nil {
		return "", err
	}
	if !availability.Configured {
		return "", &benchmarkCreateError{
			status: http.StatusConflict, code: "benchmarks.runner_not_configured",
			message: "the content-addressed local benchmark runner is not configured",
		}
	}
	if !availability.Online {
		return "", &benchmarkCreateError{
			status: http.StatusConflict, code: "benchmarks.runner_offline",
			message: "the local benchmark runner is offline",
		}
	}
	return availability.NodeID, nil
}

func validateModelOverride(field string, value **string) error {
	if *value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(**value)
	if trimmed == "" {
		return invalidBenchmarkCreate(field + " must be a non-empty string")
	}
	*value = &trimmed
	return nil
}

func validateTerminalTaskIDs(taskIDs []string) error {
	seen := make(map[string]struct{}, len(taskIDs))
	for _, taskID := range taskIDs {
		if !taskIDPattern.MatchString(taskID) {
			return invalidBenchmarkCreate("task_ids must contain only letters, numbers, dot, underscore, and hyphen")
		}
		if _, duplicate := seen[taskID]; duplicate {
			return invalidBenchmarkCreate("task_ids must be unique")
		}
		if _, found := slices.BinarySearch(terminalBenchTaskIDs, taskID); !found {
			return invalidBenchmarkCreate("task_ids contains an unknown Terminal-Bench 3.0.0 task")
		}
		seen[taskID] = struct{}{}
	}
	return nil
}

func (m *Module) resolveBenchmarkDeployment(ctx context.Context, field, deploymentID string, modelOverride *string) (string, error) {
	deployment, err := m.env.Deploy.Get(ctx, deploymentID)
	if err != nil {
		if errors.Is(err, deploy.ErrUnknown) {
			return "", &benchmarkCreateError{status: http.StatusNotFound, code: "resource.not_found", message: field + " deployment was not found"}
		}
		return "", err
	}
	if deployment.ObservedState != "healthy" || deployment.Endpoint == nil {
		return "", &benchmarkCreateError{status: http.StatusConflict, code: "benchmarks.deployment_unhealthy", message: field + " deployment must be healthy"}
	}
	if _, err := deploy.OpenAIAPIBase(deployment.Endpoint); err != nil {
		return "", &benchmarkCreateError{status: http.StatusConflict, code: "benchmarks.deployment_unhealthy", message: field + " deployment has no usable endpoint"}
	}
	model := strings.TrimSpace(deployment.Endpoint.Model)
	if modelOverride != nil {
		model = *modelOverride
	}
	if model == "" {
		return "", &benchmarkCreateError{status: http.StatusConflict, code: "benchmarks.deployment_unhealthy", message: field + " deployment has no model"}
	}
	return model, nil
}

func (m *Module) validatedBenchmarkInput(ctx context.Context, req benchmarkCreate) (map[string]any, error) {
	if err := validateModelOverride("generation_model", &req.GenerationModel); err != nil {
		return nil, err
	}
	if err := validateModelOverride("verification_model", &req.VerificationModel); err != nil {
		return nil, err
	}
	maxConcurrency, err := m.maxBenchmarkConcurrency(ctx)
	if err != nil {
		return nil, err
	}
	candidateCount := benchmarkInt(req.CandidateCount, 1)
	concurrency := benchmarkInt(req.Concurrency, 1)
	seed := benchmarkInt(req.Seed, 0)
	if candidateCount < 1 || candidateCount > 10 {
		return nil, invalidBenchmarkCreate("candidate_count must be between 1 and 10")
	}
	if concurrency < 1 || concurrency > 32 {
		return nil, invalidBenchmarkCreate("concurrency must be between 1 and 32")
	}
	if concurrency > maxConcurrency {
		return nil, invalidBenchmarkCreate("concurrency cannot exceed the benchmark max_concurrency setting")
	}
	if req.GenerationDeploymentID != "" && !uuidPattern.MatchString(req.GenerationDeploymentID) {
		return nil, invalidBenchmarkCreate("generation_deployment_id must be a UUID")
	}
	if req.VerificationDeploymentID != "" && !uuidPattern.MatchString(req.VerificationDeploymentID) {
		return nil, invalidBenchmarkCreate("verification_deployment_id must be a UUID")
	}

	input := map[string]any{
		"benchmark_id":    req.BenchmarkID,
		"version":         req.Version,
		"harness":         req.Harness,
		"candidate_count": candidateCount,
		"concurrency":     concurrency,
		"seed":            seed,
	}
	if reason := strings.TrimSpace(req.Reason); reason != "" {
		input["reason"] = reason
	}

	switch {
	case req.BenchmarkID == "lmw-code-generation" && req.Version == "1" && req.Harness == "lmw-oneshot":
		if req.GenerationDeploymentID == "" {
			return nil, invalidBenchmarkCreate("generation_deployment_id is required")
		}
		if candidateCount != 1 {
			return nil, invalidBenchmarkCreate("lmw-code-generation requires candidate_count 1")
		}
		if req.TaskIDs != nil {
			return nil, invalidBenchmarkCreate("lmw-code-generation does not accept task_ids")
		}
		if req.VerificationDeploymentID != "" || req.VerificationModel != nil || req.VerifierRepetitions != nil || req.VerifierPivots != nil {
			return nil, invalidBenchmarkCreate("lmw-code-generation does not accept verifier fields")
		}
		if len(req.Languages) == 0 {
			return nil, invalidBenchmarkCreate("languages is required")
		}
		seenLanguages := make(map[string]struct{}, len(req.Languages))
		for _, language := range req.Languages {
			if !isSupportedLanguage(language) {
				return nil, invalidBenchmarkCreate("languages must be a subset of " + strings.Join(supportedLanguages, ", "))
			}
			if _, duplicate := seenLanguages[language]; duplicate {
				return nil, invalidBenchmarkCreate("languages must be unique")
			}
			seenLanguages[language] = struct{}{}
		}
		prompts := benchmarkInt(req.PromptsPerLanguage, 8)
		maxTokens := benchmarkInt(req.MaxTokens, 512)
		temperature := 0.0
		if req.Temperature != nil {
			temperature = *req.Temperature
		}
		if prompts < 1 || prompts > 256 {
			return nil, invalidBenchmarkCreate("prompts_per_language must be between 1 and 256")
		}
		if maxTokens < 16 || maxTokens > 16384 {
			return nil, invalidBenchmarkCreate("max_tokens must be between 16 and 16384")
		}
		if temperature < 0 || temperature > 2 {
			return nil, invalidBenchmarkCreate("temperature must be between 0 and 2")
		}
		generationModel, err := m.resolveBenchmarkDeployment(ctx, "generation", req.GenerationDeploymentID, req.GenerationModel)
		if err != nil {
			return nil, err
		}
		input["generation_deployment_id"] = req.GenerationDeploymentID
		input["generation_model"] = generationModel
		input["languages"] = req.Languages
		input["prompts_per_language"] = prompts
		input["max_tokens"] = maxTokens
		input["temperature"] = temperature
		return input, nil

	case req.BenchmarkID == "terminal-bench" && req.Version == "3.0.0":
		if req.Languages != nil || req.PromptsPerLanguage != nil || req.MaxTokens != nil || req.Temperature != nil {
			return nil, invalidBenchmarkCreate("terminal-bench does not accept code-generation grader fields")
		}
		if err := validateTerminalTaskIDs(req.TaskIDs); err != nil {
			return nil, err
		}
		if _, err := m.requireBenchmarkRunner(ctx); err != nil {
			return nil, err
		}
		input["task_ids"] = req.TaskIDs
		switch req.Harness {
		case "oracle":
			if req.GenerationDeploymentID != "" || req.GenerationModel != nil || req.VerificationDeploymentID != "" || req.VerificationModel != nil || req.VerifierRepetitions != nil || req.VerifierPivots != nil {
				return nil, invalidBenchmarkCreate("terminal-bench oracle does not accept deployment or verifier fields")
			}
			return input, nil
		case "codex", "mini-swe-agent":
			if req.GenerationDeploymentID == "" {
				return nil, invalidBenchmarkCreate("generation_deployment_id is required")
			}
			generationModel, err := m.resolveBenchmarkDeployment(ctx, "generation", req.GenerationDeploymentID, req.GenerationModel)
			if err != nil {
				return nil, err
			}
			input["generation_deployment_id"] = req.GenerationDeploymentID
			input["generation_model"] = generationModel
			if req.VerificationDeploymentID == "" {
				if req.VerificationModel != nil || req.VerifierRepetitions != nil || req.VerifierPivots != nil {
					return nil, invalidBenchmarkCreate("verifier fields require verification_deployment_id")
				}
				return input, nil
			}
			if candidateCount < 2 {
				return nil, invalidBenchmarkCreate("verification requires at least two candidates")
			}
			repetitions := benchmarkInt(req.VerifierRepetitions, 8)
			pivots := 2
			if candidateCount-1 < pivots {
				pivots = candidateCount - 1
			}
			if req.VerifierPivots != nil {
				pivots = *req.VerifierPivots
			}
			if repetitions < 1 || repetitions > 32 {
				return nil, invalidBenchmarkCreate("verifier_repetitions must be between 1 and 32")
			}
			if pivots < 1 || pivots > candidateCount {
				return nil, invalidBenchmarkCreate("verifier_pivots must be between 1 and candidate_count")
			}
			verificationModel, err := m.resolveBenchmarkDeployment(ctx, "verification", req.VerificationDeploymentID, req.VerificationModel)
			if err != nil {
				return nil, err
			}
			input["verification_deployment_id"] = req.VerificationDeploymentID
			input["verification_model"] = verificationModel
			input["verifier_repetitions"] = repetitions
			input["verifier_pivots"] = pivots
			return input, nil
		default:
			return nil, invalidBenchmarkCreate("unsupported terminal-bench harness")
		}
	default:
		return nil, invalidBenchmarkCreate("unsupported benchmark_id, version, or harness")
	}
}

func (m *Module) ensureBenchmarkRunResult(ctx context.Context, runID string, input map[string]any) error {
	var req benchmarkCreate
	if err := decodeBenchmarkInput(input, &req); err != nil {
		return fmt.Errorf("decode benchmark result metadata: %w", err)
	}
	nodeID := ""
	taskCount := len(req.Languages)
	switch req.BenchmarkID {
	case "lmw-code-generation":
		deployment, err := m.env.Deploy.Get(ctx, req.GenerationDeploymentID)
		if err != nil {
			return err
		}
		nodeID, err = rankZeroNode(deployment.Placements)
		if err != nil {
			return err
		}
	case "terminal-bench":
		var err error
		nodeID, err = nodes.ApprovedLocalRunnerNodeID(ctx, m.env.Q, m.env.Nodes)
		if err != nil {
			return err
		}
		taskCount = len(req.TaskIDs)
		if taskCount == 0 {
			taskCount = len(terminalBenchTaskIDs)
		}
	default:
		return fmt.Errorf("unsupported benchmark_id %q", req.BenchmarkID)
	}
	nullable := func(value string) sql.NullString {
		return sql.NullString{String: value, Valid: value != ""}
	}
	if err := m.env.Q.InsertBenchmarkRunResult(ctx, db.InsertBenchmarkRunResultParams{
		RunID:                    runID,
		BenchmarkID:              req.BenchmarkID,
		BenchmarkVersion:         req.Version,
		Harness:                  req.Harness,
		GenerationDeploymentID:   nullable(req.GenerationDeploymentID),
		VerificationDeploymentID: nullable(req.VerificationDeploymentID),
		ExecutionNodeID:          nullable(nodeID),
		TaskCount:                int64(taskCount),
		CandidateCount:           int64(benchmarkInt(req.CandidateCount, 1)),
	}); err != nil {
		return fmt.Errorf("insert benchmark run result: %w", err)
	}
	return nil
}

func applyBenchmarkOrigin(ctx context.Context, req benchmarkCreate, input map[string]any) error {
	principal := auth.APITokenPrincipalFromContext(ctx)
	if principal == nil {
		if req.Origin != nil {
			return &benchmarkCreateError{status: http.StatusUnprocessableEntity, code: "benchmarks.service_origin",
				message: "service origin requires API token authentication"}
		}
		return nil
	}
	origin := map[string]any{"client_id": principal.ID, "client_name": principal.Name}
	if req.Origin != nil {
		if !uuidPattern.MatchString(req.Origin.RunID) || !uuidPattern.MatchString(req.Origin.ProjectID) {
			return invalidBenchmarkCreate("origin run_id and project_id must be UUIDs")
		}
		origin["run_id"] = req.Origin.RunID
		origin["project_id"] = req.Origin.ProjectID
	}
	input["origin"] = origin
	return nil
}

func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	var req benchmarkCreate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	input, err := m.validatedBenchmarkInput(r.Context(), req)
	if err != nil {
		var requestErr *benchmarkCreateError
		if errors.As(err, &requestErr) {
			httpx.WriteErr(w, requestErr.status, requestErr.code, requestErr.message)
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if err := applyBenchmarkOrigin(r.Context(), req, input); err != nil {
		var requestErr *benchmarkCreateError
		if errors.As(err, &requestErr) {
			httpx.WriteErr(w, requestErr.status, requestErr.code, requestErr.message)
			return
		}
		httpx.HandleErr(w, err)
		return
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
	if err := m.ensureBenchmarkRunResult(r.Context(), runID, input); err != nil {
		_ = m.env.Jobs.Cancel(r.Context(), runID)
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

// list returns a stable page from the complete benchmark run ledger.
func (m *Module) list(w http.ResponseWriter, r *http.Request) {
	runID := r.URL.Query().Get("origin_run_id")
	projectID := r.URL.Query().Get("origin_project_id")
	if (runID == "") != (projectID == "") ||
		runID != "" && (!uuidPattern.MatchString(runID) || !uuidPattern.MatchString(projectID)) {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "origin_run_id and origin_project_id must be UUIDs and supplied together")
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "limit must be between 1 and 200")
			return
		}
		limit = parsed
	}
	clientID := ""
	if principal := auth.APITokenPrincipalFromContext(r.Context()); principal != nil {
		clientID = principal.ID
	}
	items, next, err := m.env.Runs.ListBenchmarkOrigin(r.Context(), runs.BenchmarkOriginFilter{
		ClientID: clientID, RunID: runID, ProjectID: projectID,
		Cursor: r.URL.Query().Get("cursor"), Limit: limit,
	})
	if err != nil {
		if errors.Is(err, runs.ErrInvalidCursor) {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "cursor is malformed")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if next != "" {
		w.Header().Set("X-Next-Cursor", next)
	}
	httpx.WriteJSON(w, http.StatusOK, items)
}

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
	Reasoning        map[string]any     `json:"reasoning,omitempty"`
	ResultPath       *string            `json:"result_path,omitempty"`
	CreatedAt        string             `json:"created_at"`
}

type benchmarkRunResultView struct {
	RunID                    string               `json:"run_id"`
	BenchmarkID              string               `json:"benchmark_id"`
	BenchmarkVersion         string               `json:"benchmark_version"`
	Harness                  string               `json:"harness"`
	GenerationDeploymentID   *string              `json:"generation_deployment_id,omitempty"`
	VerificationDeploymentID *string              `json:"verification_deployment_id,omitempty"`
	ExecutionNodeID          *string              `json:"execution_node_id,omitempty"`
	Origin                   *benchmarkOriginView `json:"origin,omitempty"`
	TaskCount                int64                `json:"task_count"`
	CandidateCount           int64                `json:"candidate_count"`
	PassedCount              int64                `json:"passed_count"`
	PassAt1                  *float64             `json:"pass_at_1"`
	VerifierPassRate         *float64             `json:"verifier_pass_rate"`
	OraclePassRate           *float64             `json:"oracle_pass_rate"`
	PromptTokens             int64                `json:"prompt_tokens"`
	CompletionTokens         int64                `json:"completion_tokens"`
	TotalTokens              int64                `json:"total_tokens"`
	WallSeconds              float64              `json:"wall_seconds"`
	SummaryArtifactID        *string              `json:"summary_artifact_id,omitempty"`
	BundleArtifactID         *string              `json:"bundle_artifact_id,omitempty"`
	Metrics                  map[string]any       `json:"metrics"`
	LanguageResults          []resultView         `json:"language_results"`
	CreatedAt                string               `json:"created_at"`
}

type benchmarkRunSummaryView struct {
	Run    runs.Run               `json:"run"`
	Result benchmarkRunResultView `json:"result"`
}

type benchmarkTrialView struct {
	RunID            string         `json:"run_id"`
	TaskID           string         `json:"task_id"`
	CandidateIndex   int64          `json:"candidate_index"`
	OfficialPass     bool           `json:"official_pass"`
	VerifierSelected bool           `json:"verifier_selected"`
	VerifierScore    *float64       `json:"verifier_score"`
	TrajectoryPath   string         `json:"trajectory_path"`
	Metrics          map[string]any `json:"metrics"`
	CreatedAt        string         `json:"created_at"`
}

func nullStrPtr(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func nullFloatPtr(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	copy := value.Float64
	return &copy
}

func jsonNumbers(raw string) map[string]float64 {
	if raw == "" {
		return nil
	}
	var out map[string]float64
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func jsonObject(raw sql.NullString) map[string]any {
	if !raw.Valid {
		return nil
	}
	return jsonMap(raw.String)
}

func jsonMap(raw string) map[string]any {
	out := map[string]any{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func languageResultView(row db.BenchmarkResult) resultView {
	return resultView{
		RunID:            row.RunID,
		Language:         row.Language,
		Endpoint:         nullStrPtr(row.Endpoint),
		Model:            nullStrPtr(row.Model),
		Requests:         row.Requests,
		Successes:        row.Successes,
		PromptTokens:     row.PromptTokens,
		CompletionTokens: row.CompletionTokens,
		TotalTokens:      row.TotalTokens,
		WallSeconds:      row.WallSeconds,
		TokensPerSecond:  row.TokensPerSecond,
		LatencyMS:        jsonNumbers(row.Latency),
		FirstTokenMS:     jsonNumbers(row.FirstToken),
		Grading:          jsonObject(row.Grading),
		Reasoning:        jsonObject(row.Reasoning),
		ResultPath:       nullStrPtr(row.ResultPath),
		CreatedAt:        row.CreatedAt,
	}
}

func (m *Module) benchmarkRunResultView(ctx context.Context, row db.BenchmarkRunResult) (benchmarkRunResultView, error) {
	languageRows, err := m.env.Q.ListBenchmarkResultsByRun(ctx, row.RunID)
	if err != nil {
		return benchmarkRunResultView{}, err
	}
	languages := make([]resultView, 0, len(languageRows))
	for _, languageRow := range languageRows {
		languages = append(languages, languageResultView(languageRow))
	}
	return benchmarkRunResultView{
		RunID:                    row.RunID,
		BenchmarkID:              row.BenchmarkID,
		BenchmarkVersion:         row.BenchmarkVersion,
		Harness:                  row.Harness,
		GenerationDeploymentID:   nullStrPtr(row.GenerationDeploymentID),
		VerificationDeploymentID: nullStrPtr(row.VerificationDeploymentID),
		ExecutionNodeID:          nullStrPtr(row.ExecutionNodeID),
		TaskCount:                row.TaskCount,
		CandidateCount:           row.CandidateCount,
		PassedCount:              row.PassedCount,
		PassAt1:                  nullFloatPtr(row.PassAt1),
		VerifierPassRate:         nullFloatPtr(row.VerifierPassRate),
		OraclePassRate:           nullFloatPtr(row.OraclePassRate),
		PromptTokens:             row.PromptTokens,
		CompletionTokens:         row.CompletionTokens,
		TotalTokens:              row.TotalTokens,
		WallSeconds:              row.WallSeconds,
		SummaryArtifactID:        nullStrPtr(row.SummaryArtifactID),
		BundleArtifactID:         nullStrPtr(row.BundleArtifactID),
		Metrics:                  jsonMap(row.MetricsJson),
		LanguageResults:          languages,
		CreatedAt:                row.CreatedAt,
	}, nil
}

func (m *Module) benchmarkSummary(ctx context.Context, runID string) (benchmarkRunSummaryView, error) {
	run, err := m.env.Runs.Get(ctx, runID)
	if err != nil {
		return benchmarkRunSummaryView{}, err
	}
	if run.Module != descriptor.ID {
		return benchmarkRunSummaryView{}, runs.ErrUnknown
	}
	result, err := m.env.Q.GetBenchmarkRunResult(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return benchmarkRunSummaryView{}, runs.ErrUnknown
		}
		return benchmarkRunSummaryView{}, err
	}
	view, err := m.benchmarkRunResultView(ctx, result)
	if err != nil {
		return benchmarkRunSummaryView{}, err
	}
	view.Origin = benchmarkOrigin(run)
	return benchmarkRunSummaryView{Run: run, Result: view}, nil
}

func writeBenchmarkLookupError(w http.ResponseWriter, err error) {
	var requestErr *benchmarkCreateError
	if errors.As(err, &requestErr) {
		httpx.WriteErr(w, requestErr.status, requestErr.code, requestErr.message)
		return
	}
	if errors.Is(err, runs.ErrUnknown) {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark run was not found")
		return
	}
	httpx.HandleErr(w, err)
}

func benchmarkOrigin(run runs.Run) *benchmarkOriginView {
	raw, ok := run.Input["origin"].(map[string]any)
	if !ok {
		return nil
	}
	clientID, _ := raw["client_id"].(string)
	clientName, _ := raw["client_name"].(string)
	runID, _ := raw["run_id"].(string)
	projectID, _ := raw["project_id"].(string)
	if clientID == "" || clientName == "" || (runID == "") != (projectID == "") {
		return nil
	}
	return &benchmarkOriginView{ClientID: clientID, ClientName: clientName, RunID: runID, ProjectID: projectID}
}

func (m *Module) enforceAPITokenOwnership(ctx context.Context, runID string) error {
	principal := auth.APITokenPrincipalFromContext(ctx)
	if principal == nil {
		return nil
	}
	run, err := m.env.Runs.Get(ctx, runID)
	if err != nil || run.Module != descriptor.ID {
		return runs.ErrUnknown
	}
	origin := benchmarkOrigin(run)
	if origin == nil || origin.ClientID != principal.ID {
		return &benchmarkCreateError{status: http.StatusForbidden, code: "auth.token_ownership",
			message: "benchmark run is not owned by this API token"}
	}
	return nil
}

func (m *Module) results(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListBenchmarkRunResults(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]benchmarkRunSummaryView, 0, len(rows))
	for _, row := range rows {
		run, err := m.env.Runs.Get(r.Context(), row.RunID)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		result, err := m.benchmarkRunResultView(r.Context(), row)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		result.Origin = benchmarkOrigin(run)
		out = append(out, benchmarkRunSummaryView{Run: run, Result: result})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) get(w http.ResponseWriter, r *http.Request, runID string) {
	summary, err := m.benchmarkSummary(r.Context(), runID)
	if err != nil {
		writeBenchmarkLookupError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, summary)
}

func (m *Module) cancel(w http.ResponseWriter, r *http.Request, runID string) {
	if err := m.enforceAPITokenOwnership(r.Context(), runID); err != nil {
		writeBenchmarkLookupError(w, err)
		return
	}
	run, err := m.env.Runs.Get(r.Context(), runID)
	if err != nil || run.Module != descriptor.ID {
		if err == nil {
			err = runs.ErrUnknown
		}
		writeBenchmarkLookupError(w, err)
		return
	}
	if err := m.env.Jobs.Cancel(r.Context(), runID); err != nil {
		if errors.Is(err, runs.ErrInvalidTransition) {
			httpx.WriteErr(w, http.StatusConflict, "resource.conflict", err.Error())
			return
		}
		writeBenchmarkLookupError(w, err)
		return
	}
	run, err = m.env.Runs.Get(r.Context(), runID)
	if err != nil {
		writeBenchmarkLookupError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

// interruptedHarborRuns lists interrupted terminal-bench runs still owning
// a workspace ownership manifest: controller restart orphaned their Harbor
// containers/networks/volumes and the normal exit-path cleanup never ran.
func (m *Module) interruptedHarborRuns(ctx context.Context) ([]string, error) {
	module := descriptor.ID
	interrupted := string(runs.Interrupted)
	runsList, err := m.env.Runs.List(ctx, runs.Filter{Module: &module, State: &interrupted, Limit: 200})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(runsList))
	for _, entry := range runsList {
		manifest := filepath.Join(m.env.RunRoot, "jobs", entry.ID, "ownership.json")
		info, err := os.Lstat(manifest)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, entry.ID)
	}
	return out, nil
}

// cleanupDispatchFn abstracts the ownership cleanup dispatch for tests.
type cleanupDispatchFn func(ctx context.Context, runID string) error

// ReconcileInterruptedRuns is the restart reconciliation hook: dispatch the
// runner's `cleanup <ownership.json>` once per interrupted run workspace so
// Harbor-owned resources do not survive a controller crash. Best effort
// per run; ids of processed runs are logged for the operator.
func (m *Module) ReconcileInterruptedRuns(ctx context.Context) int {
	runIDs, err := m.interruptedHarborRuns(ctx)
	if err != nil {
		return 0
	}
	dispatch := cleanupDispatchFn(m.cleanupOwnershipForRun)
	if m.overrideCleanupDispatch != nil {
		dispatch = m.overrideCleanupDispatch
	}
	processed := 0
	for _, runID := range runIDs {
		_ = m.env.Runs.AppendLog(runID, "", 0, "stdout", []byte("[benchmark] restart reconciliation: dispatching ownership cleanup\n"))
		if err := dispatch(ctx, runID); err != nil {
			_ = m.env.Runs.AppendLog(runID, "", 0, "stdout", []byte("[benchmark] restart reconciliation cleanup failed: "+err.Error()+"\n"))
			continue
		}
		processed++
	}
	return processed
}

// harborRunView adapts an interrupted run into the harborJobContext view
// using the runs service log writer.
type harborRunView struct {
	runID     string
	workspace string
	logSink   func(string, ...any)
}

func (v harborRunView) RunID() string                   { return v.runID }
func (v harborRunView) Workspace() string               { return v.workspace }
func (v harborRunView) Logf(format string, args ...any) { v.logSink(format, args...) }

// cleanupOwnershipForRun dispatches the Harbor ownership cleanup for one
// interrupted run: it reuses the pinned coordinator spec and the runner's
// `cleanup <ownership.json>` command, then appends the outcome to the run
// log. The workspace must already hold a valid ownership manifest.
func (m *Module) cleanupOwnershipForRun(ctx context.Context, runID string) error {
	workspace := filepath.Join(m.env.RunRoot, "jobs", runID)
	if err := m.env.Runs.AppendLog(runID, "", 0, "stdout", []byte("[benchmark] harbor ownership cleanup starting\n")); err != nil {
		return fmt.Errorf("append log: %w", err)
	}
	view := harborRunView{runID: runID, workspace: workspace, logSink: func(format string, args ...any) {
		_ = m.env.Runs.AppendLog(runID, "", 0, "stdout", []byte(fmt.Sprintf(format+"\n", args...)))
	}}
	settings, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return err
	}
	imageRef, _ := settings["harbor_runner_image"].(string)
	m.cleanupHarborOwnership(ctx, strings.TrimSpace(imageRef), view)
	return nil
}

func (m *Module) trials(w http.ResponseWriter, r *http.Request, runID string) {
	if _, err := m.benchmarkSummary(r.Context(), runID); err != nil {
		writeBenchmarkLookupError(w, err)
		return
	}
	rows, err := m.env.Q.ListBenchmarkTrialResultsByRun(r.Context(), runID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]benchmarkTrialView, 0, len(rows))
	for _, row := range rows {
		out = append(out, benchmarkTrialView{
			RunID:            row.RunID,
			TaskID:           row.TaskID,
			CandidateIndex:   row.CandidateIndex,
			OfficialPass:     row.OfficialPass == 1,
			VerifierSelected: row.VerifierSelected == 1,
			VerifierScore:    nullFloatPtr(row.VerifierScore),
			TrajectoryPath:   row.TrajectoryPath,
			Metrics:          jsonMap(row.MetricsJson),
			CreatedAt:        row.CreatedAt,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) bundle(w http.ResponseWriter, r *http.Request, runID string) {
	m.serveBenchmarkArtifact(w, r, runID, "benchmark-result-bundle", "result-bundle.tar.gz", "application/gzip")
}

func (m *Module) summaryArtifact(w http.ResponseWriter, r *http.Request, runID string) {
	m.serveBenchmarkArtifact(w, r, runID, "benchmark-summary", "summary.json", "application/json")
}

func (m *Module) serveBenchmarkArtifact(w http.ResponseWriter, r *http.Request, runID, kind, filename, contentType string) {
	summary, err := m.benchmarkSummary(r.Context(), runID)
	if err != nil {
		writeBenchmarkLookupError(w, err)
		return
	}
	if summary.Run.State != string(runs.Succeeded) {
		httpx.WriteErr(w, http.StatusConflict, "benchmarks.artifact_unavailable", "benchmark artifacts are available only for succeeded runs")
		return
	}
	artifactID := summary.Result.BundleArtifactID
	if kind == "benchmark-summary" {
		artifactID = summary.Result.SummaryArtifactID
	}
	if artifactID == nil {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
		return
	}
	artifact, err := m.env.Q.GetArtifact(r.Context(), *artifactID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	var metadata struct {
		RunID string `json:"run_id"`
		Path  string `json:"path"`
	}
	if artifact.Kind != kind || !artifact.Digest.Valid ||
		!digestPattern.MatchString(artifact.Digest.String) ||
		json.Unmarshal([]byte(artifact.Metadata), &metadata) != nil || metadata.RunID != runID {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
		return
	}
	workspace := filepath.Join(m.env.RunRoot, "jobs", runID)
	path := filepath.Clean(metadata.Path)
	relative, err := filepath.Rel(workspace, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "benchmark artifact was not found")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("ETag", `"`+artifact.Digest.String+`"`)
	http.ServeContent(w, r, filename, info.ModTime(), file)
}
