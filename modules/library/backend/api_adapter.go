package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListRecipeAssistantProviders(w http.ResponseWriter, r *http.Request) {
	m.listRecipeAssistantProviders(w, r)
}

func (m *Module) TestRecipeAssistantProvider(w http.ResponseWriter, r *http.Request, id ID) {
	m.testRecipeAssistantProvider(w, r, id)
}
func (m *Module) GetRecipeAssistantCodexStatus(w http.ResponseWriter, r *http.Request) {
	m.getRecipeAssistantCodexStatus(w, r)
}
func (m *Module) StartRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request) {
	m.startRecipeAssistantCodexLogin(w, r)
}
func (m *Module) GetRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request, _ ID) {
	m.getRecipeAssistantCodexLogin(w, r)
}
func (m *Module) CancelRecipeAssistantCodexLogin(w http.ResponseWriter, r *http.Request, _ ID) {
	m.cancelRecipeAssistantCodexLogin(w, r)
}
func (m *Module) LogoutRecipeAssistantCodex(w http.ResponseWriter, r *http.Request) {
	m.logoutRecipeAssistantCodex(w, r)
}
func (m *Module) ListRecipeAssistantCodexModels(w http.ResponseWriter, r *http.Request) {
	m.listRecipeAssistantCodexModels(w, r)
}

func (m *Module) ListArtifacts(w http.ResponseWriter, r *http.Request, _ ListArtifactsParams) {
	m.listArtifacts(w, r)
}
func (m *Module) ListArtifactPlacements(w http.ResponseWriter, r *http.Request, _ ID) {
	m.listArtifactPlacements(w, r)
}
func (m *Module) ListRecipeDrafts(w http.ResponseWriter, r *http.Request, _ ListRecipeDraftsParams) {
	m.listDrafts(w, r)
}
func (m *Module) CreateRecipeDraft(w http.ResponseWriter, r *http.Request) { m.createDraft(w, r) }
func (m *Module) DeleteRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ DeleteRecipeDraftParams) {
	m.deleteDraft(w, r)
}
func (m *Module) GetRecipeDraftRunExcerpt(w http.ResponseWriter, r *http.Request, _ ID) {
	m.getDraftRunExcerpt(w, r)
}
func (m *Module) GetRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID) { m.getDraft(w, r) }
func (m *Module) DismissRecipeDraftDiagnostics(w http.ResponseWriter, r *http.Request, _ ID, _ DismissRecipeDraftDiagnosticsParams) {
	m.dismissDraftDiagnostics(w, r)
}
func (m *Module) ResolveRecipeDraftReferences(w http.ResponseWriter, r *http.Request, _ ID, _ ResolveRecipeDraftReferencesParams) {
	m.resolveDraftReferences(w, r)
}
func (m *Module) GetRecipeDraftFile(w http.ResponseWriter, r *http.Request, _ ID, _ GetRecipeDraftFileParams) {
	m.getDraftFile(w, r)
}
func (m *Module) UpdateRecipeDraftFile(w http.ResponseWriter, r *http.Request, _ ID, _ UpdateRecipeDraftFileParams) {
	m.updateDraftFile(w, r)
}
func (m *Module) GenerateRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ GenerateRecipeDraftParams) {
	m.generateDraft(w, r)
}
func (m *Module) AcceptRecipeDraftProposal(w http.ResponseWriter, r *http.Request, _ ID, _ AcceptRecipeDraftProposalParams) {
	m.acceptDraftProposal(w, r)
}
func (m *Module) DiscardRecipeDraftProposal(w http.ResponseWriter, r *http.Request, _ ID, _ DiscardRecipeDraftProposalParams) {
	m.discardDraftProposal(w, r)
}
func (m *Module) UpdateRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ UpdateRecipeDraftParams) {
	m.updateDraft(w, r)
}
func (m *Module) UpdateRecipeDraftContext(w http.ResponseWriter, r *http.Request, _ ID, _ UpdateRecipeDraftContextParams) {
	m.updateDraftContext(w, r)
}
func (m *Module) InstallRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ InstallRecipeDraftParams) {
	m.installDraft(w, r)
}
func (m *Module) PackageRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ PackageRecipeDraftParams) {
	m.packageDraft(w, r)
}
func (m *Module) ListRecipeRepositories(w http.ResponseWriter, r *http.Request) {
	m.listRecipeRepositories(w, r)
}
func (m *Module) GetRecipeRepository(w http.ResponseWriter, r *http.Request, _ string) {
	m.getRecipeRepository(w, r)
}
func (m *Module) PlanRecipeRepositoryReplacement(w http.ResponseWriter, r *http.Request, _ string) {
	m.planRecipeRepositoryReplacement(w, r)
}
func (m *Module) StartRecipeRepositoryReplacement(w http.ResponseWriter, r *http.Request, _ string) {
	m.startRecipeRepositoryReplacement(w, r)
}
func (m *Module) PlanRecipeRepositoryUpdate(w http.ResponseWriter, r *http.Request, _ string) {
	m.planRecipeRepositoryUpdate(w, r)
}
func (m *Module) StartRecipeRepositoryUpdate(w http.ResponseWriter, r *http.Request, _ string) {
	m.startRecipeRepositoryUpdate(w, r)
}

func (m *Module) ListRecipes(w http.ResponseWriter, r *http.Request) { m.listRecipes(w, r) }
func (m *Module) CheckRecipeUpdates(w http.ResponseWriter, r *http.Request) {
	m.checkRecipeUpdates(w, r)
}
func (m *Module) ImportRecipe(w http.ResponseWriter, r *http.Request) { m.importRecipeHandler(w, r) }
func (m *Module) DeleteRecipe(w http.ResponseWriter, r *http.Request, digest string, _ DeleteRecipeParams) {
	m.deleteRecipe(w, r, digest)
}
func (m *Module) GetRecipe(w http.ResponseWriter, r *http.Request, digest string) {
	m.getRecipe(w, r, digest)
}
func (m *Module) ListTransfers(w http.ResponseWriter, r *http.Request)        { m.listTransfers(w, r) }
func (m *Module) CreateTransfer(w http.ResponseWriter, r *http.Request)       { m.createTransfer(w, r) }
func (m *Module) CancelTransfer(w http.ResponseWriter, r *http.Request, _ ID) { m.cancelTransfer(w, r) }
func (m *Module) GetTransfer(w http.ResponseWriter, r *http.Request, _ ID)    { m.getTransfer(w, r) }

func (m *Module) CheckRecipeRepositoryUpdates(w http.ResponseWriter, r *http.Request, _ ID) {
	m.checkRecipeRepositoryUpdates(w, r)
}
func (m *Module) CreateRecipeChange(w http.ResponseWriter, r *http.Request, digest string) {
	m.createRecipeChange(w, r, digest)
}
func (m *Module) InspectRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ InspectRecipeDraftParams) {
	m.inspectRecipeDraft(w, r)
}
func (m *Module) CompareRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID) {
	m.compareRecipeDraft(w, r)
}
func (m *Module) PreviewRecipeGeneration(w http.ResponseWriter, r *http.Request, _ ID, _ PreviewRecipeGenerationParams) {
	m.previewRecipeGeneration(w, r)
}
func (m *Module) PlanRecipeDownload(w http.ResponseWriter, r *http.Request, digest string) {
	m.planRecipeDownload(w, r, digest)
}
func (m *Module) StartRecipeDownload(w http.ResponseWriter, r *http.Request, digest string) {
	m.startRecipeDownload(w, r, digest)
}
func (m *Module) ListRecipeDownloads(w http.ResponseWriter, r *http.Request, digest string) {
	m.listRecipeDownloads(w, r, digest)
}
func (m *Module) ResumeRecipeDownload(w http.ResponseWriter, r *http.Request, digest, runID string) {
	m.resumeRecipeDownload(w, r, digest, runID)
}
func (m *Module) GetRecipeAvailability(w http.ResponseWriter, r *http.Request, digest string) {
	m.getRecipeAvailability(w, r, digest)
}
