package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListArtifacts(w http.ResponseWriter, r *http.Request, _ ListArtifactsParams) {
	m.listArtifacts(w, r)
}
func (m *Module) ListArtifactPlacements(w http.ResponseWriter, r *http.Request, _ ID) {
	m.listArtifactPlacements(w, r)
}
func (m *Module) ListRecipeDrafts(w http.ResponseWriter, r *http.Request)        { m.listDrafts(w, r) }
func (m *Module) CreateRecipeDraft(w http.ResponseWriter, r *http.Request)       { m.createDraft(w, r) }
func (m *Module) DeleteRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID) { m.deleteDraft(w, r) }
func (m *Module) GetRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID)    { m.getDraft(w, r) }
func (m *Module) UpdateRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID, _ UpdateRecipeDraftParams) {
	m.updateDraft(w, r)
}
func (m *Module) InstallRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID) {
	m.installDraft(w, r)
}
func (m *Module) PackageRecipeDraft(w http.ResponseWriter, r *http.Request, _ ID) {
	m.packageDraft(w, r)
}
func (m *Module) ListRecipes(w http.ResponseWriter, r *http.Request)  { m.listRecipes(w, r) }
func (m *Module) ImportRecipe(w http.ResponseWriter, r *http.Request) { m.importRecipeHandler(w, r) }
func (m *Module) DeleteRecipe(w http.ResponseWriter, r *http.Request, _ string, _ DeleteRecipeParams) {
	m.deleteRecipe(w, r)
}
func (m *Module) GetRecipe(w http.ResponseWriter, r *http.Request, _ string) { m.getRecipe(w, r) }
func (m *Module) SetRecipeTrust(w http.ResponseWriter, r *http.Request, _ string) {
	m.setRecipeTrust(w, r)
}
func (m *Module) ListTransfers(w http.ResponseWriter, r *http.Request)        { m.listTransfers(w, r) }
func (m *Module) CreateTransfer(w http.ResponseWriter, r *http.Request)       { m.createTransfer(w, r) }
func (m *Module) CancelTransfer(w http.ResponseWriter, r *http.Request, _ ID) { m.cancelTransfer(w, r) }
func (m *Module) GetTransfer(w http.ResponseWriter, r *http.Request, _ ID)    { m.getTransfer(w, r) }
