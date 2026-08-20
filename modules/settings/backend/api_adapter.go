package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) GetModuleSettings(w http.ResponseWriter, r *http.Request, id string) {
	m.getModuleSettings(w, r)
}
func (m *Module) PutModuleSettings(w http.ResponseWriter, r *http.Request, id string, params PutModuleSettingsParams) {
	m.putModuleSettings(w, r)
}
func (m *Module) ListSecrets(w http.ResponseWriter, r *http.Request)         { m.listSecrets(w, r) }
func (m *Module) PutSecret(w http.ResponseWriter, r *http.Request)           { m.putSecret(w, r) }
func (m *Module) DeleteSecret(w http.ResponseWriter, r *http.Request, id ID) { m.deleteSecret(w, r) }
