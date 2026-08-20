// Package fakeagent is a test-only integration harness that boots the REAL
// lmw-server and REAL node agents in one process over the real mTLS/Connect
// protocol, with the container runtime and hardware driver stubbed so no
// Docker daemon or GPU is required.
//
// The wiring mirrors cmd/lmw-server/main.go one-for-one; the harness adds no
// production behavior.
package fakeagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/agent"
	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/hardware"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/server"
)

// Server is a running real controller on temp (or shared) state.
type Server struct {
	T        *testing.T
	Root     string // state root (shared across restarts)
	Cfg      config.Server
	DB       *sql.DB
	Q        *db.Queries
	CA       *ca.CA
	Srv      *server.Server
	Ctx      context.Context
	Cancel   context.CancelFunc
	HTTPAddr string // bound "127.0.0.1:port"
	httpSrv  *http.Server
	stopped  bool
}

// FreeTCPPort reserves an ephemeral 127.0.0.1 port (bind, capture, release).
func FreeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// NewServer boots the real controller the same way cmd/lmw-server does:
// state root, SQLite, CA, mTLS agent listener, HTTP API. agentListen is the
// agent listener address ("127.0.0.1:0" for a fresh ephemeral port, or a
// previously returned AgentAddr() to restart in place).
func NewServer(t *testing.T, root, agentListen string) *Server {
	t.Helper()
	if root == "" {
		root = t.TempDir() + "/lmw-server"
	}
	for _, d := range []string{root, filepath.Join(root, "ca")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("state root: %v", err)
		}
	}
	cfg := config.Server{
		StateRoot:      root,
		HTTPAddr:       "127.0.0.1:0",
		AgentAddr:      agentListen,
		ServerName:     "localhost",
		PublicOrigin:   "https://lmw.example.test",
		PublicAgentURL: "https://localhost:9443",
		SessionTTL:     12 * time.Hour,
	}
	t.Cleanup(func() { _ = recipe.RemovePackage(cfg.RecipeRoot()) })
	// Materialize the recipe/catalog trust key once, mirroring
	// cmd/lmw-server (the recipe store loads it by path).
	if _, err := os.Stat(cfg.TrustKeyPath()); os.IsNotExist(err) {
		_, pub, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("trust key: %v", err)
		}
		der, err := x509.MarshalPKIXPublicKey(ed25519.PublicKey(pub))
		if err != nil {
			t.Fatalf("trust key: %v", err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
		if err := os.WriteFile(cfg.TrustKeyPath(), pemBytes, 0o600); err != nil {
			t.Fatalf("trust key: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	sqlDB, err := db.Open(ctx, cfg.DBPath())
	if err != nil {
		cancel()
		t.Fatalf("open db: %v", err)
	}
	q := db.New(sqlDB)
	count, err := q.CountUsers(ctx)
	if err != nil {
		cancel()
		sqlDB.Close()
		t.Fatalf("count test admins: %v", err)
	}
	if count == 0 {
		hash, err := auth.HashPassword("test-password")
		if err != nil {
			cancel()
			sqlDB.Close()
			t.Fatalf("hash test admin: %v", err)
		}
		if err := q.CreateUser(ctx, db.CreateUserParams{Username: "admin", Argon2Hash: hash}); err != nil {
			cancel()
			sqlDB.Close()
			t.Fatalf("create test admin: %v", err)
		}
	}
	chain, err := ca.LoadKeyCert(cfg.CAKeyPath(), cfg.CACertPath())
	if err != nil {
		cancel()
		sqlDB.Close()
		t.Fatalf("load CA: %v", err)
	}
	sessions := &auth.Sessions{
		TTL: cfg.SessionTTL,
		Create: func(tokenHash, username, csrfHash, expiresAt string) error {
			return q.CreateSession(ctx, db.CreateSessionParams{
				TokenHash: tokenHash, Username: username, CsrfHash: csrfHash,
				CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ExpiresAt: expiresAt,
			})
		},
		Get: func(tokenHash string) (string, string, string, error) {
			row, err := q.GetSessionByTokenHash(ctx, tokenHash)
			if err != nil {
				return "", "", "", err
			}
			return row.Username, row.CsrfHash, row.ExpiresAt, nil
		},
		Delete: func(tokenHash string) error {
			err := q.DeleteSessionByTokenHash(ctx, tokenHash)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			return nil
		},
	}
	srv := server.New(server.Deps{
		Ctx: ctx, Cfg: cfg, DB: sqlDB, Q: q, CA: chain, Sessions: sessions,
		HeartbeatTimeout: 8 * time.Second, Version: "fakeagent", Commit: "test",
	})
	if _, err := srv.Recover(ctx); err != nil {
		cancel()
		sqlDB.Close()
		t.Fatalf("recover: %v", err)
	}
	if _, err := srv.StartAgentListener(ctx); err != nil {
		cancel()
		sqlDB.Close()
		t.Fatalf("agent listener: %v", err)
	}
	ln, err := net.Listen("tcp", cfg.HTTPAddr)
	if err != nil {
		cancel()
		sqlDB.Close()
		t.Fatalf("http listener: %v", err)
	}
	httpSrv := &http.Server{Handler: srv.Routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.Serve(ln) }()

	s := &Server{
		T: t, Root: root, Cfg: cfg, DB: sqlDB, Q: q, CA: chain, Srv: srv,
		Ctx: ctx, Cancel: cancel, HTTPAddr: ln.Addr().String(), httpSrv: httpSrv,
	}
	t.Cleanup(func() { s.Stop() })
	return s
}

// AgentAddr is the bound mTLS agent listener address.
func (s *Server) AgentAddr() string { return s.Srv.AgentAddr() }

// Stop tears the server down: cancels the lifecycle context (which shuts the
// agent listener and drops its sessions) and drains the HTTP server.
func (s *Server) Stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	s.Cancel()
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = s.httpSrv.Shutdown(shCtx)
	// Give in-flight agent sessions a moment to observe the listener close.
	time.Sleep(300 * time.Millisecond)
	s.DB.Close()
}

// RestartServer stops s and boots a new controller on the same state root
// and the same agent listener port, so live agents reconnect unchanged.
func RestartServer(t *testing.T, s *Server) *Server {
	t.Helper()
	addr := s.AgentAddr()
	s.Stop()
	return NewServer(t, s.Root, addr)
}

// IssueToken mints a real one-use enrollment token (10-minute expiry) and
// returns the raw token string.
func (s *Server) IssueToken(t *testing.T) string {
	t.Helper()
	raw, err := ca.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	id, _ := newID()
	sum := auth.SHA256([]byte(raw))
	exp := time.Now().UTC().Add(ca.TokenTTL).Format("2006-01-02T15:04:05.000Z")
	if err := s.Q.CreateEnrollmentToken(s.Ctx, db.CreateEnrollmentTokenParams{
		ID: id, TokenHash: sum[:], ExpiresAt: exp,
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw
}

// IssueTokenExpiring mints a token with an explicit expiry (past = expired).
func (s *Server) IssueTokenExpiring(t *testing.T, exp time.Time) string {
	t.Helper()
	raw, err := ca.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	id, _ := newID()
	sum := auth.SHA256([]byte(raw))
	if err := s.Q.CreateEnrollmentToken(s.Ctx, db.CreateEnrollmentTokenParams{
		ID:        id,
		TokenHash: sum[:],
		ExpiresAt: exp.UTC().Format("2006-01-02T15:04:05.000Z"),
	}); err != nil {
		t.Fatalf("create token: %v", err)
	}
	return raw
}

// ApproveNode approves a pending node (the same query the fleet module runs).
func (s *Server) ApproveNode(t *testing.T, nodeID string) {
	t.Helper()
	if err := s.Q.ApproveNode(s.Ctx, nodeID); err != nil {
		t.Fatalf("approve node %s: %v", nodeID, err)
	}
}

// Node fetches one node row.
func (s *Server) Node(t *testing.T, nodeID string) db.Node {
	t.Helper()
	row, err := s.Q.GetNode(s.Ctx, nodeID)
	if err != nil {
		t.Fatalf("get node %s: %v", nodeID, err)
	}
	return row
}

// WaitNode polls the node row until cond holds (bounded, flake-free).
func (s *Server) WaitNode(t *testing.T, nodeID string, cond func(db.Node) bool, what string) db.Node {
	t.Helper()
	var row db.Node
	Deadline(t, 20*time.Second, func() bool {
		r, err := s.Q.GetNode(s.Ctx, nodeID)
		if err != nil {
			return false
		}
		row = r
		return cond(r)
	}, "node "+nodeID+" "+what)
	return row
}

// WaitOnline polls until the node is online with inventory recorded.
func (s *Server) WaitOnline(t *testing.T, nodeID string) db.Node {
	return s.WaitNode(t, nodeID, func(n db.Node) bool {
		return n.Status == "online" && n.Inventory.Valid
	}, "online with inventory")
}

// WaitPeerListen polls the node's inventory until it advertises addr as its
// peer-transfer endpoint. Unlike WaitOnline (any inventory satisfies it),
// this waits for a specific re-advertisement — required after an agent
// restart changes LMW_PEER_ADVERTISE, because the server reads the stored
// inventory row at StartTransfer time and a stale row would make the source
// dial the dead relay.
func (s *Server) WaitPeerListen(t *testing.T, nodeID, addr string) {
	t.Helper()
	s.WaitNode(t, nodeID, func(n db.Node) bool {
		return strings.Contains(n.Inventory.String, `"peer_listen":"`+addr+`"`)
	}, "inventory advertising peer "+addr)
}

// ---------------------------------------------------------------------------
// Agents
// ---------------------------------------------------------------------------

// AgentOpts configures one fake agent.
type AgentOpts struct {
	Hostname      string // display name / certificate hostname
	Token         string // one-use enrollment token (first run only)
	StateRoot     string // agent state root ("" = fresh temp dir)
	GPUs          int    // stub NVIDIA GPU count (default 1)
	IP            string // non-loopback interface address, e.g. "10.0.0.11/24"
	RDMA          bool   // expose an mlx5_0 RoCE device
	CacheRoots    []string
	PeerBind      string // peer-transfer bind address ("" = 127.0.0.1:0)
	PeerAdvertise string // peer-transfer advertised address ("" = derive)
}

// Agent is a running real node agent.
type Agent struct {
	T       *testing.T
	S       *Server
	A       *agent.Agent
	RT      *FakeRuntime
	DRV     *NVIDIADriver
	Ctx     context.Context
	Cancel  context.CancelFunc
	cfg     config.Agent
	runErr  chan error
	done    chan struct{} // closed when Run returns (independent of runErr consumption)
	stopped bool
}

// StartAgent boots a real agent pointed at s with the stub runtime and the
// NVIDIA-like driver. The stub runtime is shared per state root: restarting
// an agent (same state root = same identity) keeps its containers, mirroring
// the real deployment where the Docker daemon outlives the agent process.
func StartAgent(t *testing.T, s *Server, o AgentOpts) *Agent {
	t.Helper()
	if o.StateRoot == "" {
		o.StateRoot = t.TempDir() + "/lmw-agent"
	}
	if o.Hostname == "" {
		o.Hostname = "node-" + t.Name()
	}
	if o.GPUs == 0 {
		o.GPUs = 1
	}
	if o.PeerBind == "" {
		o.PeerBind = "127.0.0.1:0"
	}
	rt := runtimeFor(o.StateRoot)
	drv := NewNVIDIADriver(o.Hostname, o.GPUs, o.IP, o.RDMA)
	cfg := config.Agent{
		ServerURL:     "https://localhost:" + s.AgentAddr()[len("127.0.0.1:"):],
		CASha256:      s.CA.Fingerprint(),
		Token:         o.Token,
		StateRoot:     o.StateRoot,
		PeerAddr:      o.PeerBind,
		PeerAdvertise: o.PeerAdvertise,
		CacheRoots:    o.CacheRoots,
		TelemetryInt:  time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := agent.New(cfg, "fakeagent/1.0.0", "test", rt, drv)
	h := &Agent{T: t, S: s, A: a, RT: rt, DRV: drv, Ctx: ctx, Cancel: cancel, cfg: cfg, runErr: make(chan error, 1), done: make(chan struct{})}
	go func() {
		err := a.Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Logf("agent: Run exited early: %v", err)
		}
		h.runErr <- err
		close(h.done)
	}()
	t.Cleanup(h.Stop)
	return h
}

// runtimeFor returns the stub runtime for one agent state root, creating it
// on first use (see StartAgent).
var (
	rtMu sync.Mutex
	rts  = map[string]*FakeRuntime{}
)

func runtimeFor(stateRoot string) *FakeRuntime {
	rtMu.Lock()
	defer rtMu.Unlock()
	if rt, ok := rts[stateRoot]; ok {
		return rt
	}
	rt := NewFakeRuntime()
	rts[stateRoot] = rt
	return rt
}

// Stop cancels the agent and waits for Run to return. It waits on the done
// channel, not on runErr: the error value may already have been consumed by
// RunError/NodeID, and a consumed channel would make Stop wait the full
// timeout for an agent that has long since exited.
func (h *Agent) Stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.Cancel()
	select {
	case <-h.done:
	case <-time.After(15 * time.Second):
		h.T.Errorf("agent stop: run did not return")
	}
}

// NodeID returns the enrolled node id (waits for enrollment).
func (h *Agent) NodeID() string {
	deadline := time.Now().Add(20 * time.Second)
	for h.A.NodeID() == "" {
		if time.Now().After(deadline) {
			select {
			case err := <-h.runErr:
				h.T.Fatalf("agent never enrolled: %v", err)
			default:
			}
			h.T.Fatal("agent never enrolled")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return h.A.NodeID()
}

// RunError returns the agent's lifetime error if it exited early (e.g. a
// rejected enrollment); nil within timeout otherwise.
func (h *Agent) RunError(timeout time.Duration) error {
	select {
	case err := <-h.runErr:
		return err
	case <-time.After(timeout):
		return nil
	}
}

// ReconcileReasons returns the agent's recorded ReconcileRequest reasons.
func (h *Agent) ReconcileReasons() []string { return h.A.ReconcileReasons() }

// RestartAgent stops h and starts a new agent instance on the same state
// root (same identity), optionally with a new peer bind/advertise pair.
func (h *Agent) Restart(t *testing.T, peerBind, peerAdvertise string) *Agent {
	t.Helper()
	if peerBind == "" {
		peerBind = h.cfg.PeerAddr
	}
	if peerAdvertise == "" {
		peerAdvertise = h.cfg.PeerAdvertise
	}
	o := AgentOpts{
		Hostname:      h.DRV.hostname,
		StateRoot:     h.cfg.StateRoot,
		GPUs:          len(h.DRV.GPUs),
		IP:            h.DRV.ip,
		RDMA:          h.DRV.hasRDMA,
		CacheRoots:    h.cfg.CacheRoots,
		PeerBind:      peerBind,
		PeerAdvertise: peerAdvertise,
	}
	h.Stop()
	return StartAgent(t, h.S, o)
}

// ---------------------------------------------------------------------------
// Waiting helpers
// ---------------------------------------------------------------------------

// Deadline polls cond every 100ms until true or timeout.
func Deadline(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// newID returns a random UUIDv4-style id.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Compile-time assertions for the stubs.
var (
	_ runtime.Runtime = (*FakeRuntime)(nil)
	_ hardware.Driver = (*NVIDIADriver)(nil)
)
