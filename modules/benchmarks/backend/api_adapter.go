package backend

import "net/http"

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListBenchmarks(w http.ResponseWriter, r *http.Request)       { m.list(w, r) }
func (m *Module) CreateBenchmark(w http.ResponseWriter, r *http.Request)      { m.create(w, r) }
func (m *Module) ListBenchmarkResults(w http.ResponseWriter, r *http.Request) { m.results(w, r) }
