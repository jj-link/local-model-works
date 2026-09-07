package backend

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
)

func (m *Module) createRecipeChange(response http.ResponseWriter, request *http.Request, digest string) {
	var input recipebuilder.ChangeRequest
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_change_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.AllocateChange(request.Context(), digest, input)
	if err != nil {
		writeDraftError(response, err, "")
		return
	}
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draft.ID, draft.Version, recipebuilder.PhaseInspect)
	if err != nil {
		writeDraftError(response, err, draft.ID)
		return
	}
	runID, err := m.submitDraftOperation(request, "recipe-change", reserved)
	if err != nil {
		writeSubmitError(response, draft.ID, err)
		return
	}
	setDraftETag(response, reserved.Version)
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"draft_id": draft.ID, "run_id": runID})
}

func (m *Module) inspectRecipeDraft(response http.ResponseWriter, request *http.Request) {
	m.reserveAndSubmit(response, request, recipebuilder.PhaseInspect, "recipe-draft")
}

func (m *Module) compareRecipeDraft(response http.ResponseWriter, request *http.Request) {
	draftID := chi.URLParam(request, "id")
	comparison, err := m.env.RecipeBuilder.Compare(request.Context(), draftID)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, comparison)
}

func (m *Module) changeJob(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	input, err := decodeDraftOperationInput(job.Input)
	if err != nil {
		return nil, err
	}
	draft, err := m.env.RecipeBuilder.PrepareChange(ctx, input.DraftID, input.OperationID, job.RunID, m.draftProgress(ctx, job, input))
	if err != nil {
		_, _ = m.env.RecipeBuilder.FailOperation(ctx, input.DraftID, input.OperationID, err)
		return nil, err
	}
	return draftJobOutput(draft, nil), nil
}
