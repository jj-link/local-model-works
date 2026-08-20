package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListRuns(w http.ResponseWriter, r *http.Request, _ ListRunsParams) { m.list(w, r) }
func (m *Module) GetRun(w http.ResponseWriter, r *http.Request, _ ID)               { m.get(w, r) }
func (m *Module) CancelRun(w http.ResponseWriter, r *http.Request, _ ID)            { m.cancel(w, r) }
func (m *Module) RunLogs(w http.ResponseWriter, r *http.Request, _ ID)              { m.logs(w, r) }
