package backend

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/id"
)

var _ ServerInterface = (*Module)(nil)

func defaultProjectConfig() AutoResearchProjectConfig {
	candidateCount := 1
	maxRounds := 5
	gates := map[string]bool{
		"idea_selection":      true,
		"paper_post_edit":     true,
		"experiment_handback": false,
		"claim_scope_change":  false,
		"citation_change":     false,
	}
	roles := map[string]AutoResearchProviderRef{}
	advisors := map[string]AutoResearchAdvisorConfig{}
	fallbacks := map[string][]AutoResearchProviderRef{}
	prompts := map[string]string{}
	return AutoResearchProjectConfig{
		CandidateCount: &candidateCount,
		PaperMaxRounds: &maxRounds,
		HumanGates:     &gates,
		Roles:          &roles,
		Advisors:       &advisors,
		Fallbacks:      &fallbacks,
		AuditorPrompts: &prompts,
	}
}

func parseDBTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func uuidValue(value string) (uuid.UUID, error) { return uuid.Parse(value) }

func nullableUUID(value *uuid.UUID) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: value.String(), Valid: true}
}

func ptrString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	copy := value.String
	return &copy
}

func (m *Module) projectView(row db.AutoresearchProject) (AutoResearchProject, error) {
	projectID, err := uuidValue(row.ID)
	if err != nil {
		return AutoResearchProject{}, err
	}
	config := defaultProjectConfig()
	if err := json.Unmarshal([]byte(row.ConfigJson), &config); err != nil {
		return AutoResearchProject{}, fmt.Errorf("decode project config: %w", err)
	}
	var runnerID *uuid.UUID
	if row.RunnerNodeID.Valid {
		parsed, err := uuidValue(row.RunnerNodeID.String)
		if err != nil {
			return AutoResearchProject{}, err
		}
		runnerID = &parsed
	}
	return AutoResearchProject{
		Id: projectID, Name: row.Name, Status: AutoResearchProjectStatus(row.Status),
		RunnerNodeId: runnerID, IdeaPrompt: row.IdeaPrompt, Config: config,
		Version: int(row.Version), CreatedAt: parseDBTime(row.CreatedAt), UpdatedAt: parseDBTime(row.UpdatedAt),
	}, nil
}

func (m *Module) ideaView(row db.AutoresearchIdea) (AutoResearchIdea, error) {
	ideaID, err := uuidValue(row.ID)
	if err != nil {
		return AutoResearchIdea{}, err
	}
	projectID, err := uuidValue(row.ProjectID)
	if err != nil {
		return AutoResearchIdea{}, err
	}
	return AutoResearchIdea{
		Id: ideaID, ProjectId: projectID, Ordinal: int(row.Ordinal), Source: AutoResearchIdeaSource(row.Source),
		Title: row.Title, Body: row.Body, Selected: row.Selected == 1, Version: int(row.Version),
		CreatedAt: parseDBTime(row.CreatedAt), UpdatedAt: parseDBTime(row.UpdatedAt),
	}, nil
}

func (m *Module) projectRoot(projectID string) string {
	return filepath.Join(m.env.AutoResearchRoot, projectID)
}

func (m *Module) ensureProjectRoot(projectID string) error {
	return initializeProjectRoot(m.projectRoot(projectID))
}

func parseVersionETag(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "W/")
	trimmed = strings.Trim(trimmed, `"`)
	version, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || version < 1 {
		return 0, errors.New("If-Match must contain a positive project version")
	}
	return version, nil
}

func setVersionETag(w http.ResponseWriter, version int) {
	w.Header().Set("ETag", fmt.Sprintf(`"%d"`, version))
}

func (m *Module) ListAutoResearchProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := m.env.Q.ListAutoResearchProjects(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]AutoResearchProject, 0, len(rows))
	for _, row := range rows {
		view, err := m.projectView(row)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		out = append(out, view)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func initialIdeaTitle(question string) string {
	title := strings.TrimSpace(strings.SplitN(question, "\n", 2)[0])
	runes := []rune(title)
	if len(runes) > 500 {
		title = string(runes[:500])
	}
	return title
}

func (m *Module) CreateAutoResearchProject(w http.ResponseWriter, r *http.Request) {
	var req AutoResearchProjectCreate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	question := strings.TrimSpace(req.IdeaPrompt)
	if question == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "research question is required")
		return
	}
	projectID, err := id.New()
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	config := defaultProjectConfig()
	if req.Config != nil {
		config = *req.Config
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if err := m.env.Q.CreateAutoResearchProject(r.Context(), db.CreateAutoResearchProjectParams{
		ID: projectID, Name: name, Status: "idea_intake", RunnerNodeID: nullableUUID(req.RunnerNodeId),
		IdeaPrompt: question, ConfigJson: string(configJSON),
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if err := m.ensureProjectRoot(projectID); err != nil {
		_, _ = m.env.DB.ExecContext(r.Context(), "DELETE FROM autoresearch_projects WHERE id = ?", projectID)
		httpx.HandleErr(w, err)
		return
	}
	ideaID, err := id.New()
	if err == nil {
		err = m.env.Q.CreateAutoResearchIdea(r.Context(), db.CreateAutoResearchIdeaParams{
			ID: ideaID, ProjectID: projectID, Ordinal: 1, Source: "human",
			Title: initialIdeaTitle(question), Body: question, Selected: 1,
		})
	}
	if err == nil {
		var idea db.AutoresearchIdea
		idea, err = m.env.Q.GetAutoResearchIdea(r.Context(), db.GetAutoResearchIdeaParams{ID: ideaID, ProjectID: projectID})
		if err == nil {
			err = adoptIdea(m.projectRoot(projectID), idea)
		}
	}
	if err != nil {
		_, _ = m.env.DB.ExecContext(r.Context(), "DELETE FROM autoresearch_projects WHERE id = ?", projectID)
		httpx.HandleErr(w, err)
		return
	}
	row, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view, err := m.projectView(row)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	setVersionETag(w, view.Version)
	httpx.WriteJSON(w, http.StatusCreated, view)
}

func (m *Module) GetAutoResearchProject(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	row, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	view, err := m.projectView(row)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	setVersionETag(w, view.Version)
	httpx.WriteJSON(w, http.StatusOK, view)
}

func (m *Module) UpdateAutoResearchProject(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, params UpdateAutoResearchProjectParams) {
	version, err := parseVersionETag(params.IfMatch)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	row, err := m.env.Q.GetAutoResearchProject(r.Context(), projectID.String())
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "project not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	var req AutoResearchProjectPatch
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	if req.Name != nil {
		row.Name = strings.TrimSpace(*req.Name)
		if row.Name == "" {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "name cannot be empty")
			return
		}
	}
	if req.IdeaPrompt != nil {
		row.IdeaPrompt = *req.IdeaPrompt
	}
	if req.RunnerNodeId != nil {
		row.RunnerNodeID = nullableUUID(req.RunnerNodeId)
	}
	if req.Config != nil {
		encoded, err := json.Marshal(req.Config)
		if err != nil {
			httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
			return
		}
		row.ConfigJson = string(encoded)
	}
	updated, err := m.env.Q.UpdateAutoResearchProject(r.Context(), db.UpdateAutoResearchProjectParams{
		Name: row.Name, Status: row.Status, RunnerNodeID: row.RunnerNodeID, IdeaPrompt: row.IdeaPrompt,
		ConfigJson: row.ConfigJson, ID: row.ID, Version: version,
	})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if updated == 0 {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.edit_conflict", "project changed since it was read")
		return
	}
	m.GetAutoResearchProject(w, r, projectID)
}

func (m *Module) ListAutoResearchIdeas(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId) {
	rows, err := m.env.Q.ListAutoResearchIdeas(r.Context(), projectID.String())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]AutoResearchIdea, 0, len(rows))
	for _, row := range rows {
		view, err := m.ideaView(row)
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		out = append(out, view)
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) UpdateAutoResearchIdea(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, ideaID AutoResearchIdeaId, params UpdateAutoResearchIdeaParams) {
	version, err := parseVersionETag(params.IfMatch)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	var req AutoResearchIdeaUpdate
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" || strings.TrimSpace(req.Body) == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "title and body are required")
		return
	}
	updated, err := m.env.Q.UpdateAutoResearchIdea(r.Context(), db.UpdateAutoResearchIdeaParams{
		Title: req.Title, Body: req.Body, ID: ideaID.String(), ProjectID: projectID.String(), Version: version,
	})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if updated == 0 {
		httpx.WriteErr(w, http.StatusConflict, "autoresearch.edit_conflict", "idea changed since it was read")
		return
	}
	m.writeIdeaResponse(w, r, projectID, ideaID)
}

func (m *Module) SelectAutoResearchIdea(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, ideaID AutoResearchIdeaId) {
	row, err := m.env.Q.GetAutoResearchIdea(r.Context(), db.GetAutoResearchIdeaParams{ID: ideaID.String(), ProjectID: projectID.String()})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "idea not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if row.Selected == 0 {
		updated, err := m.env.Q.SelectAutoResearchIdea(r.Context(), db.SelectAutoResearchIdeaParams{ID: row.ID, ProjectID: row.ProjectID, Version: row.Version})
		if err != nil {
			httpx.HandleErr(w, err)
			return
		}
		if updated == 0 {
			httpx.WriteErr(w, http.StatusConflict, "autoresearch.edit_conflict", "idea changed since it was read")
			return
		}
		if err := adoptIdea(m.projectRoot(projectID.String()), row); err != nil {
			_, _ = m.env.DB.ExecContext(r.Context(), `UPDATE autoresearch_ideas SET selected=0, version=version+1,
				updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND project_id=?`, row.ID, row.ProjectID)
			httpx.HandleErr(w, err)
			return
		}
	}
	m.writeIdeaResponse(w, r, projectID, ideaID)
}

func (m *Module) writeIdeaResponse(w http.ResponseWriter, r *http.Request, projectID AutoResearchProjectId, ideaID AutoResearchIdeaId) {
	row, err := m.env.Q.GetAutoResearchIdea(r.Context(), db.GetAutoResearchIdeaParams{ID: ideaID.String(), ProjectID: projectID.String()})
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	view, err := m.ideaView(row)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	setVersionETag(w, view.Version)
	httpx.WriteJSON(w, http.StatusOK, view)
}
