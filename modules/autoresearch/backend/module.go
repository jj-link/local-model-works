// Package backend implements the first-party AutoResearch module.
package backend

import (
	"encoding/json"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

// Module owns AutoResearch persistence, HTTP APIs, and job orchestration.
type Module struct {
	env *moduleapi.Env
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

func (m *Module) RegisterSettings(reg *settings.Registry) {
	if err := reg.Register(descriptor.ID, descriptor.SettingsSchema); err != nil {
		panic(err)
	}
}

func (m *Module) RegisterHTTP(r chi.Router) { HandlerFromMux(m, r) }

func (m *Module) RegisterJobs(reg *jobs.Registry) {
	for _, spec := range []jobs.Spec{
		{
			Kind: "autoresearch-factory", Title: "AutoResearch factory",
			InputSchema: factoryInputSchema, OutputSchema: runOutputSchema,
			SecretScopesFor: selectedSecretScopes,
			ArtifactKinds:   []string{"research-workspace", "research-paper"},
			Executor:        m.runFactory,
		},
		{
			Kind: "autoresearch-paper-edit", Title: "AutoResearch paper edit",
			InputSchema: paperEditInputSchema, OutputSchema: runOutputSchema,
			SecretScopesFor: selectedSecretScopes,
			ArtifactKinds:   []string{"research-workspace", "research-paper"},
			Executor:        m.runPaperEdit,
		},
		{
			Kind: "autoresearch-paper-compile", Title: "AutoResearch paper compile",
			InputSchema: paperCompileInputSchema, OutputSchema: runOutputSchema,
			ArtifactKinds: []string{"research-paper"},
			Executor:      m.runPaperCompile,
		},
	} {
		if err := reg.Register(descriptor.ID, spec); err != nil {
			panic(err)
		}
	}
}

var factoryInputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id","factory"],
  "properties":{
    "project_id":{"type":"string","format":"uuid"},
    "factory":{"enum":["idea","proposal","deep_lit","experiment","paper"]},
    "parent_run_id":{"type":"string","format":"uuid"},
    "provider_overrides":{"type":"object"},
    "ssh_secret_name":{"type":"string"}
  }
}`)

var paperEditInputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id","message","base_etags"],
  "properties":{
    "project_id":{"type":"string","format":"uuid"},
    "message":{"type":"string","minLength":1},
    "base_etags":{"type":"object","additionalProperties":{"type":"string"}},
    "provider_overrides":{"type":"object"}
  }
}`)

var paperCompileInputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id"],
  "properties":{"project_id":{"type":"string","format":"uuid"}}
}`)

var runOutputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id","changed_paths"],
  "properties":{
    "project_id":{"type":"string","format":"uuid"},
    "changed_paths":{"type":"array","items":{"type":"string"}},
    "paper_path":{"type":"string"}
  }
}`)

func selectedSecretScopes(input map[string]any) []string {
	seen := map[string]struct{}{}
	var scopes []string
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if (key == "secret_name" || key == "ssh_secret_name") && child != nil {
					if name, ok := child.(string); ok && name != "" {
						if _, exists := seen[name]; !exists {
							seen[name] = struct{}{}
							scopes = append(scopes, name)
						}
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(input)
	return scopes
}
