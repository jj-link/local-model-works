package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
)

// handleCreateEnrollmentToken mints a one-time, 10-minute enrollment token.
// The raw token is returned exactly once; only its SHA-256 is stored.
func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Description string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "auth.invalid_body", err.Error())
		return
	}
	raw, err := ca.NewToken()
	if err != nil {
		handleErr(w, err)
		return
	}
	id, err := uuidV7()
	if err != nil {
		handleErr(w, err)
		return
	}
	sum := auth.SHA256([]byte(raw))
	expires := time.Now().UTC().Add(10 * time.Minute)
	ctx := r.Context()
	desc := sql.NullString{}
	if req.Description != "" {
		desc = sql.NullString{String: req.Description, Valid: true}
	}
	if err := s.q.CreateEnrollmentToken(ctx, db.CreateEnrollmentTokenParams{
		ID: id, TokenHash: sum[:], Description: desc, ExpiresAt: dbTime(expires),
	}); err != nil {
		handleErr(w, err)
		return
	}
	s.bus.Publish(ctx, "enrollment.token_created", "", mustJSON(map[string]any{"id": id, "expires_at": dbTime(expires)}))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          id,
		"token":       raw,
		"description": req.Description,
		"created_at":  dbTime(time.Now().UTC()),
		"expires_at":  dbTime(expires),
	})
}

func (s *Server) handleListEnrollmentTokens(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListEnrollmentTokens(r.Context())
	if err != nil {
		handleErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, t := range rows {
		desc := ""
		if t.Description.Valid {
			desc = t.Description.String
		}
		usedAt := ""
		if t.UsedAt.Valid {
			usedAt = t.UsedAt.String
		}
		out = append(out, map[string]any{
			"id":          t.ID,
			"description": desc,
			"created_at":  t.CreatedAt,
			"expires_at":  t.ExpiresAt,
			"used_at":     usedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	tok, err := s.q.GetEnrollmentTokenByID(r.Context(), id)
	if err != nil {
		if isNoRows(err) {
			writeErr(w, http.StatusNotFound, "resource.not_found", "enrollment token not found")
			return
		}
		handleErr(w, err)
		return
	}
	if tok.UsedAt.Valid {
		writeErr(w, http.StatusConflict, "resource.conflict", "enrollment token already used")
		return
	}
	if err := s.q.DeleteEnrollmentToken(r.Context(), id); err != nil {
		handleErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.q.ListNodes(r.Context())
	if err != nil {
		handleErr(w, err)
		return
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, nodeView(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request) {
	node, err := s.q.GetNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if isNoRows(err) {
			writeErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		handleErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, nodeView(node))
}

// handleApproveNode admits a pending node into the fleet. If the node has a
// live session it goes online immediately; otherwise the next heartbeat does.
func (s *Server) handleApproveNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	node, err := s.q.GetNode(ctx, id)
	if err != nil {
		if isNoRows(err) {
			writeErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		handleErr(w, err)
		return
	}
	if node.Status != "pending" {
		writeErr(w, http.StatusConflict, "node.not_pending", "node is not pending approval")
		return
	}
	if err := s.q.ApproveNode(ctx, id); err != nil {
		handleErr(w, err)
		return
	}
	status := "pending"
	if s.nodes.Online(id) {
		status = "online"
		if err := s.q.SetNodeStatus(ctx, db.SetNodeStatusParams{
			Status: status, LastHeartbeat: sql.NullString{String: dbTime(time.Now().UTC()), Valid: true}, ID: id,
		}); err != nil {
			handleErr(w, err)
			return
		}
	}
	approved, err := s.q.GetNode(ctx, id)
	if err != nil {
		handleErr(w, err)
		return
	}
	s.bus.Publish(ctx, "node.approved", id, mustJSON(map[string]any{"node_id": id, "status": status}))
	writeJSON(w, http.StatusOK, nodeView(approved))
}

// nodeView renders the Node API shape (labels/inventory as JSON values).
func nodeView(n db.Node) map[string]any {
	labels := map[string]string{}
	_ = json.Unmarshal([]byte(n.Labels), &labels)
	view := map[string]any{
		"id":           n.ID,
		"display_name": n.DisplayName,
		"labels":       labels,
		"status":       n.Status,
		"created_at":   n.CreatedAt,
	}
	if n.AgentVersion.Valid {
		view["agent_version"] = n.AgentVersion.String
	}
	if n.LastHeartbeat.Valid {
		view["last_heartbeat"] = n.LastHeartbeat.String
	}
	if n.CertificateExpiresAt.Valid {
		view["certificate_expires_at"] = n.CertificateExpiresAt.String
	}
	if n.Inventory.Valid && n.Inventory.String != "" {
		var inv any
		if json.Unmarshal([]byte(n.Inventory.String), &inv) == nil {
			view["inventory"] = inv
		}
	}
	return view
}

// isNoRows matches the sqlc-level no-row error.
func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
