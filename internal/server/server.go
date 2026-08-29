// Package server is the Local Model Works controller: the browser/CLI HTTP
// API, the agent mTLS listener, and the event stream.
package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/modules"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipe/repositorycompiler"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/servingtelemetry"
	"github.com/jj-link/local-model-works/internal/settings"
	"github.com/jj-link/local-model-works/internal/telemetry"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const sessionCookie = "lmw_session"

type userCtxKey struct{}

// Deps wires the server's dependencies.
type Deps struct {
	// Ctx is the process lifecycle context: jobs and schedules run against
	// it and stop when it is cancelled.
	Ctx              context.Context
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
	ctx              context.Context
	db               *sql.DB
	q                *db.Queries
	ca               *ca.CA
	sessions         *auth.Sessions
	bus              *events.EventBus
	nodes            *nodes.Registry
	commands         *commands.Broker
	fabrics          *fabric.Service
	jobs             *jobs.Registry
	settings         *settings.Registry
	telemetry        *telemetry.Service
	env              *moduleapi.Env
	modules          []moduleapi.Module
	heartbeatTimeout time.Duration
	version          string
	commit           string

	mu        sync.Mutex
	agentSrv  *http.Server
	agentAddr string

	runs    *runs.Service
	deploys *deploy.Service
}

// New assembles a Server: the core domain services, the first-party module
// backends (compiled in via internal/modules.Constructors), and the module
// job/settings registries.
func New(d Deps) *Server {
	hbt := d.HeartbeatTimeout
	if hbt <= 0 {
		hbt = 30 * time.Second
	}
	bus := events.NewEventBus(d.Q)
	nodes := nodes.NewRegistry()
	broker := commands.New()
	runRoot := d.Cfg.RunRoot()
	runsSvc := runs.New(d.DB, d.Q, bus, runRoot)
	deploys := deploy.New(d.DB, d.Q, bus, runsSvc, nodes, d.CA)
	go deploys.RunRepositoryUpdateCoordinator(d.Ctx)
	fabrics := fabric.New(d.Q, bus)
	jobsReg := jobs.New(runsSvc, runRoot, d.Ctx, d.DB, d.Q)
	settingsReg := settings.New(d.Q)
	telemetrySvc := telemetry.New(d.DB, d.Q)
	go telemetrySvc.RunRetention(d.Ctx)
	servingPoll := servingtelemetry.New(deploys, telemetrySvc)
	go servingPoll.Run(d.Ctx)

	v, err := recipe.NewValidator()
	if err != nil {
		panic(fmt.Sprintf("recipe validator: %v", err))
	}
	recipes, err := recipe.New(d.DB, d.Q, bus, v, d.Cfg.TrustKeyPath(), d.Cfg.CatalogRoot(), d.Cfg.RecipeRoot())
	if err != nil {
		panic(fmt.Sprintf("recipe store: %v", err))
	}
	recipes.SetRepositoryCompilerRegistry(repositorycompiler.NewRegistry(v))
	recipes.SetInstallHook(func() {
		nodes.Broadcast(&agentv1.ServerMessage{Body: &agentv1.ServerMessage_ReconcileRequest{
			ReconcileRequest: &agentv1.ReconcileRequest{Reason: "artifact.rescan"},
		}})
	})
	go recipes.RunUpdateChecker(d.Ctx, recipe.DefaultUpdateCheckInterval)
	builder := recipebuilder.New(d.Q, d.Cfg.StateRoot, v, recipes)
	box, err := auth.NewSecretBox(d.Cfg.SecretKeyPath())
	if err != nil {
		panic(fmt.Sprintf("secret box: %v", err))
	}
	jobsReg.SetSecretBox(box)
	env := &moduleapi.Env{
		Ctx: d.Ctx, Q: d.Q, DB: d.DB, Bus: bus, CA: d.CA,
		Deploy: deploys, Fabrics: fabrics, Recipes: recipes, RecipeBuilder: builder, Runs: runsSvc,
		Jobs: jobsReg, Settings: settingsReg, Secrets: box, Telemetry: telemetrySvc,
		Nodes: nodes, Commands: broker, RunRoot: runRoot,
	}
	s := &Server{
		cfg:              d.Cfg,
		ctx:              d.Ctx,
		db:               d.DB,
		q:                d.Q,
		ca:               d.CA,
		sessions:         d.Sessions,
		bus:              bus,
		nodes:            nodes,
		commands:         broker,
		fabrics:          fabrics,
		jobs:             jobsReg,
		settings:         settingsReg,
		telemetry:        telemetrySvc,
		env:              env,
		heartbeatTimeout: hbt,
		version:          d.Version,
		commit:           d.Commit,
		runs:             runsSvc,
		deploys:          deploys,
	}
	for _, ctor := range modules.Constructors {
		m := ctor(env)
		s.modules = append(s.modules, m)
		m.RegisterJobs(jobsReg)
		m.RegisterSettings(settingsReg)
	}
	jobsReg.StartSchedules()
	return s
}

// Bus is the event stream.
func (s *Server) Bus() *events.EventBus { return s.bus }

// Nodes is the live agent session registry.
func (s *Server) Nodes() *nodes.Registry { return s.nodes }

// Commands is the agent command-result broker.
func (s *Server) Commands() *commands.Broker { return s.commands }

// Env is the module service surface.
func (s *Server) Env() *moduleapi.Env { return s.env }

// Deployments is the serving deployment service.
func (s *Server) Deployments() *deploy.Service { return s.deploys }

// Runs is the run/state-machine service.
func (s *Server) Runs() *runs.Service { return s.runs }

// Recover marks one-shot runs interrupted by a previous crash and returns
// the count. Deployment dispatch resumes when agents reconnect (Converge).
func (s *Server) Recover(ctx context.Context) (int, error) {
	return s.runs.MarkInterrupted(ctx)
}

// Routes builds the browser/CLI HTTP API.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.securityHeaders)
	r.Route("/api/v1", func(v chi.Router) {
		v.Get("/healthz", s.handleHealth)
		v.Post("/login", s.handleLogin)
		v.Post("/browser-login", s.handleBrowserLogin)
		v.With(s.requireAuth).Group(func(a chi.Router) {
			a.Post("/logout", s.handleLogout)
			a.Get("/session", s.handleSession)
			a.Get("/system/info", s.handleSystemInfo)
			a.Get("/modules", s.handleModules)
			a.Get("/events", s.handleEvents)
			a.Get("/metrics", s.handleMetrics)
			a.Post("/migration/scan", s.handleMigrationScan)
			a.Post("/migration/import", s.handleMigrationImport)
			a.Post("/enrollment-tokens", s.handleCreateEnrollmentToken)
			a.Get("/enrollment-tokens", s.handleListEnrollmentTokens)
			a.Delete("/enrollment-tokens/{id}", s.handleDeleteEnrollmentToken)
			// First-party module routes (compiled list; the core keeps no
			// feature handlers).
			for _, m := range s.modules {
				m.RegisterHTTP(a)
			}
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, http.StatusNotFound, "resource.not_found", "no such endpoint")
			return
		}
		webServe(w, r)
	})
	return r
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	body, err := s.telemetry.Prometheus(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "telemetry.metrics", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// securityHeaders applies the baseline hardening response headers.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nonce, err := newCSPNonce()
		if err != nil {
			http.Error(w, "secure random unavailable", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", cspPolicy(nonce))
		next.ServeHTTP(w, r.WithContext(contextWithCSPNonce(r.Context(), nonce)))
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
		if mutating {
			if _, err := s.sessions.Validate(token, "", false); err != nil {
				writeErr(w, http.StatusUnauthorized, "auth.unauthorized", "missing or invalid session")
				return
			}
			expectedOrigin, err := s.cfg.NormalizedPublicOrigin()
			if err != nil || r.Header.Get("Origin") != expectedOrigin {
				writeErr(w, http.StatusForbidden, "auth.origin", "missing or invalid Origin")
				return
			}
		}
		sess, err := s.sessions.Validate(token, r.Header.Get("X-CSRF-Token"), mutating)
		if err != nil {
			if mutating {
				writeErr(w, http.StatusForbidden, "auth.csrf", "missing or invalid X-CSRF-Token")
				return
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
