// Package backend implements authenticated chat completions against running
// Local Model Works deployments.
package backend

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module is the Chat backend.
type Module struct {
	getDeployment func(context.Context, string) (*deploy.Deployment, error)
	client        *http.Client
}

// New builds the module from the core deployment service.
func New(env *moduleapi.Env) moduleapi.Module {
	return &Module{
		getDeployment: env.Deploy.Get,
		client:        &http.Client{Timeout: 10 * time.Minute},
	}
}

func (m *Module) Descriptor() moduleapi.Descriptor    { return descriptor }
func (m *Module) RegisterJobs(*jobs.Registry)         {}
func (m *Module) RegisterSettings(*settings.Registry) {}

// RegisterHTTP mounts Chat inside the authenticated module router.
func (m *Module) RegisterHTTP(r chi.Router) { HandlerFromMux(m, r) }
