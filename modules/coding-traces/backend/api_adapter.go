package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListCodingTraces(w http.ResponseWriter, r *http.Request, _ ListCodingTracesParams) {
	m.listTraces(w, r)
}
func (m *Module) ListCodingTraceExports(w http.ResponseWriter, r *http.Request) { m.listExports(w, r) }
func (m *Module) CreateCodingTraceExport(w http.ResponseWriter, r *http.Request) {
	m.createExport(w, r)
}
func (m *Module) DownloadCodingTraceExport(w http.ResponseWriter, r *http.Request, _ ExportID) {
	m.downloadExport(w, r)
}
func (m *Module) GetCodingTraceSettings(w http.ResponseWriter, r *http.Request) { m.getSettings(w, r) }
func (m *Module) PutCodingTraceSettings(w http.ResponseWriter, r *http.Request, _ PutCodingTraceSettingsParams) {
	m.putSettings(w, r)
}
func (m *Module) ListSweGymExperiments(w http.ResponseWriter, r *http.Request) {
	m.listExperiments(w, r)
}
func (m *Module) CreateSweGymExperiment(w http.ResponseWriter, r *http.Request) {
	m.createExperiment(w, r)
}
func (m *Module) GetSweGymExperiment(w http.ResponseWriter, r *http.Request, _ ExperimentID) {
	m.getExperiment(w, r)
}
func (m *Module) CancelSweGymExperiment(w http.ResponseWriter, r *http.Request, _ ExperimentID) {
	m.cancelExperiment(w, r)
}
func (m *Module) ResumeSweGymExperiment(w http.ResponseWriter, r *http.Request, _ ExperimentID) {
	m.resumeExperiment(w, r)
}
func (m *Module) PlanSweGymExperiment(w http.ResponseWriter, r *http.Request) { m.planExperiment(w, r) }
func (m *Module) DeleteCodingTrace(w http.ResponseWriter, r *http.Request, _ TraceID) {
	m.deleteTrace(w, r)
}
func (m *Module) GetCodingTrace(w http.ResponseWriter, r *http.Request, _ TraceID) { m.getTrace(w, r) }
func (m *Module) ListCodingTraceEvents(w http.ResponseWriter, r *http.Request, _ TraceID, _ ListCodingTraceEventsParams) {
	m.listTraceEvents(w, r)
}
func (m *Module) PinCodingTrace(w http.ResponseWriter, r *http.Request, _ TraceID) { m.pinTrace(w, r) }
