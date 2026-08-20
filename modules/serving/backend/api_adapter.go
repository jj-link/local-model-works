package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListDeployments(w http.ResponseWriter, r *http.Request)     { m.list(w, r) }
func (m *Module) CreateDeployment(w http.ResponseWriter, r *http.Request)    { m.create(w, r) }
func (m *Module) PlanDeployment(w http.ResponseWriter, r *http.Request)      { m.plan(w, r) }
func (m *Module) GetDeployment(w http.ResponseWriter, r *http.Request, _ ID) { m.get(w, r) }
func (m *Module) DeploymentLogs(w http.ResponseWriter, r *http.Request, _ ID, _ DeploymentLogsParams) {
	m.logs(w, r)
}
func (m *Module) StopDeployment(w http.ResponseWriter, r *http.Request, _ ID)   { m.stop(w, r) }
func (m *Module) VerifyDeployment(w http.ResponseWriter, r *http.Request, _ ID) { m.verify(w, r) }
