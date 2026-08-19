// Package modules is the compile-time registry of first-party modules.
//
// Manifests is the single source of truth for the module set; running
// `go run ./internal/generate/modules gen` validates every manifest against
// schemas/module/v1alpha1.schema.json and emits registry_gen.go. Adding or
// removing a module is an edit to this file plus a regeneration.
package modules

import "encoding/json"

// Manifest is the union of the on-disk module manifest
// (localmodelworks/v1alpha1) and the /modules API fields.
type Manifest struct {
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

// Nav is the sidebar entry for a module.
type Nav struct {
	Label string `json:"label"`
	Order int    `json:"order"`
	Icon  string `json:"icon,omitempty"`
}

const apiVersion = "localmodelworks/v1alpha1"

func settings(s string) json.RawMessage { return json.RawMessage(s) }

// Manifests is the first-party module set.
var Manifests = []Manifest{
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "fleet",
		Title:          "Fleet",
		Route:          "/fleet",
		Nav:            Nav{Label: "Fleet", Order: 10, Icon: "fleet"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{}}`),
		Capabilities:   []string{"nodes.read", "fabrics.read", "events.publish"},
	},
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "library",
		Title:          "Library",
		Route:          "/library",
		Nav:            Nav{Label: "Library", Order: 20, Icon: "library"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"catalog_urls":{"type":"array","items":{"type":"string","format":"uri"}},"auto_trust_local":{"type":"boolean"}}}`),
		JobKinds:       []string{"recipe-import"},
		ArtifactKinds:  []string{"model", "dataset", "adapter", "checkpoint", "recipe", "file"},
		Capabilities:   []string{"recipes.write", "artifacts.read", "transfers.write", "secrets.read", "events.publish"},
	},
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "serving",
		Title:          "Serving",
		Route:          "/serving",
		Nav:            Nav{Label: "Serving", Order: 30, Icon: "serving"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"verify_timeout_seconds":{"type":"integer","minimum":5,"maximum":600}}}`),
		JobKinds:       []string{"serve", "stop"},
		ArtifactKinds:  []string{"model"},
		Capabilities:   []string{"deployments.write", "runs.create", "nodes.read", "transfers.write", "secrets.read", "events.publish"},
	},
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "benchmarks",
		Title:          "Benchmarks",
		Route:          "/benchmarks",
		Nav:            Nav{Label: "Benchmarks", Order: 40, Icon: "benchmarks"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{"default_prompts_per_language":{"type":"integer","minimum":1,"maximum":256},"default_max_tokens":{"type":"integer","minimum":16,"maximum":16384},"languages":{"type":"array","items":{"type":"string"},"uniqueItems":true}}}`),
		JobKinds:       []string{"benchmark"},
		Capabilities:   []string{"runs.create", "deployments.read", "events.publish"},
	},
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "runs",
		Title:          "Runs",
		Route:          "/runs",
		Nav:            Nav{Label: "Runs", Order: 50, Icon: "runs"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{}}`),
		Capabilities:   []string{"runs.read", "events.publish"},
	},
	{
		APIVersion:     apiVersion,
		Kind:           "Module",
		ID:             "settings",
		Title:          "Settings",
		Route:          "/settings",
		Nav:            Nav{Label: "Settings", Order: 90, Icon: "settings"},
		SettingsSchema: settings(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"properties":{}}`),
		Capabilities:   []string{"secrets.read", "secrets.write", "modules.settings"},
	},
}
