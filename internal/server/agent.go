package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/nodes"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	agentv1connect "github.com/jj-link/local-model-works/proto/agent/v1/agentv1connect"
)

type peerCertKey struct{}

func peerCertFrom(ctx context.Context) *x509.Certificate {
	c, _ := ctx.Value(peerCertKey{}).(*x509.Certificate)
	return c
}

// StartAgentListener starts the separately configured :9443 mTLS listener
// and returns its bound address. TLS client certificates are requested;
// Enroll authenticates with a one-time token (the agent has no certificate
// yet), Session requires a verified node certificate.
func (s *Server) StartAgentListener(ctx context.Context) (string, error) {
	dnsNames, ipSANs := serverSans(s.cfg.ServerName)
	certPEM, keyPEM, _, err := s.ca.ServerCert(dnsNames, ipSANs, 90*24*time.Hour)
	if err != nil {
		return "", err
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return "", fmt.Errorf("server keypair: %w", err)
	}
	// Present the CA after the leaf so a fingerprint-pinned installer can
	// recover the CA certificate and pin it on first contact.
	if caBlock, _ := pem.Decode(s.ca.PEMCert()); caBlock != nil {
		tlsCert.Certificate = append(tlsCert.Certificate, caBlock.Bytes)
	}
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
				r = r.WithContext(context.WithValue(r.Context(), peerCertKey{}, r.TLS.PeerCertificates[0]))
			}
			next.ServeHTTP(w, r)
		})
	})
	mux.Get("/packages/{digest}/layer", s.handlePackageLayer)
	if p, h := agentv1connect.NewEnrollmentServiceHandler(s); p != "" {
		mux.Mount(p, h)
	}
	if p, h := agentv1connect.NewAgentServiceHandler(s); p != "" {
		mux.Mount(p, h)
	}
	ln, err := net.Listen("tcp", s.cfg.AgentAddr)
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.mu.Lock()
	s.agentSrv = srv
	s.agentAddr = ln.Addr().String()
	s.mu.Unlock()

	go func() {
		clientCAs := x509.NewCertPool()
		clientCAs.AppendCertsFromPEM(s.ca.PEMCert())
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The agent speaks gRPC over h2; advertise it so the connect
			// client's ALPN check passes.
			NextProtos: []string{"h2"},
			// A presented client certificate must chain to our CA; Enroll
			// presents none because the agent has not been issued one yet.
			ClientAuth: tls.VerifyClientCertIfGiven,
			ClientCAs:  clientCAs,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
				return &tlsCert, nil
			},
		}
		if err := srv.Serve(tls.NewListener(ln, tlsCfg)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(os.Stderr, "agent listener: %v\n", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		// Shutdown only waits for connections to drain; a live agent
		// session that outlives the grace period would keep its stream
		// (and the agent talking to a dead controller) indefinitely.
		// Force-close the remainder so agents observe the disconnect and
		// reconnect to a restarted controller.
		_ = srv.Close()
	}()
	return ln.Addr().String(), nil
}

// serverSans splits the configured comma-separated server names and
// resolves any DNS names to IP literals so the minted leaf certificate is
// valid for every address agents connect to: the loopback name for local
// agents and the tailnet name for remote nodes.
func serverSans(names string) ([]string, []net.IP) {
	var dns []string
	var ips []net.IP
	seen := map[string]bool{}
	for _, n := range strings.Split(names, ",") {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		if ip := net.ParseIP(n); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dns = append(dns, n)
		resolved, err := net.LookupIP(n)
		if err != nil {
			continue
		}
		for _, ip := range resolved {
			ips = append(ips, ip)
		}
	}
	return dns, ips
}

// Enroll exchanges a one-time token and the agent's self-generated public
// key for a node certificate. The node row is created (pending approval);
// the credential stores the public key and serial for certificate rotation.
func (s *Server) Enroll(ctx context.Context, req *connect.Request[agentv1.EnrollRequest]) (*connect.Response[agentv1.EnrollResponse], error) {
	r := req.Msg
	if r.GetCaFingerprint() != s.ca.Fingerprint() {
		return nil, connect.NewError(connect.CodeUnauthenticated,
			fmt.Errorf("CA fingerprint mismatch: agent pinned a different CA"))
	}
	pub, err := ca.ParsePublicKeyPEM([]byte(r.GetPublicKeyPem()))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("public key: %w", err))
	}
	sum := auth.SHA256([]byte(r.GetToken()))
	tok, err := s.q.GetEnrollmentTokenByHash(ctx, sum[:])
	if err != nil {
		if isNoRows(err) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("enrollment token not recognized"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().UTC()
	if tok.UsedAt.Valid {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("enrollment token already used"))
	}
	expires, err := parseDBTime(tok.ExpiresAt)
	if err != nil || now.After(expires) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("enrollment token expired"))
	}

	agent := r.GetAgent()
	nodeID, err := uuidV7()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	displayName := agent.GetHostname()
	if displayName == "" {
		displayName = "node-" + nodeID[:8]
	}
	certPEM, certExpiry, err := s.ca.NodeCertFor(nodeID, agent.GetHostname(), pub, 90*24*time.Hour)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	serial, err := ca.SerialOf(certPEM)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.q.CreateNode(ctx, db.CreateNodeParams{
		ID: nodeID, DisplayName: displayName, Labels: "{}", CreatedAt: dbTime(now),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := s.q.UpsertNodeCredential(ctx, db.UpsertNodeCredentialParams{
		NodeID: nodeID, PublicKeyPem: r.GetPublicKeyPem(), Serial: serial,
		IssuedAt: dbTime(now), ExpiresAt: dbTime(certExpiry),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if _, err := s.q.UseEnrollmentToken(ctx, db.UseEnrollmentTokenParams{
		UsedAt:     sql.NullString{String: dbTime(now), Valid: true},
		UsedByNode: sql.NullString{String: nodeID, Valid: true},
		ID:         tok.ID,
		ExpiresAt:  dbTime(now),
	}); err != nil {
		return nil, connect.NewError(connect.CodeAborted, errors.New("enrollment token was consumed concurrently"))
	}
	if err := s.q.SetNodeCertificate(ctx, db.SetNodeCertificateParams{
		CertificateExpiresAt: sql.NullString{String: dbTime(certExpiry), Valid: true},
		ID:                   nodeID,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	s.bus.Publish(ctx, "node.enrolled", nodeID, mustJSON(map[string]any{
		"node_id": nodeID, "display_name": displayName,
	}))
	return connect.NewResponse(&agentv1.EnrollResponse{
		NodeId:               nodeID,
		NodeCertificatePem:   string(certPEM),
		CaCertificatePem:     string(s.ca.PEMCert()),
		ServerName:           s.cfg.ServerName,
		CertificateExpiresAt: timestamppb.New(certExpiry),
	}), nil
}

// Session is the per-node bidirectional control channel: heartbeat,
// inventory, telemetry, command results, state updates, logs, placements,
func (s *Server) Session(ctx context.Context, stream *connect.BidiStream[agentv1.AgentMessage, agentv1.ServerMessage]) error {
	leaf := peerCertFrom(ctx)
	if leaf == nil {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("node client certificate required"))
	}
	nodeID := leaf.Subject.CommonName
	const nodeCNPrefix = "lmw-node-"
	if len(nodeID) > len(nodeCNPrefix) && nodeID[:len(nodeCNPrefix)] == nodeCNPrefix {
		nodeID = nodeID[len(nodeCNPrefix):]
	}
	node, err := s.q.GetNode(ctx, nodeID)
	if err != nil {
		if isNoRows(err) {
			return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("unknown node %s", nodeID))
		}
		return connect.NewError(connect.CodeInternal, err)
	}

	conn := nodes.NewConn(nodeID)
	s.nodes.Register(conn)
	defer func() {
		s.nodes.Unregister(conn)
		conn.Close()
		// Only mark offline when this session was the last one; a
		// replacement session registered in the gap must not be knocked
		// offline by the old session's teardown.
		if !s.nodes.Online(nodeID) {
			s.markOffline(context.WithoutCancel(ctx), nodeID)
		}
	}()

	status := "pending"
	if node.Status != "pending" {
		// The node was already approved (possibly while this session was
		// connecting): mark it online. A still-pending node is left alone
		// rather than rewritten, so a concurrent ApproveNode can never be
		// clobbered back to "pending" by this session's status write.
		status = "online"
		if err := s.q.SetNodeStatus(ctx, db.SetNodeStatusParams{
			Status: status, LastHeartbeat: sql.NullString{String: dbTime(time.Now().UTC()), Valid: true}, ID: nodeID,
		}); err != nil {
			return connect.NewError(connect.CodeInternal, err)
		}
	}
	s.bus.Publish(ctx, "node.online", nodeID, mustJSON(map[string]any{"status": status}))

	// Reconcile: tell the agent which active deployments it must host,
	// then re-drive dispatch from persisted phases (idempotent).
	if refs := s.reconcileDeployments(ctx, nodeID); len(refs) > 0 {
		conn.Send(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_ReconcileRequest{
			ReconcileRequest: &agentv1.ReconcileRequest{Deployments: refs, Reason: "reconnect"},
		}})
	}
	s.deploys.Converge(ctx, nodeID)

	// Writer pump: outbound messages until the stream or the conn closes.
	go func() {
		for {
			select {
			case <-conn.Done():
				return
			case m := <-conn.SendCh:
				if err := stream.Send(m); err != nil {
					return
				}
			}
		}
	}()

	// Watchdog: a silent-but-open connection goes offline after the
	// heartbeat timeout.
	var hbMu sync.Mutex
	lastHB := time.Now()
	stopped := make(chan struct{})
	defer close(stopped)
	tick := s.heartbeatTimeout / 3
	if tick < time.Second {
		tick = time.Second
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stopped:
				return
			case <-t.C:
				hbMu.Lock()
				idle := time.Since(lastHB)
				hbMu.Unlock()
				if idle > s.heartbeatTimeout {
					s.markOffline(context.WithoutCancel(ctx), nodeID)
				}
			}
		}
	}()

	for {
		msg, err := stream.Receive()
		if err != nil {
			break
		}
		switch body := msg.GetBody().(type) {
		case *agentv1.AgentMessage_Heartbeat:
			now := time.Now()
			hbMu.Lock()
			lastHB = now
			hbMu.Unlock()
			if body.Heartbeat.GetAgentVersion() != "" {
				_ = s.q.SetNodeVersion(ctx, db.SetNodeVersionParams{
					AgentVersion: sql.NullString{String: body.Heartbeat.GetAgentVersion(), Valid: true},
					ID:           nodeID,
				})
			}
			// The session start captured the row; a node approved while
			// connected needs a re-check before it is promoted to online.
			online := node.Status != "pending"
			if !online {
				if cur, err := s.q.GetNode(ctx, nodeID); err == nil {
					online = cur.Status != "pending"
				}
			}
			if online {
				_ = s.q.SetNodeStatus(ctx, db.SetNodeStatusParams{
					Status: "online", LastHeartbeat: sql.NullString{String: dbTime(now), Valid: true}, ID: nodeID,
				})
			}
			_ = stream.Send(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_HeartbeatAck{
				HeartbeatAck: &agentv1.HeartbeatAck{
					ServerTime:          timestamppb.New(now),
					TelemetryIntervalMs: 5000,
				},
			}})
		case *agentv1.AgentMessage_Inventory:
			data, err := json.Marshal(body.Inventory)
			if err == nil {
				if err := s.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{
					Inventory:    sql.NullString{String: string(data), Valid: true},
					AgentVersion: sql.NullString{},
					ID:           nodeID,
				}); err == nil {
					s.bus.Publish(ctx, "node.inventory", nodeID, data)
				}
			}
		case *agentv1.AgentMessage_Telemetry:
			t := body.Telemetry
			data, err := json.Marshal(t)
			if err == nil {
				_ = s.telemetry.Ingest(ctx, nodeID, t.GetAt().AsTime(), data)
			}
		case *agentv1.AgentMessage_CommandResult:
			s.bus.Publish(ctx, "run.command_result", nodeID, mustJSON(body.CommandResult))
			s.commands.Deliver(body.CommandResult)
			s.deploys.OnCommandResult(ctx, body.CommandResult)
		case *agentv1.AgentMessage_StateUpdate:
			s.applyStateUpdate(ctx, nodeID, body.StateUpdate)
		case *agentv1.AgentMessage_LogChunk:
			s.appendLog(nodeID, body.LogChunk)
		case *agentv1.AgentMessage_PlacementReport:
			s.applyPlacementReport(ctx, nodeID, body.PlacementReport)
		case *agentv1.AgentMessage_TransferProgress:
			s.applyTransferProgress(ctx, nodeID, body.TransferProgress)
		case *agentv1.AgentMessage_RotateCertificate:
			s.rotateCertificate(ctx, nodeID, conn)
		case *agentv1.AgentMessage_Ack:
			s.bus.Publish(ctx, "run.command_ack", nodeID, mustJSON(body.Ack))
			if !body.Ack.GetOk() {
				s.deploys.OnTransferResult(ctx, body.Ack.GetCommandId(), body.Ack.GetError())
			}
		}
	}
	return nil
}

// markOffline transitions an online node to offline, once.
func (s *Server) markOffline(ctx context.Context, nodeID string) {
	node, err := s.q.GetNode(ctx, nodeID)
	if err != nil {
		return
	}
	if node.Status == "online" {
		if err := s.q.SetNodeStatus(ctx, db.SetNodeStatusParams{Status: "offline", LastHeartbeat: sql.NullString{}, ID: nodeID}); err != nil {
			return
		}
		s.bus.Publish(ctx, "node.offline", nodeID, nil)
		s.deploys.MarkNodeOffline(ctx, nodeID)
	}
}

// reconcileDeployments lists the unresolved deployments whose placement
// includes the node.
func (s *Server) reconcileDeployments(ctx context.Context, nodeID string) []*agentv1.DeploymentRef {
	deps, err := s.q.ListDeployments(ctx)
	if err != nil {
		return nil
	}
	var refs []*agentv1.DeploymentRef
	for _, d := range deps {
		if d.DesiredState != "running" && d.DesiredState != "stopped" {
			continue
		}
		if d.DesiredState == "stopped" && d.ObservedState == "stopped" {
			continue
		}
		ps := deploy.ParsePlacementSet(d.Placement)
		if len(ps.RanksOnNode(nodeID)) == 0 {
			continue
		}
		refs = append(refs, &agentv1.DeploymentRef{
			DeploymentId: d.ID,
			DesiredState: d.DesiredState,
		})
	}
	return refs
}

// applyStateUpdate delegates container-state handling to the deploy
// service (observed state, run failure, stop confirmation).
func (s *Server) applyStateUpdate(ctx context.Context, nodeID string, su *agentv1.StateUpdate) {
	s.deploys.OnStateUpdate(ctx, nodeID, su)
}

func (s *Server) appendLog(nodeID string, lc *agentv1.LogChunk) {
	runID := lc.GetRunId()
	if runID == "" {
		return
	}
	// Terminal marker: the agent's tailer fully drained this stream; record
	// the deterministic EOF so log consumers can wait for it.
	if lc.GetFinal() {
		s.runs.MarkLogEnd(runID, lc.GetDeploymentId(), lc.GetRank(), lc.GetStream(), lc.GetEndOffset())
		return
	}
	if err := s.ensureRunRoot(); err != nil {
		return
	}
	path := s.runLogPath(runID, lc.GetDeploymentId(), lc.GetRank(), lc.GetStream())
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(lc.GetData())
}

// applyPlacementReport records one node placement report. Agents carry the
// artifact identity (they do not know DB ids), so the row is upserted under
// the resolved artifact id; the deploy service is notified with the
// identity (its transfer-gate keys are identity-based).
func (s *Server) applyPlacementReport(ctx context.Context, nodeID string, pr *agentv1.PlacementReport) {
	identity := pr.GetArtifactId()
	parsed, err := artifactidentity.Parse(identity)
	if err != nil {
		s.bus.Publish(ctx, "artifact.placement.rejected", nodeID, mustJSON(map[string]any{
			"identity": identity, "error": err.Error(),
		}))
		return
	}
	art, err := s.q.GetArtifactByIdentity(ctx, identity)
	if errors.Is(err, sql.ErrNoRows) {
		sum := sha256.Sum256([]byte(identity))
		if err := s.q.CreateArtifact(ctx, db.CreateArtifactParams{
			ID: "artifact-" + hex.EncodeToString(sum[:8]), Kind: parsed.Kind, Identity: identity,
			Revision: optionalSQLString(parsed.Revision), Digest: optionalSQLString(parsed.Digest), Metadata: "{}",
		}); err != nil {
			return
		}
		art, err = s.q.GetArtifactByIdentity(ctx, identity)
	}
	if err != nil {
		return
	}
	verified := sql.NullString{}
	if v := pr.GetVerifiedAt(); v != nil && !v.AsTime().IsZero() {
		verified = sql.NullString{String: dbTime(v.AsTime()), Valid: true}
	}
	diagnostics := make([]map[string]any, 0, len(pr.GetDiagnostics()))
	for _, d := range pr.GetDiagnostics() {
		diagnostics = append(diagnostics, map[string]any{
			"code": d.GetCode(), "severity": d.GetSeverity(), "message": d.GetMessage(),
		})
	}
	dj, _ := json.Marshal(diagnostics)
	// Dedup: agents re-report their whole placement set on every rescan, so
	// without change detection the activity stream floods with identical
	// artifact.placement events. Only publish (and notify the deploy
	// service) when the row is new or a tracked field actually changed.
	prev, perr := s.q.GetPlacement(ctx, db.GetPlacementParams{
		ArtifactID: art.ID, NodeID: nodeID, Path: pr.GetPath(),
	})
	changed := true
	if perr == nil {
		verifiedChanged := prev.VerifiedAt.Valid != verified.Valid ||
			(prev.VerifiedAt.Valid && verified.Valid && prev.VerifiedAt.String != verified.String)
		changed = prev.State != pr.GetState() ||
			prev.SizeBytes != int64(pr.GetSizeBytes()) ||
			verifiedChanged
	}
	if err := s.q.UpsertPlacement(ctx, db.UpsertPlacementParams{
		ArtifactID: art.ID, NodeID: nodeID, Path: pr.GetPath(), State: pr.GetState(),
		VerifiedAt: verified, Diagnostics: string(dj), SizeBytes: int64(pr.GetSizeBytes()),
	}); err != nil {
		return
	}
	if !changed {
		return
	}
	s.bus.Publish(ctx, "artifact.placement", nodeID, mustJSON(pr))
	s.deploys.OnPlacementReport(ctx, nodeID, identity, pr.GetState())
}

func optionalSQLString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

func (s *Server) applyTransferProgress(ctx context.Context, nodeID string, tp *agentv1.TransferProgress) {
	bytesTotal := int64(0)
	if tp.GetBytesTotal() > 0 {
		bytesTotal = int64(tp.GetBytesTotal())
	}
	_ = s.q.UpdateTransferProgress(ctx, db.UpdateTransferProgressParams{
		BytesDone: int64(tp.GetBytesDone()), BytesTotal: bytesTotal, ID: tp.GetTransferId(),
	})
	s.bus.Publish(ctx, "transfer.progress", nodeID, mustJSON(tp))
}

// rotateCertificate re-signs the node's current public key with a fresh
// validity window and sends the replacement certificate.
func (s *Server) rotateCertificate(ctx context.Context, nodeID string, conn *nodes.Conn) {
	cred, err := s.q.GetNodeCredential(ctx, nodeID)
	if err != nil {
		s.bus.Publish(ctx, "node.certificate_rotation_failed", nodeID,
			mustJSON(map[string]any{"error": "no credential on file"}))
		return
	}
	pub, err := ca.ParsePublicKeyPEM([]byte(cred.PublicKeyPem))
	if err != nil {
		s.bus.Publish(ctx, "node.certificate_rotation_failed", nodeID,
			mustJSON(map[string]any{"error": err.Error()}))
		return
	}
	certPEM, expires, err := s.ca.NodeCertFor(nodeID, "", pub, 90*24*time.Hour)
	if err != nil {
		s.bus.Publish(ctx, "node.certificate_rotation_failed", nodeID,
			mustJSON(map[string]any{"error": err.Error()}))
		return
	}
	serial, err := ca.SerialOf(certPEM)
	if err != nil {
		return
	}
	if err := s.q.UpsertNodeCredential(ctx, db.UpsertNodeCredentialParams{
		NodeID: nodeID, PublicKeyPem: cred.PublicKeyPem, Serial: serial,
		IssuedAt: dbTime(time.Now().UTC()), ExpiresAt: dbTime(expires),
	}); err != nil {
		return
	}
	if err := s.q.SetNodeCertificate(ctx, db.SetNodeCertificateParams{
		CertificateExpiresAt: sql.NullString{String: dbTime(expires), Valid: true},
		ID:                   nodeID,
	}); err != nil {
		return
	}
	conn.Send(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_Certificate{
		Certificate: &agentv1.Certificate{
			NodeCertificatePem: string(certPEM),
			CaCertificatePem:   string(s.ca.PEMCert()),
			ExpiresAt:          timestamppb.New(expires),
		},
	}})
	s.bus.Publish(ctx, "node.certificate_rotated", nodeID,
		mustJSON(map[string]any{"serial": serial}))
}

// uuidV7 returns a time-ordered UUIDv7 string (node IDs, token IDs).
func uuidV7() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	now := time.Now().UnixMilli()
	b[0] = byte(now >> 28)
	b[1] = byte(now >> 20)
	b[2] = byte(now >> 12)
	b[3] = byte(now >> 4)
	b[4] = byte(now<<4)&0x0F | 0x70 // version 7
	b[6] = byte(b[6])&0x3F | 0x80   // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
