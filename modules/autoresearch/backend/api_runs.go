package backend

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
)

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (m *Module) submitFactoryRun(r *http.Request, project db.AutoresearchProject, factory string, input map[string]any, parentRunID string) (string, error) {
	input["project_id"] = project.ID
	input["factory"] = factory
	runID, err := m.env.Jobs.Submit(r.Context(), "autoresearch-factory", input)
	if err != nil {
		return "", err
	}
	if err := m.env.Q.CreateAutoResearchRun(r.Context(), db.CreateAutoResearchRunParams{
		RunID: runID, ProjectID: project.ID, Factory: factory, ParentRunID: nullString(parentRunID),
		WorkerNodeID: project.RunnerNodeID, ConfigSnapshot: project.ConfigJson,
	}); err != nil {
		_ = m.env.Runs.Cancel(r.Context(), runID)
		return "", err
	}
	_, err = m.env.DB.ExecContext(r.Context(), `UPDATE autoresearch_projects
		SET status='running', version=version+1, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=?`, project.ID)
	if err != nil {
		_ = m.env.Runs.Cancel(r.Context(), runID)
		return "", err
	}
	return runID, nil
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
	runs := make([]any, 0, len(links))
	for _, link := range links {
		run, err := m.env.Runs.Get(r.Context(), link.RunID)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		runs = append(runs, run)
	}
	httpx.WriteJSON(w, http.StatusOK, runs)
}

func (m *Module) PauseAutoResearchRun(w http.ResponseWriter, _ *http.Request, _ AutoResearchRunId) {
	httpx.WriteErr(w, http.StatusConflict, "autoresearch.pause_unavailable", "managed runner pause is not available")
}

func (m *Module) ResumeAutoResearchRun(w http.ResponseWriter, _ *http.Request, _ AutoResearchRunId) {
	httpx.WriteErr(w, http.StatusConflict, "autoresearch.resume_unavailable", "managed runner resume is not available")
}

func (m *Module) StopAutoResearchRun(w http.ResponseWriter, r *http.Request, runID AutoResearchRunId) {
	if _, err := m.env.Q.GetAutoResearchRun(r.Context(), runID.String()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "AutoResearch run not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if err := m.env.Runs.Cancel(r.Context(), runID.String()); err != nil {
		httpx.HandleErr(w, err)
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
