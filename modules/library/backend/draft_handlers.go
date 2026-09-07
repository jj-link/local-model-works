package backend

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	recipeassistant "github.com/jj-link/local-model-works/internal/recipebuilder/assistant"
)

func (m *Module) createDraft(response http.ResponseWriter, request *http.Request) {
	var source recipebuilder.GitSource
	if err := httpx.DecodeBody(request, &source); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.Allocate(request.Context(), source)
	if err != nil {
		writeDraftError(response, err, "")
		return
	}
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draft.ID, draft.Version, recipebuilder.PhaseInspect)
	if err != nil {
		writeDraftError(response, err, draft.ID)
		return
	}
	runID, err := m.submitDraftOperation(request, "recipe-draft", reserved)
	if err != nil {
		writeSubmitError(response, draft.ID, err)
		return
	}
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{
		"draft_id": draft.ID, "run_id": runID,
	})
}

func (m *Module) listDrafts(response http.ResponseWriter, request *http.Request) {
	if repositoryID := request.URL.Query().Get("repository_id"); repositoryID != "" {
		if request.URL.Query().Get("package_digest") != "" {
			httpx.WriteErr(response, 422, "recipe.draft_invalid", "choose either repository_id or package_digest")
			return
		}
		drafts, err := m.env.RecipeBuilder.ListByRepository(request.Context(), repositoryID)
		if err != nil {
			writeDraftError(response, err, "")
			return
		}
		httpx.WriteJSON(response, http.StatusOK, drafts)
		return
	}
	if digest := request.URL.Query().Get("package_digest"); digest != "" {
		drafts, err := m.env.RecipeBuilder.ListByPackageDigest(request.Context(), digest)
		if err != nil {
			writeDraftError(response, err, "")
			return
		}
		httpx.WriteJSON(response, http.StatusOK, drafts)
		return
	}
	drafts, err := m.env.RecipeBuilder.List(request.Context())
	if err != nil {
		writeDraftError(response, err, "")
		return
	}
	httpx.WriteJSON(response, http.StatusOK, drafts)
}

func (m *Module) getDraft(response http.ResponseWriter, request *http.Request) {
	draft, err := m.env.RecipeBuilder.Get(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}
func (m *Module) getDraftRunExcerpt(response http.ResponseWriter, request *http.Request) {
	draft, err := m.env.RecipeBuilder.Get(request.Context(), chi.URLParam(request, "id"))
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	if draft.RunID == "" {
		httpx.WriteJSON(response, http.StatusOK, map[string]any{
			"run_id": "", "stdout": "", "stderr": "", "truncated": false,
		})
		return
	}
	const streamLimit = recipebuilder.MaxRunExcerptBytes / 2
	stdout, stdoutTruncated, err := m.readRunTail(draft.RunID, "stdout", streamLimit)
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	stderr, stderrTruncated, err := m.readRunTail(draft.RunID, "stderr", streamLimit)
	if err != nil {
		httpx.HandleErr(response, err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, map[string]any{
		"run_id": draft.RunID, "stdout": recipebuilder.RedactRunExcerpt(stdout),
		"stderr": recipebuilder.RedactRunExcerpt(stderr), "truncated": stdoutTruncated || stderrTruncated,
	})
}

func (m *Module) readRunTail(runID, stream string, limit int) ([]byte, bool, error) {
	chunk, _, size, err := m.env.Runs.ReadLog(runID, "", 0, stream, 0, limit)
	if err != nil || size <= uint64(limit) {
		return chunk, false, err
	}
	chunk, _, _, err = m.env.Runs.ReadLog(runID, "", 0, stream, size-uint64(limit), limit)
	return chunk, true, err
}

func (m *Module) updateDraft(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input recipebuilder.UpdateRequest
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.Update(request.Context(), chi.URLParam(request, "id"), version, input)
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) updateDraftContext(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input struct {
		Files []recipebuilder.ContextFile `json:"files"`
	}
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.SetContext(request.Context(), chi.URLParam(request, "id"), version, input.Files)
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) dismissDraftDiagnostics(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input struct {
		DiagnosticIDs []string `json:"diagnostic_ids"`
	}
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.diagnostic_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.DismissDiagnostics(request.Context(), chi.URLParam(request, "id"), version, input.DiagnosticIDs)
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) getDraftFile(response http.ResponseWriter, request *http.Request) {
	name := request.URL.Query().Get("path")
	candidate, content, err := m.env.RecipeBuilder.ReadOwnedFile(request.Context(), chi.URLParam(request, "id"), name, request.URL.Query().Get("sha256"), request.URL.Query().Get("source_commit"))
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	if candidate.Binary {
		httpx.WriteErr(response, http.StatusUnsupportedMediaType, "recipe.draft_file_binary", "binary draft files cannot be opened in the text editor")
		return
	}
	httpx.WriteJSON(response, http.StatusOK, map[string]any{
		"path": candidate.Path, "sha256": candidate.SHA256, "origin": candidate.Origin, "content": string(content),
	})
}

func (m *Module) updateDraftFile(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_file_invalid", err.Error())
		return
	}
	draft, err := m.env.RecipeBuilder.UpdateGeneratedFile(request.Context(), chi.URLParam(request, "id"), version, input.Path, input.Content)
	if err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) generateDraft(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input generationSelection
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.draft_invalid", err.Error())
		return
	}
	draftID := chi.URLParam(request, "id")
	if !input.ContentConsent || input.PreviewSHA256 == "" {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.generation_consent_required", "review the exact content and explicitly consent before generation")
		return
	}
	approval, provider, err := m.prepareGeneration(request.Context(), draftID, version, input)
	if err != nil {
		var changed *recipebuilder.Error
		if errors.As(err, &changed) && (changed.Code == "recipe.draft_source_changed" || changed.Code == "recipe.draft_file_unavailable") {
			err = consentStale("approved source bytes changed or are no longer available")
		}
		writeDraftError(response, err, draftID)
		return
	}
	if approval.PreviewSHA256 != input.PreviewSHA256 {
		writeDraftError(response, consentStale("draft, destination, provider settings, or selected log bytes changed after preview"), draftID)
		return
	}
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draftID, version, recipebuilder.PhaseGenerate)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	runID, err := m.submitDraftOperationInput(request, "recipe-generate", reserved, map[string]any{
		"approval": approval, "provider": provider,
	})
	if err != nil {
		writeSubmitError(response, draftID, err)
		return
	}
	setDraftETag(response, reserved.Version)
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"draft_id": draftID, "run_id": runID})
}
func (m *Module) resolveDraftReferences(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input recipebuilder.ResolveRequest
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.reference_invalid", err.Error())
		return
	}
	if input.Credentials == nil {
		input.Credentials = []recipebuilder.ResolveCredential{}
	}
	if input.FileChecksums == nil {
		input.FileChecksums = []recipebuilder.FileChecksum{}
	}
	draftID := chi.URLParam(request, "id")
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draftID, version, recipebuilder.PhaseResolve)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	extra := map[string]any{"credentials": input.Credentials, "file_checksums": input.FileChecksums}
	runID, err := m.submitDraftOperationInput(request, "recipe-resolve", reserved, extra)
	if err != nil {
		writeSubmitError(response, draftID, err)
		return
	}
	setDraftETag(response, reserved.Version)
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"draft_id": draftID, "run_id": runID})
}

func (m *Module) acceptDraftProposal(response http.ResponseWriter, request *http.Request) {
	m.reviewDraftProposal(response, request, true)
}

func (m *Module) discardDraftProposal(response http.ResponseWriter, request *http.Request) {
	m.reviewDraftProposal(response, request, false)
}

func (m *Module) reviewDraftProposal(response http.ResponseWriter, request *http.Request, accept bool) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.proposal_invalid", err.Error())
		return
	}
	draftID := chi.URLParam(request, "id")
	var draft *recipebuilder.Draft
	var err error
	if accept {
		draft, err = m.env.RecipeBuilder.AcceptProposal(request.Context(), draftID, version, input.ProposalID)
	} else {
		draft, err = m.env.RecipeBuilder.DiscardProposal(request.Context(), draftID, version, input.ProposalID)
	}
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	setDraftETag(response, draft.Version)
	httpx.WriteJSON(response, http.StatusOK, draft)
}

func (m *Module) packageDraft(response http.ResponseWriter, request *http.Request) {
	m.reserveAndSubmit(response, request, recipebuilder.PhasePackage, "recipe-package")
}

func (m *Module) installDraft(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	var input struct {
		PackageDigest        string   `json:"package_digest"`
		AcknowledgedWarnings []string `json:"acknowledged_warnings"`
	}
	if err := httpx.DecodeBody(request, &input); err != nil {
		httpx.WriteErr(response, http.StatusUnprocessableEntity, "recipe.install_invalid", err.Error())
		return
	}
	draftID := chi.URLParam(request, "id")
	draft, err := m.env.RecipeBuilder.Get(request.Context(), draftID)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	if draft.PackageDigest == "" || input.PackageDigest != draft.PackageDigest {
		httpx.WriteErr(response, http.StatusConflict, "recipe.install_digest_mismatch", "package digest does not match the reviewed draft package")
		return
	}
	if !sameStrings(input.AcknowledgedWarnings, draft.AcknowledgedWarnings) {
		httpx.WriteErr(response, http.StatusConflict, "recipe.install_warning_consent_mismatch", "warning consent does not match the reviewed draft version")
		return
	}
	if err := m.env.RecipeBuilder.CheckSaveBaseline(request.Context(), draft.ID); err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draftID, version, recipebuilder.PhaseInstall)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	runID, err := m.submitDraftOperationInput(request, "recipe-install", reserved, map[string]any{
		"package_digest": input.PackageDigest, "acknowledged_warnings": input.AcknowledgedWarnings,
	})
	if err != nil {
		writeSubmitError(response, draftID, err)
		return
	}
	setDraftETag(response, reserved.Version)
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{"draft_id": draftID, "run_id": runID})
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]bool, len(left))
	for _, value := range left {
		values[value] = true
	}
	for _, value := range right {
		if !values[value] {
			return false
		}
	}
	return true
}

func (m *Module) reserveAndSubmit(response http.ResponseWriter, request *http.Request, operationKind, jobKind string) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	draftID := chi.URLParam(request, "id")
	reserved, err := m.env.RecipeBuilder.ReserveOperation(request.Context(), draftID, version, operationKind)
	if err != nil {
		writeDraftError(response, err, draftID)
		return
	}
	runID, err := m.submitDraftOperation(request, jobKind, reserved)
	if err != nil {
		writeSubmitError(response, draftID, err)
		return
	}
	setDraftETag(response, reserved.Version)
	httpx.WriteJSON(response, http.StatusAccepted, map[string]string{
		"draft_id": draftID, "run_id": runID,
	})
}

func (m *Module) submitDraftOperation(request *http.Request, jobKind string, draft *recipebuilder.Draft) (string, error) {
	return m.submitDraftOperationInput(request, jobKind, draft, nil)
}

func (m *Module) submitDraftOperationInput(request *http.Request, jobKind string, draft *recipebuilder.Draft, extra map[string]any) (string, error) {
	op := draft.Operation
	if op == nil {
		return "", errors.New("reserved draft has no operation")
	}
	input := map[string]any{"draft_id": draft.ID, "operation_id": op.ID}
	for key, value := range extra {
		input[key] = value
	}
	runID, err := m.env.Jobs.Submit(request.Context(), jobKind, input)
	if err != nil {
		_, _ = m.env.RecipeBuilder.FailOperation(request.Context(), draft.ID, op.ID,
			&recipebuilder.Error{Code: "recipe.operation_submit_failed", Message: err.Error(), Retryable: true})
		return "", err
	}
	return runID, nil
}

func (m *Module) deleteDraft(response http.ResponseWriter, request *http.Request) {
	version, ok := requireDraftVersion(response, request)
	if !ok {
		return
	}
	if err := m.env.RecipeBuilder.Delete(request.Context(), chi.URLParam(request, "id"), version); err != nil {
		writeDraftError(response, err, chi.URLParam(request, "id"))
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func requireDraftVersion(response http.ResponseWriter, request *http.Request) (int64, bool) {
	versionText := strings.Trim(request.Header.Get("If-Match"), `"`)
	version, err := strconv.ParseInt(versionText, 10, 64)
	if err != nil {
		httpx.WriteErr(response, http.StatusPreconditionRequired, "recipe.draft_if_match", "If-Match draft version is required")
		return 0, false
	}
	return version, true
}

func setDraftETag(response http.ResponseWriter, version int64) {
	response.Header().Set("ETag", `"`+strconv.FormatInt(version, 10)+`"`)
}

func writeSubmitError(response http.ResponseWriter, draftID string, err error) {
	httpx.WriteJSON(response, http.StatusServiceUnavailable, httpx.Error{
		Code: "recipe.operation_submit_failed", Message: err.Error(),
		Details: map[string]any{"draft_id": draftID, "retryable": true},
	})
}

func writeDraftError(response http.ResponseWriter, err error, draftID string) {
	var packError *recipe.PackError
	if errors.As(err, &packError) || errors.Is(err, recipe.ErrUnknown) {
		writeRecipeUpdateError(response, err)
		return
	}
	var assistantError *recipeassistant.Error
	if errors.As(err, &assistantError) {
		writeAssistantError(response, err)
		return
	}
	var typed *recipebuilder.Error
	if !errors.As(err, &typed) {
		httpx.HandleErr(response, err)
		return
	}
	status := http.StatusInternalServerError
	switch typed.Code {
	case "recipe.draft_unknown":
		status = http.StatusNotFound
	case "recipe.draft_stale_version", "recipe.generation_consent_stale", "recipe.proposal_stale":
		status = http.StatusPreconditionFailed
	case "recipe.draft_operation_active", "recipe.draft_operation_conflict", "recipe.draft_operation_lost",
		"recipe.draft_immutable":
		status = http.StatusConflict
	default:
		if strings.HasPrefix(typed.Code, "recipe.source_") || strings.HasPrefix(typed.Code, "recipe.draft_") ||
			strings.HasPrefix(typed.Code, "recipe.question_") || strings.HasPrefix(typed.Code, "recipe.warning_") {
			status = http.StatusUnprocessableEntity
		}
	}
	details := map[string]any{
		"draft_id": draftID, "diagnostics": typed.Diagnostics, "retryable": typed.Retryable,
	}
	httpx.WriteJSON(response, status, httpx.Error{Code: typed.Code, Message: typed.Message, Details: details})
}
