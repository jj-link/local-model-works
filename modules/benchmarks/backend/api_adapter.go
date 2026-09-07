package backend

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
)

var _ ServerInterface = (*Module)(nil)

func (m *Module) ListBenchmarks(w http.ResponseWriter, r *http.Request, _ ListBenchmarksParams) {
	m.list(w, r)
}
func (m *Module) CreateBenchmark(w http.ResponseWriter, r *http.Request)      { m.create(w, r) }
func (m *Module) ListBenchmarkResults(w http.ResponseWriter, r *http.Request) { m.results(w, r) }
func (m *Module) GetBenchmarkCatalog(w http.ResponseWriter, r *http.Request)  { m.catalog(w, r) }
func (m *Module) GetBenchmark(w http.ResponseWriter, r *http.Request, runID openapi_types.UUID) {
	m.get(w, r, runID.String())
}
func (m *Module) CancelBenchmark(w http.ResponseWriter, r *http.Request, runID openapi_types.UUID) {
	m.cancel(w, r, runID.String())
}
func (m *Module) ListBenchmarkTrials(w http.ResponseWriter, r *http.Request, runID openapi_types.UUID) {
	m.trials(w, r, runID.String())
}
func (m *Module) DownloadBenchmarkBundle(w http.ResponseWriter, r *http.Request, runID openapi_types.UUID) {
	m.bundle(w, r, runID.String())
}
func (m *Module) DownloadBenchmarkSummary(w http.ResponseWriter, r *http.Request, runID openapi_types.UUID) {
	m.summaryArtifact(w, r, runID.String())
}
