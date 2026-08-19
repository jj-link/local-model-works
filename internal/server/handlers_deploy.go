package server

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/deploy"
)
// handleDeployErr maps deploy sentinels to the stable (status, code) pairs
// the OpenAPI contract declares for /deployments*.
func (s *Server) handleDeployErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, deploy.ErrUnknown):
		writeErr(w, http.StatusNotFound, "resource.not_found", err.Error())
	case errors.Is(err, deploy.ErrRecipe):
		writeErr(w, http.StatusNotFound, "recipe.not_found", err.Error())
	case errors.Is(err, deploy.ErrProfile), errors.Is(err, deploy.ErrNoTarget):
		writeErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
	case errors.Is(err, deploy.ErrPlanStale):
		writeErr(w, http.StatusConflict, "plan.stale", err.Error())
	case errors.Is(err, deploy.ErrNotReady), errors.Is(err, deploy.ErrConflict), errors.Is(err, deploy.ErrState):
		writeErr(w, http.StatusConflict, "resource.conflict", err.Error())
	default:
		handleErr(w, err)
	}
}

// handlePlanDeployment previews a deployment: placement, artifacts, ports,
// risks, conflicts.
func (s *Server) handlePlanDeployment(w http.ResponseWriter, r *http.Request) {
	var req deploy.PlanRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	plan, err := s.deploys.Plan(r.Context(), req)
	if err != nil {
		s.handleDeployErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleCreateDeployment validates the plan digest, commits the deployment
// with its resource leases, and starts dispatch.
func (s *Server) handleCreateDeployment(w http.ResponseWriter, r *http.Request) {
	var req deploy.CreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
		return
	}
	dep, err := s.deploys.Create(r.Context(), req)
	if err != nil {
		s.handleDeployErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, dep)
}

// handleListDeployments returns all deployments.
func (s *Server) handleListDeployments(w http.ResponseWriter, r *http.Request) {
	deps, err := s.deploys.List(r.Context())
	if err != nil {
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deps)
}

// handleGetDeployment returns one deployment.
func (s *Server) handleGetDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.deploys.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.handleDeployErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// handleStopDeployment stops a running deployment. Offline ranks keep their
// leases and are re-driven on reconnect.
func (s *Server) handleStopDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.deploys.Stop(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.handleDeployErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}

// handleVerifyDeployment runs the workload probe against the live endpoint.
func (s *Server) handleVerifyDeployment(w http.ResponseWriter, r *http.Request) {
	dep, err := s.deploys.Verify(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.handleDeployErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dep)
}
