// Package backend implements the fleet first-party module: enrolled nodes
// (list, detail, display name/labels, approval, certificate rotation) and
// the operator CRUD for multi-node fabrics, on top of the core fabric
// service. The fleet module owns no job kinds and declares no settings.

package backend

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/httpx"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
	"github.com/jj-link/local-model-works/internal/telemetry"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// Module is the fleet backend.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

// RegisterJobs: the fleet module operates on node and fabric state; it
// creates no runs.
func (m *Module) RegisterJobs(*jobs.Registry) {}

// RegisterSettings: the fleet manifest declares no settings.
func (m *Module) RegisterSettings(*settings.Registry) {}

// RegisterHTTP mounts the module's routes on the authenticated group.
func (m *Module) RegisterHTTP(r chi.Router) {
	HandlerFromMux(m, r)
}

func (m *Module) nodeTelemetry(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	from, to, limit := now-3600, now, 2000
	resolution := r.URL.Query().Get("resolution")
	if resolution == "" {
		resolution = "5s"
	}
	if value, err := strconv.ParseInt(r.URL.Query().Get("from"), 10, 64); err == nil {
		from = value
	}
	if value, err := strconv.ParseInt(r.URL.Query().Get("to"), 10, 64); err == nil {
		to = value
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		limit = value
	}
	samples, err := m.env.Telemetry.NodeHistory(r.Context(), chi.URLParam(r, "id"), resolution, from, to, limit)
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "telemetry.query", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, samples)
}

// listNodeTelemetry — GET /nodes/telemetry: newest full sample per node.
func (m *Module) listNodeTelemetry(w http.ResponseWriter, r *http.Request) {
	samples, err := m.env.Telemetry.LatestNodes(r.Context())
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "telemetry.query", err.Error())
		return
	}
	ids := make([]string, 0, len(samples))
	for id := range samples {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]telemetry.NodeSample, 0, len(ids))
	for _, id := range ids {
		out = append(out, samples[id])
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// listNodes — GET /nodes.
func (m *Module) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := m.env.Q.ListNodes(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, m.nodeView(n))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getNode — GET /nodes/{id}.
func (m *Module) getNode(w http.ResponseWriter, r *http.Request) {
	node, err := m.env.Q.GetNode(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m.nodeView(node))
}

// updateNode — PUT /nodes/{id}: set display name and labels. A PUT is a
// full replace; labels omitted means cleared.
func (m *Module) updateNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	var req struct {
		DisplayName string            `json:"display_name"`
		Labels      map[string]string `json:"labels"`
	}
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.HandleErr(w, fmt.Errorf("%w: %v", httpx.ErrUnprocessable, err))
		return
	}
	labels := "{}"
	if req.Labels != nil {
		b, err := json.Marshal(req.Labels)
		if err != nil {
			httpx.HandleErr(w, fmt.Errorf("%w: %v", httpx.ErrUnprocessable, err))
			return
		}
		labels = string(b)
	}
	if err := m.env.Q.UpdateNodeMeta(ctx, db.UpdateNodeMetaParams{
		DisplayName: req.DisplayName, Labels: labels, ID: id,
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	node, err := m.env.Q.GetNode(ctx, id)
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, m.nodeView(node))
}

// approveNode — POST /nodes/{id}/approve: admit a pending node into the
// fleet. If the node has a live session it goes online immediately;
// otherwise the next heartbeat does.
func (m *Module) approveNode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	node, err := m.env.Q.GetNode(ctx, id)
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	if node.Status != "pending" {
		httpx.WriteErr(w, http.StatusConflict, "node.not_pending", "node is not pending approval")
		return
	}
	if err := m.env.Q.ApproveNode(ctx, id); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	status := "pending"
	if m.env.Nodes.Online(id) {
		status = "online"
		if err := m.env.Q.SetNodeStatus(ctx, db.SetNodeStatusParams{
			Status: status, LastHeartbeat: sql.NullString{String: httpx.DBTime(time.Now().UTC()), Valid: true}, ID: id,
		}); err != nil {
			httpx.HandleErr(w, err)
			return
		}
	}
	approved, err := m.env.Q.GetNode(ctx, id)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	m.env.Bus.Publish(ctx, "node.approved", id, httpx.MustJSON(map[string]any{"node_id": id, "status": status}))
	httpx.WriteJSON(w, http.StatusOK, m.nodeView(approved))
}

// rotateCertificate — POST /nodes/{id}/rotate-certificate: re-sign the
// node's stored public key with a fresh 90-day client certificate. If the
// node has a live session the replacement is pushed immediately; otherwise
// the agent picks it up on its next session.
func (m *Module) rotateCertificate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if _, err := m.env.Q.GetNode(ctx, id); err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "node not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	cred, err := m.env.Q.GetNodeCredential(ctx, id)
	if err != nil {
		if httpx.IsNoRows(err) {
			httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", "node credential not found")
			return
		}
		httpx.HandleErr(w, err)
		return
	}
	pub, err := ca.ParsePublicKeyPEM([]byte(cred.PublicKeyPem))
	if err != nil {
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", "node public key: "+err.Error())
		return
	}
	certPEM, expires, err := m.env.CA.NodeCertFor(id, "", pub, ca.CertValidity)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	serial, err := ca.SerialOf(certPEM)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if err := m.env.Q.UpsertNodeCredential(ctx, db.UpsertNodeCredentialParams{
		NodeID: id, PublicKeyPem: cred.PublicKeyPem, Serial: serial,
		IssuedAt: httpx.DBTime(time.Now().UTC()), ExpiresAt: httpx.DBTime(expires),
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if err := m.env.Q.SetNodeCertificate(ctx, db.SetNodeCertificateParams{
		CertificateExpiresAt: sql.NullString{String: httpx.DBTime(expires), Valid: true},
		ID:                   id,
	}); err != nil {
		httpx.HandleErr(w, err)
		return
	}
	if m.env.Nodes.Online(id) {
		m.env.Nodes.Send(id, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_Certificate{
			Certificate: &agentv1.Certificate{
				NodeCertificatePem: string(certPEM),
				CaCertificatePem:   string(m.env.CA.PEMCert()),
				ExpiresAt:          timestamppb.New(expires),
			},
		}})
	}
	cert, err := ca.ParseCertPEM(certPEM)
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	m.env.Bus.Publish(ctx, "node.certificate_rotated", id, httpx.MustJSON(map[string]any{"serial": serial}))
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"serial":     serial,
		"not_before": httpx.DBTime(cert.NotBefore),
		"expires_at": httpx.DBTime(expires),
	})
}

// nodeView renders the Node API shape (labels/inventory as JSON values,
// timestamps as stored strings); status is reported online whenever a
// live session exists, even if the stored row has not caught up.
func (m *Module) nodeView(n db.Node) map[string]any {
	labels := map[string]string{}
	_ = json.Unmarshal([]byte(n.Labels), &labels)
	status := n.Status
	if m.env.Nodes.Online(n.ID) {
		status = "online"
	}
	view := map[string]any{
		"id":           n.ID,
		"display_name": n.DisplayName,
		"labels":       labels,
		"status":       status,
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

// writeFabricErr maps fabric service sentinels to the stable (status,
// code) pairs the OpenAPI contract declares; httpx sentinels pass through.
func writeFabricErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fabric.ErrUnknown):
		httpx.WriteErr(w, http.StatusNotFound, "resource.not_found", err.Error())
	case errors.Is(err, fabric.ErrVersionMismatch),
		errors.Is(err, fabric.ErrNameConflict),
		errors.Is(err, fabric.ErrInUse):
		httpx.WriteErr(w, http.StatusConflict, "resource.conflict", err.Error())
	case errors.Is(err, fabric.ErrValidation):
		httpx.WriteErr(w, http.StatusUnprocessableEntity, "resource.unprocessable", err.Error())
	default:
		httpx.HandleErr(w, err)
	}
}

// listFabrics — GET /fabrics.
func (m *Module) listFabrics(w http.ResponseWriter, r *http.Request) {
	fabrics, err := m.env.Fabrics.List(r.Context())
	if err != nil {
		httpx.HandleErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, fabrics)
}

// createFabric — POST /fabrics.
func (m *Module) createFabric(w http.ResponseWriter, r *http.Request) {
	var req fabric.CreateRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.HandleErr(w, fmt.Errorf("%w: %v", httpx.ErrUnprocessable, err))
		return
	}
	f, err := m.env.Fabrics.Create(r.Context(), req)
	if err != nil {
		writeFabricErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, f)
}

// getFabric — GET /fabrics/{id}.
func (m *Module) getFabric(w http.ResponseWriter, r *http.Request) {
	f, err := m.env.Fabrics.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeFabricErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

// updateFabric — PUT /fabrics/{id}, under If-Match.
func (m *Module) updateFabric(w http.ResponseWriter, r *http.Request) {
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		httpx.WriteErr(w, http.StatusConflict, "resource.conflict", "If-Match is required")
		return
	}
	var req fabric.CreateRequest
	if err := httpx.DecodeBody(r, &req); err != nil {
		httpx.HandleErr(w, fmt.Errorf("%w: %v", httpx.ErrUnprocessable, err))
		return
	}
	f, err := m.env.Fabrics.Update(r.Context(), chi.URLParam(r, "id"), ifMatch, req)
	if err != nil {
		writeFabricErr(w, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, f)
}

// deleteFabric — DELETE /fabrics/{id}, under If-Match.
func (m *Module) deleteFabric(w http.ResponseWriter, r *http.Request) {
	ifMatch := r.Header.Get("If-Match")
	if ifMatch == "" {
		httpx.WriteErr(w, http.StatusConflict, "resource.conflict", "If-Match is required")
		return
	}
	if err := m.env.Fabrics.Delete(r.Context(), chi.URLParam(r, "id"), ifMatch); err != nil {
		writeFabricErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
