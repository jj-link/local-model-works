// Package backend registers the Workshop first-party module. Workshop is a
// read-only composition of core fleet, fabric, and serving resources.
package backend

import (
	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

type Module struct{}

func New(*moduleapi.Env) moduleapi.Module             { return &Module{} }
func (m *Module) Descriptor() moduleapi.Descriptor    { return descriptor }
func (m *Module) RegisterHTTP(chi.Router)             {}
func (m *Module) RegisterJobs(*jobs.Registry)         {}
func (m *Module) RegisterSettings(*settings.Registry) {}
