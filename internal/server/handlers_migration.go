package server

import (
	"net/http"
)

func (s *Server) handleMigrationScan(w http.ResponseWriter, r *http.Request) {
	s.submitMigration(w, r, "migration-scan")
}

func (s *Server) handleMigrationImport(w http.ResponseWriter, r *http.Request) {
	s.submitMigration(w, r, "migration-import")
}

func (s *Server) submitMigration(w http.ResponseWriter, r *http.Request, kind string) {
	var input map[string]any
	if err := decodeBody(r, &input); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "migration.invalid_body", err.Error())
		return
	}
	runID, err := s.jobs.Submit(r.Context(), kind, input)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "migration.rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"run_id": runID})
}
