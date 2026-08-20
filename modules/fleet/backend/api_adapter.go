package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListFabrics(w http.ResponseWriter, r *http.Request) { m.listFabrics(w, r) }
func (m *Module) CreateFabric(w http.ResponseWriter, r *http.Request, params CreateFabricParams) {
	m.createFabric(w, r)
}
func (m *Module) DeleteFabric(w http.ResponseWriter, r *http.Request, id ID, params DeleteFabricParams) {
	m.deleteFabric(w, r)
}
func (m *Module) GetFabric(w http.ResponseWriter, r *http.Request, id ID) { m.getFabric(w, r) }
func (m *Module) UpdateFabric(w http.ResponseWriter, r *http.Request, id ID, params UpdateFabricParams) {
	m.updateFabric(w, r)
}
func (m *Module) ListNodes(w http.ResponseWriter, r *http.Request)         { m.listNodes(w, r) }
func (m *Module) GetNode(w http.ResponseWriter, r *http.Request, id ID)    { m.getNode(w, r) }
func (m *Module) UpdateNode(w http.ResponseWriter, r *http.Request, id ID) { m.updateNode(w, r) }
func (m *Module) GetNodeTelemetry(w http.ResponseWriter, r *http.Request, id ID, params GetNodeTelemetryParams) {
	m.nodeTelemetry(w, r)
}
func (m *Module) ApproveNode(w http.ResponseWriter, r *http.Request, id ID) { m.approveNode(w, r) }
func (m *Module) RotateNodeCertificate(w http.ResponseWriter, r *http.Request, _ ID) {
	m.rotateCertificate(w, r)
}
