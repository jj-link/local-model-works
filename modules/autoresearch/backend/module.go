// Package backend implements the first-party AutoResearch module.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

type Module struct {
	env                  *moduleapi.Env
	projectLocks         sync.Map
	credentialCleanupMu  sync.Mutex
	credentialCleanupErr error
}

// New builds the module from the core service surface.
func New(env *moduleapi.Env) moduleapi.Module {
	return &Module{env: env, credentialCleanupErr: scrubStartupCredentials(env.AutoResearchRoot)}
}

func (m *Module) ensureCredentialCleanup() error {
	m.credentialCleanupMu.Lock()
	defer m.credentialCleanupMu.Unlock()
	if m.credentialCleanupErr != nil {
		m.credentialCleanupErr = scrubStartupCredentials(m.env.AutoResearchRoot)
	}
	if m.credentialCleanupErr != nil {
		return fmt.Errorf("autoresearch.credential_cleanup_failed: %w", m.credentialCleanupErr)
	}
	return nil
}

func (m *Module) recordCredentialCleanupFailure(err error) {
	m.credentialCleanupMu.Lock()
	m.credentialCleanupErr = err
	m.credentialCleanupMu.Unlock()
}

func projectLeaseResource(projectID string) string {
	return "autoresearch-project:" + projectID
}

func projectLeaseResources(input map[string]any) []string {
	projectID, _ := input["project_id"].(string)
	if projectID == "" {
		return nil
	}
	return []string{projectLeaseResource(projectID)}
}

func (m *Module) lockProject(projectID string) func() {
	value, _ := m.projectLocks.LoadOrStore(projectID, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

func (m *Module) activeProjectOperation(ctx context.Context, projectID string) string {
	if m.env.Runs == nil {
		return ""
	}
	owners := m.env.Runs.ActiveOwners(ctx, projectLeaseResource(projectID))
	if len(owners) == 0 {
		return ""
	}
	return owners[0].OwnerKind + "/" + owners[0].OwnerID
}

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
			LeaseResources:  projectLeaseResources,
			ArtifactKinds:   []string{"research-workspace", "research-paper"},
			Executor:        m.runFactory,
		},
		{
			Kind: "autoresearch-paper-edit", Title: "AutoResearch paper edit",
			InputSchema: paperEditInputSchema, OutputSchema: runOutputSchema,
			SecretScopesFor: selectedSecretScopes,
			LeaseResources:  projectLeaseResources,
			ArtifactKinds:   []string{"research-workspace", "research-paper"},
			Executor:        m.runPaperEdit,
		},
		{
			Kind: "autoresearch-paper-compile", Title: "AutoResearch paper compile",
			InputSchema: paperCompileInputSchema, OutputSchema: runOutputSchema,
			SecretScopesFor: selectedSecretScopes,
			LeaseResources:  projectLeaseResources,
			ArtifactKinds:   []string{"research-paper"},
			Executor:        m.runPaperCompile,
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
    "provider_config":{"type":"object"},
    "candidate_count":{"type":"integer","minimum":1,"maximum":10},
    "prompt":{"type":"string"},
    "release":{"type":"boolean"},
    "paper_request":{"type":"string"},
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
    "provider_overrides":{"type":"object"},
    "provider_config":{"type":"object"}
  }
}`)

var paperCompileInputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id"],
  "properties":{
    "project_id":{"type":"string","format":"uuid"},
    "provider_config":{"type":"object"}
  }
}`)

var runOutputSchema = json.RawMessage(`{
  "type":"object","additionalProperties":false,
  "required":["project_id","changed_paths"],
  "properties":{
    "project_id":{"type":"string","format":"uuid"},
    "changed_paths":{"type":"array","items":{"type":"string"}},
    "before_digests":{"type":"object","additionalProperties":{"type":"string"}},
    "after_digests":{"type":"object","additionalProperties":{"type":"string"}},
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
	sort.Strings(scopes)
	return scopes
}
