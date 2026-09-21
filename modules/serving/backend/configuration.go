package backend

import (
	"net/http"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/httpx"
)

func (m *Module) PlanDeploymentConfiguration(w http.ResponseWriter, r *http.Request, id string) {
	var request deploy.RepositoryReplacementSettings
	if err := httpx.DecodeBody(r, &request); err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	plan, err := m.env.Deploy.PlanDeploymentConfiguration(r.Context(), id, request)
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, plan)
}

func (m *Module) ApplyDeploymentConfiguration(w http.ResponseWriter, r *http.Request, id string) {
	var request struct {
		deploy.RepositoryReplacementSettings
		PlanDigest string `json:"plan_digest"`
	}
	if err := httpx.DecodeBody(r, &request); err != nil || request.PlanDigest == "" {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "launch settings and plan_digest are required")
		return
	}
	runID, err := m.env.Deploy.CreateDeploymentConfiguration(r.Context(), id, request.RepositoryReplacementSettings, request.PlanDigest)
	if err != nil {
		m.deployErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}
