// Package server is the Local Model Works controller: the browser/CLI HTTP
// API, the agent mTLS listener, and the event stream.
package server

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/runs"
)

const sessionCookie = "lmw_session"

type userCtxKey struct{}

// Deps wires the server's dependencies.
type Deps struct {
	Cfg              config.Server
	DB               *sql.DB
	Q                *db.Queries
	CA               *ca.CA
	Sessions         *auth.Sessions
	HeartbeatTimeout time.Duration // default 30s
	Version          string
	Commit           string
}

// Server is the controller.
type Server struct {
	cfg              config.Server
	q                *db.Queries
	ca               *ca.CA
	sessions         *auth.Sessions
	bus              *events.EventBus
	nodes            *NodeRegistry
	heartbeatTimeout time.Duration
	version          string
	commit           string

	mu        sync.Mutex
	agentSrv  *http.Server
	agentAddr string

	runs    *runs.Service
	deploys *deploy.Service
}

// New assembles a Server.
func New(d Deps) *Server {
	hbt := d.HeartbeatTimeout
	if hbt <= 0 {
		hbt = 30 * time.Second
	}
	bus := events.NewEventBus(d.Q)
	nodes := NewNodeRegistry()
	runRoot := filepath.Join(d.Cfg.StateRoot, "runs")
	runsSvc := runs.New(d.DB, d.Q, bus, runRoot)
	deploys := deploy.New(d.DB, d.Q, bus, runsSvc, nodes, d.CA)
	return &Server{
		cfg:              d.Cfg,
		q:                d.Q,
		ca:               d.CA,
		sessions:         d.Sessions,
		bus:              bus,
		nodes:            nodes,
		heartbeatTimeout: hbt,
		version:          d.Version,
		commit:           d.Commit,
		runs:             runsSvc,
		deploys:          deploys,
	}
}

// Bus is the event stream.
func (s *Server) Bus() *events.EventBus { return s.bus }

// Nodes is the live agent session registry.
func (s *Server) Nodes() *NodeRegistry { return s.nodes }

// Deployments is the serving deployment service.
func (s *Server) Deployments() *deploy.Service { return s.deploys }

// Runs is the run/state-machine service.
func (s *Server) Runs() *runs.Service { return s.runs }

// Recover marks one-shot runs interrupted by a previous crash and returns
// the count. Deployment dispatch resumes when agents reconnect (Converge).
func (s *Server) Recover(ctx context.Context) (int, error) {
	return s.runs.MarkInterrupted(ctx)
}
// BootstrapAdmin creates the initial operator account when none exists.
func BootstrapAdmin(ctx context.Context, q *db.Queries, username, password string) error {
	n, err := q.CountUsers(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	return q.CreateUser(ctx, db.CreateUserParams{Username: username, Argon2Hash: hash})
}

// Routes builds the browser/CLI HTTP API.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.securityHeaders)
	r.Route("/api/v1", func(v chi.Router) {
		v.Get("/healthz", s.handleHealth)
		v.Post("/login", s.handleLogin)
		v.With(s.requireAuth).Group(func(a chi.Router) {
			a.Post("/logout", s.handleLogout)
			a.Get("/session", s.handleSession)
			a.Get("/system/info", s.handleSystemInfo)
			a.Get("/modules", s.handleModules)
			a.Get("/events", s.handleEvents)
			a.Post("/enrollment-tokens", s.handleCreateEnrollmentToken)
			a.Get("/enrollment-tokens", s.handleListEnrollmentTokens)
			a.Delete("/enrollment-tokens/{id}", s.handleDeleteEnrollmentToken)
			a.Get("/nodes", s.handleListNodes)
			a.Get("/nodes/{id}", s.handleGetNode)
			a.Post("/nodes/{id}/approve", s.handleApproveNode)
			a.Get("/deployments", s.handleListDeployments)
			a.Post("/deployments", s.handleCreateDeployment)
			a.Post("/deployments/plan", s.handlePlanDeployment)
			a.Get("/deployments/{id}", s.handleGetDeployment)
			a.Post("/deployments/{id}/stop", s.handleStopDeployment)
			a.Post("/deployments/{id}/verify", s.handleVerifyDeployment)
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "resource.not_found", "no such endpoint")
			return
		}
		webIndex(w, r)
	})
	return r
}

// securityHeaders applies the baseline hardening response headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:")
		next.ServeHTTP(w, r)
	})
}

// requireAuth validates the session cookie and, for mutating requests, the
// X-CSRF-Token against the session-bound CSRF token.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if cookie, err := r.Cookie(sessionCookie); err == nil {
			token = cookie.Value
		}
		mutating := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodDelete || r.Method == http.MethodPatch
		csrf := r.Header.Get("X-CSRF-Token")
		sess, err := s.sessions.Validate(token, csrf, mutating)
		if err != nil {
			if mutating {
				if _, e := s.sessions.Validate(token, "", false); e == nil {
					writeErr(w, http.StatusForbidden, "auth.csrf", "missing or invalid X-CSRF-Token")
					return
				}
			}
			writeErr(w, http.StatusUnauthorized, "auth.unauthorized", "missing or invalid session")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userCtxKey{}, sess.Username)))
	})
}

// AgentAddr is the bound address of the mTLS listener (after
// StartAgentListener).
func (s *Server) AgentAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentAddr
}

// dbTime renders timestamps in the schema's canonical format.
func dbTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// parseDBTime parses the schema's canonical timestamp.
func parseDBTime(v string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", v); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, v)
}

// ensureRunRoot guarantees the run-state root exists.
func (s *Server) ensureRunRoot() error {
	return os.MkdirAll(s.cfg.RunRoot(), 0o755)
}

// runLogPath is the append-only log file for one deployment rank and
// stream. It returns "" when a path component could escape RunRoot.
func (s *Server) runLogPath(runID, deploymentID string, rank int32, stream string) string {
	if deploymentID == "" {
		deploymentID = "adhoc"
	}
	// runID and deploymentID arrive from the agent; refuse any component
	// that could escape RunRoot.
	if runID == "" || runID == "." || runID == ".." || deploymentID == "." ||
		deploymentID == ".." || filepath.Base(runID) != runID || filepath.Base(deploymentID) != deploymentID {
		return ""
	}
	if stream != "stdout" && stream != "stderr" {
		stream = "stdout"
	}
	return filepath.Join(s.cfg.RunRoot(), runID, "logs", deploymentID,
		filepath.Base(strings.ReplaceAll(deploymentID, " ", "_"))+"-rank"+itoa(rank)+"_"+stream+".log")
}

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
