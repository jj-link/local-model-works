package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	runsvc "github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/workload"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (m *Module) submitFactoryRunContext(ctx context.Context, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string) (string, error) {
	input["project_id"] = project.ID
	input["factory"] = factory
	input["provider_config"] = m.projectConfigWithDefaults(ctx, project.ConfigJson)
	runID, err := m.env.Jobs.Submit(ctx, "autoresearch-factory", input)
	if err != nil {
		return "", err
	}
	if err := m.env.Q.CreateAutoResearchRun(ctx, db.CreateAutoResearchRunParams{
		RunID: runID, ProjectID: project.ID, Factory: factory, ParentRunID: nullString(parentRunID),
		WorkerNodeID: project.RunnerNodeID, ConfigSnapshot: project.ConfigJson,
	}); err != nil {
		_ = m.env.Runs.Cancel(ctx, runID)
		return "", err
	}
	_, err = m.env.DB.ExecContext(ctx, `UPDATE autoresearch_projects
		SET status='running', version=version+1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, project.ID)
	if err != nil {
		_ = m.env.Runs.Cancel(ctx, runID)
		return "", err
	}
	return runID, nil
}

func (m *Module) submitFactoryRun(r *http.Request, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string) (string, error) {
	return m.submitFactoryRunContext(r.Context(), project, factory, input, parentRunID)
}

func (m *Module) GenerateAutoResearchIdeas(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	project, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	var req AutoResearchIdeaGenerate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	count := 1
	if req.CandidateCount != nil {
		count = *req.CandidateCount
	}
	if count < 1 || count > 10 {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "candidate_count must be from 1 through 10")
		return
	}
	sources, err := m.env.Q.ListAutoResearchSources(r.Context(), project.ID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	for _, source := range sources {
		if source.Status != "ready" {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.source_decision_required", "all attached sources must be ready before candidate generation")
			return
		}
	}
	prompt := project.IdeaPrompt
	if req.Prompt != nil {
		prompt = *req.Prompt
	}
	input := map[string]any{"candidate_count": count, "prompt": prompt}
	runID, err := m.submitFactoryRun(r, project, "idea", input, "")
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
	httpx.WriteJSON(w, http.StatusAccepted, run)
}

func (m *Module) CreateAutoResearchRun(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	project, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	var req AutoResearchRunCreate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if req.Factory == Idea {
		selected, err := m.env.Q.CountSelectedAutoResearchIdeas(r.Context(), project.ID)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		if selected == 0 {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.idea_selection_required", "select at least one idea before continuing")
			return
		}
	}
	input := map[string]any{}
	if req.ProviderOverrides != nil {
		input["provider_overrides"] = req.ProviderOverrides
	}
	if req.SshSecretName != nil {
		input["ssh_secret_name"] = *req.SshSecretName
	}
	parent := ""
	if req.ParentRunId != nil {
		parent = req.ParentRunId.String()
		input["parent_run_id"] = parent
	}
	runID, err := m.submitFactoryRun(r, project, string(req.Factory), input, parent)
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

func (m *Module) ListAutoResearchRuns(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	links, err := m.env.Q.ListAutoResearchRuns(r.Context(), projectID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	views := make([]any, 0, len(links))
	for _, link := range links {
		run, err := m.env.Runs.Get(r.Context(), link.RunID)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		views = append(views, run)
	}
	httpx.WriteJSON(w, http.StatusOK, views)
}

func (m *Module) runWorkloadClient(ctx context.Context, runID string) (*workload.Client, error) {
	link, err := m.env.Q.GetAutoResearchRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if !link.WorkerNodeID.Valid || link.WorkerNodeID.String == "" {
		return nil, errors.New("autoresearch.runner_node_missing")
	}
	return workload.New(m.env.Nodes, m.env.Commands, link.WorkerNodeID.String, "", runID, 0), nil
}

func acknowledge(result *agentv1.CommandResult, operation string) error {
	if result == nil || !result.GetOk() {
		message := "missing acknowledgement"
		if result != nil && result.GetError() != "" {
			message = result.GetError()
		}
		return fmt.Errorf("%s failed: %s", operation, message)
	}
	return nil
}

func (m *Module) PauseAutoResearchRun(w http.ResponseWriter, r *http.Request, runID AutoResearchRunId) {
	run, err := m.env.Runs.Get(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if run.State != string(runsvc.Running) {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.run_not_running", "run must be running before pause")
		return
	}
	client, err := m.runWorkloadClient(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	inspected, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, 15*time.Second)
	if err != nil || acknowledge(inspected, "inspect") != nil || inspected.GetContainerState() != "running" {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.container_not_running", "managed run container is not running")
		return
	}
	paused, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_PAUSE, nil, 15*time.Second)
	if err != nil || acknowledge(paused, "pause") != nil || paused.GetContainerState() != "paused" {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.pause_failed", "agent did not acknowledge paused state")
		return
	}
	if err := m.env.Runs.SetState(r.Context(), runID.String(), runsvc.Paused, "", ""); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, _ = m.env.Runs.Get(r.Context(), runID.String())
	httpx.WriteJSON(w, http.StatusOK, run)
}

func (m *Module) ResumeAutoResearchRun(w http.ResponseWriter, r *http.Request, runID AutoResearchRunId) {
	run, err := m.env.Runs.Get(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if run.State != string(runsvc.Paused) {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.run_not_paused", "run must be paused before resume")
		return
	}
	client, err := m.runWorkloadClient(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	inspected, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, 15*time.Second)
	if err != nil || acknowledge(inspected, "inspect") != nil || inspected.GetContainerState() != "paused" {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.container_not_paused", "managed run container is not paused")
		return
	}
	resumed, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_UNPAUSE, nil, 15*time.Second)
	if err != nil || acknowledge(resumed, "unpause") != nil || resumed.GetContainerState() != "running" {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.resume_failed", "agent did not acknowledge running state")
		return
	}
	if err := m.env.Runs.SetState(r.Context(), runID.String(), runsvc.Running, "", ""); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, _ = m.env.Runs.Get(r.Context(), runID.String())
	httpx.WriteJSON(w, http.StatusOK, run)
}

func (m *Module) StopAutoResearchRun(w http.ResponseWriter, r *http.Request, runID AutoResearchRunId) {
	client, err := m.runWorkloadClient(r.Context(), runID.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "AutoResearch run not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	inspected, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, 15*time.Second)
	if err != nil || acknowledge(inspected, "inspect") != nil {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.container_missing", "managed run container could not be inspected")
		return
	}
	if inspected.GetContainerState() == "paused" {
		resumed, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_UNPAUSE, nil, 15*time.Second)
		if err != nil || acknowledge(resumed, "unpause") != nil {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.resume_failed", "paused container could not be resumed for shutdown")
			return
		}
	}
	stopped, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, 30*time.Second)
	if err != nil || acknowledge(stopped, "stop") != nil {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.stop_failed", "agent did not acknowledge stopped state")
		return
	}
	if err := m.env.Runs.Cancel(r.Context(), runID.String()); err != nil && !errors.Is(err, runsvc.ErrInvalidTransition) {
		httpx.HandleErr(w, err)
		return
	}
	drainCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	_, drainErr := m.env.Runs.WaitLogEnd(drainCtx, runID.String(), "", 0, "stdout")
	cancel()
	if drainErr != nil {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.log_drain_failed", drainErr.Error())
		return
	}
	removed, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, 30*time.Second)
	if err != nil || acknowledge(removed, "remove") != nil {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.remove_failed", "agent did not acknowledge container removal")
		return
	}
	run, err := m.env.Runs.Get(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, run)
}

func configSnapshot(value string) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal([]byte(value), &out)
	return out
}
