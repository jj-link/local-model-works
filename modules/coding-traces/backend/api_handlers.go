package backend

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/settings"
	"github.com/jj-link/local-model-works/internal/traces"
)

type sweGymConfig struct {
	Preset                 string    `json:"preset,omitempty"`
	Dataset                string    `json:"dataset"`
	TaskIDs                []string  `json:"task_ids,omitempty"`
	Repositories           []string  `json:"repositories,omitempty"`
	Limit                  int       `json:"limit,omitempty"`
	ModelSource            string    `json:"model_source"`
	DeploymentID           string    `json:"deployment_id,omitempty"`
	Model                  string    `json:"model"`
	Endpoint               string    `json:"endpoint,omitempty"`
	SecretReference        string    `json:"secret_reference,omitempty"`
	RuntimeSecretReference string    `json:"runtime_secret_reference,omitempty"`
	RuntimeEndpoint        string    `json:"runtime_endpoint,omitempty"`
	Temperatures           []float64 `json:"temperatures"`
	RolloutsPerTask        int       `json:"rollouts_per_task"`
	MaxTurns               int       `json:"max_turns"`
	ContextLimit           int       `json:"context_limit,omitempty"`
	CaptureReasoning       bool      `json:"capture_reasoning"`
	OutputLimit            int       `json:"output_limit,omitempty"`
	Seed                   int64     `json:"seed,omitempty"`
	Workers                int       `json:"workers"`
	PerNodeWorkers         int       `json:"per_node_workers,omitempty"`
	EligibleNodes          []string  `json:"eligible_nodes,omitempty"`
	Runtime                string    `json:"runtime"`
	RetryLimit             int       `json:"retry_limit,omitempty"`
	TimeoutSeconds         int       `json:"timeout_seconds,omitempty"`
	ImagePrefix            string    `json:"image_prefix,omitempty"`
}

type samplingSpec struct {
	Name        string  `json:"name"`
	Model       string  `json:"model"`
	Temperature float64 `json:"temperature"`
	MaxTurns    int     `json:"max_turns"`
	Rollouts    int     `json:"rollouts"`
}

type sweGymPlan struct {
	Config        sweGymConfig   `json:"config"`
	ConfigDigest  string         `json:"config_digest"`
	PlanDigest    string         `json:"plan_digest"`
	Tasks         []sweGymTask   `json:"tasks"`
	Sampling      []samplingSpec `json:"sampling_matrix"`
	TotalRollouts int            `json:"total_rollouts"`
	Sources       map[string]any `json:"sources"`
	NodeCapacity  map[string]any `json:"node_capacity"`
	Warnings      []string       `json:"warnings"`
}

func (m *Module) plan(ctx context.Context, config sweGymConfig) (sweGymPlan, error) {
	if err := m.normalizeConfig(ctx, &config); err != nil {
		return sweGymPlan{}, err
	}
	rows, spec, err := m.cache.rows(ctx, config.Dataset)
	if err != nil {
		return sweGymPlan{}, err
	}
	selected, err := filterRows(rows, config.TaskIDs, config.Repositories, config.Limit)
	if err != nil {
		return sweGymPlan{}, err
	}
	tasks, err := resolveTasks(ctx, selected, config.ImagePrefix)
	if err != nil {
		return sweGymPlan{}, err
	}
	sampling, err := samplingMatrix(config)
	if err != nil {
		return sweGymPlan{}, err
	}
	rollouts := 0
	for _, item := range sampling {
		rollouts += item.Rollouts
	}
	capacity, err := m.nodeCapacity(ctx, config.EligibleNodes)
	if err != nil {
		return sweGymPlan{}, err
	}
	configBytes, err := cjson.Marshal(config)
	if err != nil {
		return sweGymPlan{}, err
	}
	configDigest := digest(configBytes)
	plan := sweGymPlan{
		Config: config, ConfigDigest: configDigest, Tasks: tasks, Sampling: sampling,
		TotalRollouts: len(tasks) * rollouts, NodeCapacity: capacity, Warnings: []string{
			"The paper names CodeAct 2.1; the pinned released OpenHands artifacts identify CodeAct 2.2.",
		},
		Sources: map[string]any{
			"openhands_commit": openHandsCommit, "swe_bench_fork_commit": sweBenchCommit,
			"dataset": spec.Name, "dataset_revision": spec.Revision, "dataset_sha256": spec.LFS,
			"hints_enabled": false, "browsing_enabled": false,
		},
	}
	withoutDigest := plan
	withoutDigest.PlanDigest = ""
	planBytes, err := cjson.Marshal(withoutDigest)
	if err != nil {
		return sweGymPlan{}, err
	}
	plan.PlanDigest = digest(planBytes)
	return plan, nil
}

func (m *Module) normalizeConfig(ctx context.Context, config *sweGymConfig) error {
	if _, ok := datasets[config.Dataset]; !ok {
		return fmt.Errorf("dataset must be lite or full")
	}
	if config.Preset == "" {
		config.Preset = "custom"
	}
	if config.ModelSource != "lmw_deployment" && config.ModelSource != "external_api" {
		return fmt.Errorf("invalid model_source")
	}
	if config.Preset != "custom" {
		if config.ModelSource != "external_api" {
			return fmt.Errorf("paper presets require external_api model source")
		}
		if config.Model == "" {
			config.Model = "paper-sampling-matrix"
		}
	} else if config.Model == "" {
		return fmt.Errorf("model is required")
	}
	if config.RolloutsPerTask < 1 {
		return fmt.Errorf("rollouts_per_task must be positive")
	}
	if config.MaxTurns < 1 {
		return fmt.Errorf("max_turns must be positive")
	}
	if config.Workers < 1 {
		return fmt.Errorf("workers must be positive")
	}
	if config.PerNodeWorkers < 1 {
		config.PerNodeWorkers = 1
	}
	if config.Runtime != "lmw_local" && config.Runtime != "openhands_remote" {
		return fmt.Errorf("invalid runtime")
	}
	if config.Runtime == "openhands_remote" && (config.RuntimeEndpoint == "" || config.RuntimeSecretReference == "") {
		return fmt.Errorf("openhands_remote requires runtime_endpoint and runtime_secret_reference")
	}
	if config.Runtime == "openhands_remote" {
		secret, err := m.env.Q.GetSecretByName(ctx, config.RuntimeSecretReference)
		if err != nil || secret.Purpose != "runtime-provider" {
			return fmt.Errorf("runtime_secret_reference must name a runtime-provider secret")
		}
	}
	if config.TimeoutSeconds == 0 {
		config.TimeoutSeconds = 7200
	}
	settingsValue, _, settingsErr := m.env.Settings.Get(ctx, descriptor.ID)
	if settingsErr == nil {
		if capture, ok := settingsValue["capture_reasoning"].(bool); ok {
			config.CaptureReasoning = capture
		} else {
			config.CaptureReasoning = true
		}
	} else {
		config.CaptureReasoning = true
	}
	if config.ContextLimit == 0 {
		config.ContextLimit = 32768
	}
	if config.OutputLimit == 0 {
		config.OutputLimit = 4096
	}
	if len(config.Temperatures) == 0 {
		config.Temperatures = []float64{0}
	}
	if config.ModelSource == "external_api" {
		if config.Endpoint == "" || config.SecretReference == "" {
			return fmt.Errorf("external_api requires endpoint and secret_reference")
		}
		secret, err := m.env.Q.GetSecretByName(ctx, config.SecretReference)
		if err != nil || secret.Purpose != "model-provider" {
			return fmt.Errorf("secret_reference must name a model-provider secret")
		}
	} else {
		if config.DeploymentID == "" {
			return fmt.Errorf("lmw_deployment requires deployment_id")
		}
		deployment, err := m.env.Deploy.Get(ctx, config.DeploymentID)
		if err != nil {
			return err
		}
		if deployment.Endpoint == nil {
			return fmt.Errorf("deployment has no endpoint")
		}
		config.Endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", deployment.Endpoint.Port)
		if len(config.EligibleNodes) == 0 {
			for _, placement := range deployment.Placements {
				if placement.Rank == 0 {
					config.EligibleNodes = []string{placement.NodeID}
					break
				}
			}
		}
	}
	return nil
}

func samplingMatrix(config sweGymConfig) ([]samplingSpec, error) {
	switch config.Preset {
	case "custom":
		out := make([]samplingSpec, 0, len(config.Temperatures))
		for _, temperature := range config.Temperatures {
			if temperature < 0 || temperature > 2 {
				return nil, fmt.Errorf("temperature out of range")
			}
			out = append(out, samplingSpec{Name: fmt.Sprintf("custom-t%g", temperature), Model: config.Model, Temperature: temperature, MaxTurns: config.MaxTurns, Rollouts: config.RolloutsPerTask})
		}
		return out, nil
	case "paper-d0":
		if config.Dataset != "lite" {
			return nil, fmt.Errorf("paper D0 uses the Lite dataset")
		}
		return []samplingSpec{{Name: "D0-gpt4o-t0", Model: "gpt-4o-2024-05-13", MaxTurns: 30, Rollouts: config.RolloutsPerTask}}, nil
	case "paper-d1":
		if config.Dataset != "lite" {
			return nil, fmt.Errorf("paper D1 uses the Lite dataset")
		}
		out := make([]samplingSpec, 0, 5)
		for _, temperature := range []float64{0.2, 0.3, 0.4, 0.5, 0.8} {
			out = append(out, samplingSpec{Name: fmt.Sprintf("D1-gpt4o-t%g", temperature), Model: "gpt-4o-2024-05-13", Temperature: temperature, MaxTurns: 30, Rollouts: config.RolloutsPerTask})
		}
		return out, nil
	case "paper-d2":
		if config.Dataset == "full" {
			return []samplingSpec{{Name: "D2-gpt4o-t0", Model: "gpt-4o-2024-05-13", MaxTurns: 50, Rollouts: config.RolloutsPerTask}, {Name: "D2-gpt4o-t1", Model: "gpt-4o-2024-05-13", Temperature: 1, MaxTurns: 50, Rollouts: config.RolloutsPerTask}}, nil
		}
		return []samplingSpec{{Name: "D2-gpt4o-t0", Model: "gpt-4o-2024-05-13", MaxTurns: 50, Rollouts: config.RolloutsPerTask}, {Name: "D2-claude35-t0", Model: "claude-3-5-sonnet-20241022", MaxTurns: 50, Rollouts: config.RolloutsPerTask}}, nil
	default:
		return nil, fmt.Errorf("unknown preset %q", config.Preset)
	}
}

func (m *Module) nodeCapacity(ctx context.Context, eligible []string) (map[string]any, error) {
	allowed := stringSet(eligible)
	nodes, err := m.env.Q.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var online []string
	for _, node := range nodes {
		if len(allowed) > 0 && !allowed[node.ID] {
			continue
		}
		if node.Status == "online" && m.env.Nodes.Online(node.ID) {
			online = append(online, node.ID)
		}
	}
	if len(online) == 0 {
		return nil, fmt.Errorf("no eligible online nodes")
	}
	return map[string]any{"eligible_nodes": online, "online_nodes": len(online)}, nil
}

func (m *Module) listTraces(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var state, task, before *string
	if value := query.Get("state"); value != "" {
		state = &value
	}
	if value := query.Get("task_id"); value != "" {
		task = &value
	}
	if value := query.Get("cursor"); value != "" {
		before = &value
	}
	var success *bool
	if value := query.Get("success"); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			httpx.WriteErr(w, 400, "invalid.success", "success must be boolean")
			return
		}
		success = &parsed
	}
	limit := int64(50)
	if value := query.Get("limit"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 1 || parsed > 200 {
			httpx.WriteErr(w, 400, "invalid.limit", "limit must be 1..200")
			return
		}
		limit = parsed
	}
	rows, err := m.env.Traces.List(r.Context(), state, task, success, before, limit)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, traceView(row))
	}
	next := ""
	if int64(len(rows)) == limit && len(rows) > 0 {
		next = rows[len(rows)-1].CreatedAt
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (m *Module) getTrace(w http.ResponseWriter, r *http.Request) {
	row, err := m.env.Traces.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view := traceView(row)
	if verification, err := m.env.Q.GetCodingTraceVerification(r.Context(), row.ID); err == nil {
		view["verification"] = verificationView(verification)
	}
	httpx.WriteJSON(w, 200, view)
}

func (m *Module) listTraceEvents(w http.ResponseWriter, r *http.Request) {
	from, _ := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64)
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	rows, err := m.env.Traces.Events(r.Context(), chi.URLParam(r, "id"), from, limit)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, eventView(row))
	}
	next := ""
	if len(rows) > 0 {
		next = strconv.FormatInt(rows[len(rows)-1].Sequence+1, 10)
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (m *Module) pinTrace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.WriteErr(w, 422, "resource.unprocessable", err.Error())
		return
	}
	id := chi.URLParam(r, "id")
	if err := m.env.Traces.Pin(r.Context(), id, body.Pinned); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	row, _ := m.env.Traces.Get(r.Context(), id)
	httpx.WriteJSON(w, 200, traceView(row))
}

func (m *Module) deleteTrace(w http.ResponseWriter, r *http.Request) {
	if err := m.env.Traces.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, traces.ErrConflict) {
			httpx.WriteErr(w, 409, "trace.active", "recording traces cannot be deleted")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) planExperiment(w http.ResponseWriter, r *http.Request) {
	var config sweGymConfig
	if err := httpx.DecodeBody(r, &config); err != nil {
		httpx.WriteErr(w, 422, "resource.unprocessable", err.Error())
		return
	}
	plan, err := m.plan(r.Context(), config)
	if err != nil {
		httpx.WriteErr(w, 422, "swe_gym.plan_invalid", err.Error())
		return
	}
	httpx.WriteJSON(w, 200, plan)
}

func (m *Module) createExperiment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Plan sweGymPlan `json:"plan"`
	}
	if err := httpx.DecodeBody(r, &body); err != nil {
		httpx.WriteErr(w, 422, "resource.unprocessable", err.Error())
		return
	}
	fresh, err := m.plan(r.Context(), body.Plan.Config)
	if err != nil {
		httpx.WriteErr(w, 422, "swe_gym.plan_invalid", err.Error())
		return
	}
	if fresh.PlanDigest != body.Plan.PlanDigest {
		httpx.WriteErr(w, 409, "plan.stale", "dataset, image, node, or configuration plan changed")
		return
	}
	experimentID, err := id.New()
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	configJSON, _ := cjson.Marshal(fresh.Config)
	planJSON, _ := cjson.Marshal(fresh)
	manifestJSON, _ := cjson.Marshal(map[string]any{"plan_digest": fresh.PlanDigest, "sources": fresh.Sources, "effective_sampling": fresh.Sampling})
	tx, err := m.env.DB.BeginTx(r.Context(), nil)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	defer tx.Rollback()
	qtx := m.env.Q.WithTx(tx)
	if err := qtx.CreateSweGymExperiment(r.Context(), db.CreateSweGymExperimentParams{ID: experimentID, Config: string(configJSON), ConfigDigest: fresh.ConfigDigest, Plan: string(planJSON), PlanDigest: fresh.PlanDigest, Manifest: string(manifestJSON), TotalItems: int64(fresh.TotalRollouts)}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	for _, task := range fresh.Tasks {
		rolloutIndex := int64(0)
		for _, sampling := range fresh.Sampling {
			for range sampling.Rollouts {
				itemID, _ := id.New()
				if err := qtx.CreateSweGymWorkItem(r.Context(), db.CreateSweGymWorkItemParams{ID: itemID, ExperimentID: experimentID, TaskID: task.InstanceID, RolloutIndex: rolloutIndex}); err != nil {
					httpx.HandleErr(w, err)
					return
				}
				rolloutIndex++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	_, err = m.env.Jobs.SubmitPrepared(r.Context(), "swe-gym-orchestrate", map[string]any{"experiment_id": experimentID}, func(runID string) error {
		rows, err := m.env.Q.SetSweGymExperimentRun(r.Context(), db.SetSweGymExperimentRunParams{RunID: sql.NullString{String: runID, Valid: true}, ID: experimentID, PlanDigest: fresh.PlanDigest})
		if err != nil {
			return err
		}
		if rows != 1 {
			return errors.New("plan.stale")
		}
		return nil
	})
	if err != nil {
		_, _ = m.env.Q.FinishSweGymExperiment(r.Context(), db.FinishSweGymExperimentParams{State: "failed", ID: experimentID})
		httpx.HandleErr(w, err)
		return
	}
	experiment, _ := m.env.Q.GetSweGymExperiment(r.Context(), experimentID)
	httpx.WriteJSON(w, 201, experimentView(experiment))
}

func (m *Module) listExperiments(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListSweGymExperiments(r.Context(), db.ListSweGymExperimentsParams{Limit: 50})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, experimentView(row))
	}
	next := ""
	if len(rows) == 50 {
		next = rows[len(rows)-1].CreatedAt
	}
	httpx.WriteJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (m *Module) getExperiment(w http.ResponseWriter, r *http.Request) {
	row, err := m.env.Q.GetSweGymExperiment(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	items, err := m.env.Q.ListSweGymWorkItems(r.Context(), row.ID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view := experimentView(row)
	view["work_items"] = items
	httpx.WriteJSON(w, 200, view)
}

func (m *Module) cancelExperiment(w http.ResponseWriter, r *http.Request) {
	row, err := m.env.Q.GetSweGymExperiment(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if row.State == "completed" || row.State == "failed" || row.State == "cancelled" {
		httpx.WriteErr(w, 409, "experiment.terminal", "experiment is already terminal")
		return
	}
	_, _ = m.env.Q.UpdateSweGymExperimentState(r.Context(), db.UpdateSweGymExperimentStateParams{State: "cancelling", ID: row.ID})
	if row.RunID.Valid {
		_ = m.env.Jobs.Cancel(r.Context(), row.RunID.String)
	}
	row, _ = m.env.Q.GetSweGymExperiment(r.Context(), row.ID)
	httpx.WriteJSON(w, 200, experimentView(row))
}

func (m *Module) resumeExperiment(w http.ResponseWriter, r *http.Request) {
	row, err := m.env.Q.GetSweGymExperiment(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if row.State != "cancelled" && row.State != "failed" {
		httpx.WriteErr(w, 409, "experiment.not_resumable", "only cancelled or failed experiments can resume")
		return
	}
	items, _ := m.env.Q.ListSweGymWorkItems(r.Context(), row.ID)
	for _, item := range items {
		_, _ = m.env.Q.SetSweGymWorkItemQueued(r.Context(), item.ID)
	}
	_, _ = m.env.Q.UpdateSweGymExperimentState(r.Context(), db.UpdateSweGymExperimentStateParams{State: "planned", ID: row.ID})
	_, err = m.env.Jobs.SubmitPrepared(r.Context(), "swe-gym-orchestrate", map[string]any{"experiment_id": row.ID}, func(runID string) error {
		rows, err := m.env.Q.SetSweGymExperimentRun(r.Context(), db.SetSweGymExperimentRunParams{RunID: sql.NullString{String: runID, Valid: true}, ID: row.ID, PlanDigest: row.PlanDigest})
		if err != nil {
			return err
		}
		if rows != 1 {
			return errors.New("experiment resume conflict")
		}
		return nil
	})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	row, _ = m.env.Q.GetSweGymExperiment(r.Context(), row.ID)
	httpx.WriteJSON(w, 200, experimentView(row))
}

func (m *Module) createExport(w http.ResponseWriter, r *http.Request) {
	var selection map[string]any
	if err := httpx.DecodeBody(r, &selection); err != nil {
		httpx.WriteErr(w, 422, "resource.unprocessable", err.Error())
		return
	}
	exportID, _ := id.New()
	selectionJSON, _ := cjson.Marshal(selection)
	runID, err := m.env.Jobs.SubmitPrepared(r.Context(), "trace-export", map[string]any{"export_id": exportID, "selection": selection}, func(runID string) error {
		return m.env.Q.CreateCodingTraceExport(r.Context(), db.CreateCodingTraceExportParams{ID: exportID, RunID: runID, Selection: string(selectionJSON), Seed: int64(number(selection["seed"], 0))})
	})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, _ := m.env.Runs.Get(r.Context(), runID)
	httpx.WriteJSON(w, 201, run)
}

func (m *Module) listExports(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListCodingTraceExports(r.Context(), 100)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	views := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		views = append(views, exportView(row))
	}
	httpx.WriteJSON(w, 200, views)
}

func (m *Module) downloadExport(w http.ResponseWriter, r *http.Request) {
	row, err := m.env.Q.GetCodingTraceExport(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if row.State != "completed" || !row.ArtifactPath.Valid {
		httpx.WriteErr(w, 409, "export.not_ready", "export is not complete")
		return
	}
	root := filepath.Join(m.env.RunRoot, "jobs", row.RunID)
	path := filepath.Clean(filepath.Join(root, filepath.FromSlash(row.ArtifactPath.String)))
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		httpx.WriteErr(w, 404, "resource.not_found", "export artifact missing")
		return
	}
	file, err := os.Open(path)
	if err != nil {
		httpx.WriteErr(w, 404, "resource.not_found", "export artifact missing")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		httpx.WriteErr(w, 404, "resource.not_found", "export artifact missing")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", row.ID+".tar.gz"))
	http.ServeContent(w, r, row.ID+".tar.gz", info.ModTime(), file)
}

func (m *Module) getSettings(w http.ResponseWriter, r *http.Request) {
	value, version, err := m.env.Settings.Get(r.Context(), descriptor.ID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	merged := defaultSettings()
	for key, item := range value {
		merged[key] = item
	}
	w.Header().Set("ETag", version)
	httpx.WriteJSON(w, 200, merged)
}

func (m *Module) putSettings(w http.ResponseWriter, r *http.Request) {
	var value map[string]any
	if err := httpx.DecodeBody(r, &value); err != nil {
		httpx.WriteErr(w, 422, "resource.unprocessable", err.Error())
		return
	}
	version, err := m.env.Settings.Set(r.Context(), descriptor.ID, value, r.Header.Get("If-Match"))
	if err != nil {
		if errors.Is(err, settings.ErrStale) {
			httpx.WriteErr(w, 409, "settings.stale", err.Error())
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	w.Header().Set("ETag", version)
	httpx.WriteJSON(w, 200, value)
}

func defaultSettings() map[string]any {
	return map[string]any{"capture_reasoning": true, "retention_days": float64(0), "export_max_context_tokens": float64(32768), "export_success_cap_per_task": float64(2)}
}
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func number(value any, fallback int) int {
	if value == nil {
		return fallback
	}
	if n, ok := value.(float64); ok {
		return int(n)
	}
	return fallback
}

func traceView(row db.CodingTrace) map[string]any {
	view := map[string]any{"id": row.ID, "run_id": row.RunID, "task_id": row.TaskID, "problem": row.Problem, "repository": row.Repository, "base_revision": row.BaseRevision, "model_source": row.ModelSource, "model": row.Model, "scaffold": row.Scaffold, "state": row.State, "token_count": row.TokenCount, "turn_count": row.TurnCount, "pinned": row.Pinned != 0, "schema_version": row.SchemaVersion, "redaction_version": row.RedactionVersion, "redaction_count": row.RedactionCount, "created_at": row.CreatedAt}
	view["sampling"] = jsonObject(row.Sampling)
	view["experiment_id"] = nullable(row.ExperimentID)
	view["final_diff"] = nullable(row.FinalDiff)
	view["failure_kind"] = nullable(row.FailureKind)
	view["digest"] = nullable(row.Digest)
	view["finished_at"] = nullable(row.FinishedAt)
	if row.SuccessLabel.Valid {
		view["success_label"] = row.SuccessLabel.Int64 == 1
	} else {
		view["success_label"] = nil
	}
	return view
}
func eventView(row db.CodingTraceEvent) map[string]any {
	return map[string]any{"trace_id": row.TraceID, "sequence": row.Sequence, "event_id": row.EventID, "agent_id": row.AgentID, "parent_agent_id": row.ParentAgentID, "occurred_at": row.OccurredAt, "kind": row.Kind, "payload": jsonObject(row.Payload), "input_tokens": row.InputTokens, "output_tokens": row.OutputTokens, "redaction_count": row.RedactionCount}
}
func verificationView(row db.CodingTraceVerification) map[string]any {
	return map[string]any{"id": row.ID, "command": row.Command, "timeout_seconds": row.TimeoutSeconds, "exit_status": nullableInt(row.ExitStatus), "stdout": row.Stdout, "stderr": row.Stderr, "fail_to_pass_report": jsonObject(row.FailToPassReport), "pass_to_pass_report": jsonObject(row.PassToPassReport), "status": row.Status, "failure_kind": nullable(row.FailureKind)}
}
func experimentView(row db.SweGymExperiment) map[string]any {
	return map[string]any{"id": row.ID, "run_id": nullable(row.RunID), "state": row.State, "config": jsonObject(row.Config), "config_digest": row.ConfigDigest, "plan": jsonObject(row.Plan), "plan_digest": row.PlanDigest, "manifest": jsonObject(row.Manifest), "total_items": row.TotalItems, "completed_items": row.CompletedItems, "resolved_items": row.ResolvedItems, "unresolved_items": row.UnresolvedItems, "infrastructure_errors": row.InfrastructureErrors, "created_at": row.CreatedAt}
}
func exportView(row db.CodingTraceExport) map[string]any {
	return map[string]any{"id": row.ID, "run_id": row.RunID, "state": row.State, "selection": jsonObject(row.Selection), "seed": row.Seed, "artifact_path": nullable(row.ArtifactPath), "manifest_digest": nullable(row.ManifestDigest), "canonical_count": row.CanonicalCount, "policy_count": row.PolicyCount, "verifier_count": row.VerifierCount, "excluded_count": row.ExcludedCount, "created_at": row.CreatedAt}
}
func jsonObject(value string) any {
	var out any
	if json.Unmarshal([]byte(value), &out) != nil {
		return map[string]any{}
	}
	return out
}
func nullable(value sql.NullString) any {
	if value.Valid {
		return value.String
	}
	return nil
}
func nullableInt(value sql.NullInt64) any {
	if value.Valid {
		return value.Int64
	}
	return nil
}
