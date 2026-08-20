// Package moduleapi is the leaf contract for first-party feature modules.
//
// A module is a self-contained feature tree (modules/<id>/) whose Go package
// implements Module. The generated registry (internal/modules) and the
// controller (internal/server) are the only consumers; core domain packages
// never import a module, so adding or removing a module never edits core
// switch statements.
package moduleapi

import (
	"database/sql"
	"encoding/json"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/recipebuilder"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/settings"
	"github.com/jj-link/local-model-works/internal/telemetry"
)

// Nav is the sidebar entry for a module.
type Nav struct {
	Label string `json:"label"`
	Order int    `json:"order"`
	Icon  string `json:"icon,omitempty"`
}

// Descriptor is the on-disk module manifest (modules/<id>/module.yaml) as a
// Go value; the generator emits one per module from the YAML source of truth.
type Descriptor struct {
	APIVersion     string          `json:"apiVersion"`
	Kind           string          `json:"kind"`
	ID             string          `json:"id"`
	Title          string          `json:"title"`
	Route          string          `json:"route"`
	Nav            Nav             `json:"nav"`
	SettingsSchema json.RawMessage `json:"settingsSchema,omitempty"`
	JobKinds       []string        `json:"jobKinds,omitempty"`
	ArtifactKinds  []string        `json:"artifactKinds,omitempty"`
	APIFragment    string          `json:"apiFragment,omitempty"`
	Capabilities   []string        `json:"capabilities,omitempty"`
}

// Env is the core service surface handed to every module. Core owns
// identity/auth, nodes/fabrics, artifacts, recipes, runs/schedules,
// placements/leases, secrets, and event streaming; modules add the
// operator-facing experience on top of these services.
type Env struct {
	Q             *db.Queries
	DB            *sql.DB
	Bus           *events.EventBus
	CA            *ca.CA
	Deploy        *deploy.Service
	Fabrics       *fabric.Service
	Recipes       *recipe.Service
	RecipeBuilder *recipebuilder.Service
	Runs          *runs.Service
	Jobs          *jobs.Registry
	Settings      *settings.Registry
	Secrets       *auth.SecretBox
	Telemetry     *telemetry.Service
	// Nodes is the live agent session registry (send workload/transfer/log
	// commands, query online state).
	Nodes *nodes.Registry
	// Commands awaits agent command results by command ID (with replay for
	// late waiters and cancelable subscriptions).
	Commands *commands.Broker
	// RunRoot is the controller run-state root (logs, job workspaces).
	RunRoot string
}

// Module is the first-party module contract.
//
// RegisterHTTP mounts the module's /api/v1 routes (the router passed in is
// the authenticated group; module routes must stay within the module's
// declared capability scope). RegisterJobs declares job kinds submitted
// through the shared job SDK; RegisterSettings declares the module's
// operator settings (validated against the manifest's settingsSchema).
type Module interface {
	Descriptor() Descriptor
	RegisterHTTP(r chi.Router)
	RegisterJobs(reg *jobs.Registry)
	RegisterSettings(reg *settings.Registry)
}
