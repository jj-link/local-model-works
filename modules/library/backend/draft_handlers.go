package backend

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/httpx"
)

func (m *Module) createDraft(response http.ResponseWriter, request *http.Request) {
	var input map[string]any
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	runID, err := m.env.Jobs.Submit(request.Context(), "recipe-draft", input)
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"run_id": runID})
}

func (m *Module) listDrafts(response http.ResponseWriter, request *http.Request) {
	drafts, err := m.env.RecipeBuilder.List(request.Context())
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, drafts)
}

func (m *Module) getDraft(response http.ResponseWriter, request *http.Request) {
	draft, err := m.env.RecipeBuilder.Get(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, draft)
}

type updateDraftRequest struct {
	Manifest       json.RawMessage `json:"manifest"`
	SelectedAssets []string        `json:"selected_assets"`
}

func (m *Module) updateDraft(response http.ResponseWriter, request *http.Request) {
	versionText := strings.Trim(request.Header.Get("If-Match"), `"`)
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		httpx.WriteErr(response, http.StatusPreconditionRequired, "recipe.draft_if_match", "If-Match draft version is required")
		return
	}
	var input updateDraftRequest
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.Update(request.Context(), chi.URLParam(request, "id"), version, input.Manifest, input.SelectedAssets)
	if err != nil {
		if strings.Contains(err.Error(), "version_conflict") {
			httpx.WriteErr(response, http.StatusPreconditionFailed, "recipe.draft_version_conflict", err.Error())
			return
		}
		httpx.HandleErr(response, err)
		return
	}
	response.Header().Set("ETag", `"`+strconv.FormatInt(draft.Version, 10)+`"`)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) packageDraft(response http.ResponseWriter, request *http.Request) {
	draft, err := m.env.RecipeBuilder.Package(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) installDraft(response http.ResponseWriter, request *http.Request) {
	installed, err := m.env.RecipeBuilder.Install(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, installed)
}

func (m *Module) deleteDraft(response http.ResponseWriter, request *http.Request) {
	if err := m.env.RecipeBuilder.Delete(request.Context(), chi.URLParam(request, "id")); err != nil {
		httpx.HandleErr(response, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}
