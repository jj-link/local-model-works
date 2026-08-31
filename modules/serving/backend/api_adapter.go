package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListDeployments(w http.ResponseWriter, r *http.Request)     { m.list(w, r) }
func (m *Module) CreateDeployment(w http.ResponseWriter, r *http.Request)    { m.create(w, r) }
func (m *Module) PlanDeployment(w http.ResponseWriter, r *http.Request)      { m.plan(w, r) }
func (m *Module) GetDeployment(w http.ResponseWriter, r *http.Request, _ ID) { m.get(w, r) }
func (m *Module) ListLaunchProfiles(w http.ResponseWriter, r *http.Request, _ string) {
	m.listLaunchProfiles(w, r)
}
func (m *Module) CreateLaunchProfile(w http.ResponseWriter, r *http.Request, _ string) {
	m.createLaunchProfile(w, r)
}
func (m *Module) UpdateLaunchProfile(w http.ResponseWriter, r *http.Request, _ string) {
	m.updateLaunchProfile(w, r)
}
func (m *Module) DeleteLaunchProfile(w http.ResponseWriter, r *http.Request, _ string) {
	m.deleteLaunchProfile(w, r)
}
func (m *Module) DeploymentLogs(w http.ResponseWriter, r *http.Request, _ ID, _ DeploymentLogsParams) {
	m.logs(w, r)
}
func (m *Module) StopDeployment(w http.ResponseWriter, r *http.Request, _ ID)  { m.stop(w, r) }
func (m *Module) StartDeployment(w http.ResponseWriter, r *http.Request, _ ID) { m.start(w, r) }
func (m *Module) DeleteDeployment(w http.ResponseWriter, r *http.Request, _ ID) {
	m.deleteDeployment(w, r)
}
func (m *Module) VerifyDeployment(w http.ResponseWriter, r *http.Request, _ ID) { m.verify(w, r) }
func (m *Module) ListDeploymentTelemetry(w http.ResponseWriter, r *http.Request) {
	m.listDeploymentTelemetry(w, r)
}
func (m *Module) GetDeploymentTelemetry(w http.ResponseWriter, r *http.Request, _ ID, _ GetDeploymentTelemetryParams) {
	m.deploymentTelemetry(w, r)
}
