// Package backend implements the focused OpenHands/SWE-Gym data replication module.
package backend

import (
	"encoding/json"

	"github.com/go-chi/chi/v5"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/settings"
)

type Module struct {
	env   *moduleapi.Env
	cache *datasetCache
}

func New(env *moduleapi.Env) moduleapi.Module {
	return &Module{env: env, cache: newDatasetCache(env.RunRoot)}
}

func (m *Module) Descriptor() moduleapi.Descriptor { return descriptor }

func (m *Module) RegisterHTTP(r chi.Router) { HandlerFromMux(m, r) }

func (m *Module) RegisterSettings(reg *settings.Registry) {
	if err := reg.Register(descriptor.ID, descriptor.SettingsSchema); err != nil {
		panic(err)
	}
}

func (m *Module) RegisterJobs(reg *jobs.Registry) {
	register := func(spec jobs.Spec) {
		if err := reg.Register(descriptor.ID, spec); err != nil {
			panic(err)
		}
	}
	register(jobs.Spec{Kind: "trace-tokenize", Title: "Tokenize coding traces", InputSchema: objectSchema,
		OutputSchema: objectSchema, Executor: m.runTokenize})
	register(jobs.Spec{Kind: "trace-export", Title: "Export coding trace datasets", InputSchema: exportInputSchema,
		OutputSchema: objectSchema, Executor: m.runExport, ArtifactKinds: []string{"coding-trace-dataset"}})
	register(jobs.Spec{Kind: "trace-retention", Title: "Apply coding trace retention", InputSchema: objectSchema,
		OutputSchema: objectSchema, Executor: m.runRetention, Schedule: "24h"})
	register(jobs.Spec{Kind: "swe-gym-orchestrate", Title: "Orchestrate SWE-Gym replication", InputSchema: experimentInputSchema,
		OutputSchema: objectSchema, Executor: m.runOrchestrator})
	register(jobs.Spec{Kind: "swe-gym-rollout", Title: "Run OpenHands SWE-Gym rollout", InputSchema: rolloutInputSchema,
		OutputSchema: objectSchema, Executor: m.runRollout, SecretScopesFor: selectedSecrets})
	register(jobs.Spec{Kind: "swe-gym-grade", Title: "Grade SWE-Gym patch", InputSchema: gradeInputSchema,
		OutputSchema: objectSchema, Executor: m.runGrade, ArtifactKinds: []string{"swe-gym-report"}})
}

var objectSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`)
var experimentInputSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["experiment_id"],"properties":{"experiment_id":{"type":"string"}},"additionalProperties":false}`)
var exportInputSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["export_id","selection"],"properties":{"export_id":{"type":"string"},"selection":{"type":"object"}},"additionalProperties":false}`)
var rolloutInputSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["experiment_id","work_item_id","task","sampling","config"],"properties":{"experiment_id":{"type":"string"},"work_item_id":{"type":"string"},"task":{"type":"object"},"sampling":{"type":"object"},"config":{"type":"object"}},"additionalProperties":false}`)
var gradeInputSchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["trace_id","task","patch","node_id","timeout_seconds"],"properties":{"trace_id":{"type":"string"},"task":{"type":"object"},"patch":{"type":"string"},"node_id":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1}},"additionalProperties":false}`)

func selectedSecrets(input map[string]any) []string {
	config, _ := input["config"].(map[string]any)
	seen := map[string]bool{}
	var out []string
	for _, key := range []string{"secret_reference", "runtime_secret_reference"} {
		if value, ok := config[key].(string); ok && value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}
