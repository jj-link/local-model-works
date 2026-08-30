package backend

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

func (m *Module) submitFactoryRunContextMode(ctx context.Context, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string, chained bool) (string, error) {
	unlock := m.lockProject(project.ID)
	defer unlock()
	if owner := m.activeProjectOperation(ctx, project.ID); owner != "" && !chained {
		return "", fmt.Errorf("%w: project operation %s is active", jobs.ErrLeaseConflict, owner)
	}
	if err := m.ensureCredentialCleanup(); err != nil {
		return "", err
	}
	settings, err := m.requireWorkerSettings(ctx)
	if err != nil {
		return "", err
	}
	providerConfig, inherited, err := providerSnapshotFromInput(input)
	if err != nil {
		return "", err
	}
	if !inherited {
		providerConfig, err = effectiveProviderSnapshot(project.ConfigJson, settings, input["provider_overrides"])
		if err != nil {
			return "", err
		}
	}
	configSnapshotJSON, err := json.Marshal(providerConfig)
	if err != nil {
		return "", err
	}
	input["project_id"] = project.ID
	input["factory"] = factory
	input["provider_config"] = providerConfig
	var runID string
	if chained {
		runID, err = m.env.Jobs.SubmitChained(ctx, parentRunID, "autoresearch-factory", input)
	} else {
		runID, err = m.env.Jobs.Submit(ctx, "autoresearch-factory", input)
	}
	if err != nil {
		return "", err
	}
	if err := m.env.Q.CreateAutoResearchRun(ctx, db.CreateAutoResearchRunParams{
		RunID: runID, ProjectID: project.ID, Factory: factory, ParentRunID: nullString(parentRunID),
		WorkerNodeID: nullString(settings.RunnerNodeID), ConfigSnapshot: string(configSnapshotJSON),
	}); err != nil {
		_ = m.env.Runs.Cancel(ctx, runID)
		return "", err
	}
	if err := m.setProjectStatus(ctx, project.ID, "running"); err != nil {
		_ = m.env.Runs.Cancel(ctx, runID)
		return "", err
	}
	return runID, nil
}

func (m *Module) submitFactoryRunContext(ctx context.Context, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string) (string, error) {
	return m.submitFactoryRunContextMode(ctx, project, factory, input, parentRunID, false)
}

func (m *Module) submitFactoryRun(r *http.Request, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string) (string, error) {
	return m.submitFactoryRunContext(r.Context(), project, factory, input, parentRunID)
}

func writeAutoResearchSubmissionError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, jobs.ErrLeaseConflict) {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.project_busy", err.Error())
		return true
	}
	for _, code := range []string{"autoresearch.runner_not_configured", "autoresearch.runner_offline", "autoresearch.credential_cleanup_failed"} {
		if strings.HasPrefix(err.Error(), code) {
			httpx.WriteErr(w, http.StatusConflict, code, err.Error())
			return true
		}
	}
	return false
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
		if writeAutoResearchSubmissionError(w, err) {
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

func nextFactoryForRun(factory AutoResearchFactory, state string, input map[string]any) (AutoResearchFactory, error) {
	if state != string(runsvc.Succeeded) {
		return factory, nil
	}
	if factory == Idea {
		if _, intake := input["candidate_count"]; intake {
			return Idea, nil
		}
	}
	switch factory {
	case Idea:
		return Proposal, nil
	case Proposal:
		return DeepLit, nil
	case DeepLit:
		return Experiment, nil
	case Experiment:
		return Paper, nil
	case Paper:
		return Paper, nil
	default:
		return "", fmt.Errorf("autoresearch.factory_invalid: %s", factory)
	}
}

func (m *Module) resolveFactory(ctx context.Context, project db.AutoresearchProject, requested *AutoResearchFactory) (AutoResearchFactory, error) {
	if project.Status == "completed" {
		return "", errors.New("autoresearch.project_completed")
	}
	if requested != nil {
		return *requested, nil
	}
	links, err := m.env.Q.ListAutoResearchRuns(ctx, project.ID)
	if err != nil {
		return "", err
	}
	if len(links) == 0 {
		return Idea, nil
	}
	latest := links[0]
	run, err := m.env.Runs.Get(ctx, latest.RunID)
	if err != nil {
		return "", err
	}
	return nextFactoryForRun(AutoResearchFactory(latest.Factory), run.State, run.Input)
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
	factory, err := m.resolveFactory(r.Context(), project, req.Factory)
	if err != nil {
		if err.Error() == "autoresearch.project_completed" {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.project_completed", "completed projects cannot start another research run")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if factory == Idea {
		selected, err := m.env.Q.CountSelectedAutoResearchIdeas(r.Context(), project.ID)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		if selected != 1 {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.idea_selection_required", "select exactly one idea before continuing")
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
	runID, err := m.submitFactoryRun(r, project, string(factory), input, parent)
	if err != nil {
		if errors.Is(err, jobs.ErrInput) {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
			return
		}
		if writeAutoResearchSubmissionError(w, err) {
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
	run, err := m.env.Runs.Get(r.Context(), runID.String())
	if err != nil {
		if errors.Is(err, runsvc.ErrUnknown) || errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "AutoResearch run not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	state := runsvc.State(run.State)
	if state.Terminal() {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.run_terminal", "terminal runs cannot be stopped")
		return
	}
	link, err := m.env.Q.GetAutoResearchRun(r.Context(), runID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	switch state {
	case runsvc.Queued, runsvc.Planning, runsvc.Waiting, runsvc.Verifying:
		if err := m.env.Jobs.Cancel(r.Context(), runID.String()); err != nil && !errors.Is(err, runsvc.ErrInvalidTransition) {
			httpx.HandleErr(w, err)
			return
		}
	case runsvc.Running, runsvc.Paused:
		client, err := m.runWorkloadClient(r.Context(), runID.String())
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		inspected, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, 15*time.Second)
		if err != nil || acknowledge(inspected, "inspect") != nil {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.container_missing", "managed run container could not be inspected")
			return
		}
		if state == runsvc.Paused || inspected.GetContainerState() == "paused" {
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
		if err := m.env.Jobs.Cancel(r.Context(), runID.String()); err != nil && !errors.Is(err, runsvc.ErrInvalidTransition) {
			httpx.HandleErr(w, err)
			return
		}
		drainCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		_, _ = m.env.Runs.WaitLogEnd(drainCtx, runID.String(), "", 0, "stdout")
		cancel()
		removed, err := client.Do(r.Context(), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, 30*time.Second)
		if err != nil || acknowledge(removed, "remove") != nil {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.remove_failed", "agent did not acknowledge container removal")
			return
		}
	default:
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.run_not_stoppable", "run is already cancelling")
		return
	}
	if err := m.setProjectStatus(r.Context(), link.ProjectID, "failed"); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	run, err = m.env.Runs.Get(r.Context(), runID.String())
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
