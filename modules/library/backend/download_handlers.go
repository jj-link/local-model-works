package backend

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/httpx"
)

func writeDownloadError(w http.ResponseWriter, err error) {
	var typed *downloads.Error
	if errors.As(err, &typed) {
		httpx.WriteErr(w, typed.Status, typed.Code, typed.Message)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		httpx.WriteErr(w, http.StatusNotFound, "download.not_found", "Recipe or download resource does not exist")
		return
	}
	httpx.HandleErr(w, err)
}
func (m *Module) planRecipeDownload(w http.ResponseWriter, r *http.Request, digest string) {
	var input downloads.PlanRequest
	if err := httpx.DecodeBody(r, &input); err != nil {
		httpx.WriteErr(w, 422, "download.request_invalid", err.Error())
		return
	}
	input.RecipeDigest = digest
	plan, err := m.env.Downloads.Plan(r.Context(), input)
	if err != nil {
		writeDownloadError(w, err)
		return
	}
	httpx.WriteJSON(w, 200, plan)
}
func (m *Module) startRecipeDownload(w http.ResponseWriter, r *http.Request, digest string) {
	var input downloads.CreateRequest
	if err := httpx.DecodeBody(r, &input); err != nil {
		httpx.WriteErr(w, 422, "download.request_invalid", err.Error())
		return
	}
	input.RecipeDigest = digest
	runID, err := m.env.Downloads.Submit(r.Context(), input)
	if err != nil {
		writeDownloadError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}
func (m *Module) listRecipeDownloads(w http.ResponseWriter, r *http.Request, digest string) {
	attempts, err := m.env.Downloads.List(r.Context(), digest)
	if err != nil {
		writeDownloadError(w, err)
		return
	}
	httpx.WriteJSON(w, 200, attempts)
}
func (m *Module) resumeRecipeDownload(w http.ResponseWriter, r *http.Request, digest, targetRunID string) {
	var input downloads.ResumeRequest
	if err := httpx.DecodeBody(r, &input); err != nil {
		httpx.WriteErr(w, 422, "download.request_invalid", err.Error())
		return
	}
	input.RecipeDigest = digest
	runID, err := m.env.Downloads.Resume(r.Context(), targetRunID, input)
	if err != nil {
		writeDownloadError(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}
func (m *Module) getRecipeAvailability(w http.ResponseWriter, r *http.Request, digest string) {
	var input downloads.AvailabilityRequest
	if err := httpx.DecodeBody(r, &input); err != nil {
		httpx.WriteErr(w, 422, "download.request_invalid", err.Error())
		return
	}
	input.RecipeDigest = digest
	availability, err := m.env.Downloads.Availability(r.Context(), input)
	if err != nil {
		writeDownloadError(w, err)
		return
	}
	httpx.WriteJSON(w, 200, availability)
}
