// Package modules is the compile-time registry of first-party modules.
//
// The single source of truth is the on-disk manifest at modules/<id>/module.yaml
// (localmodelworks/v1alpha1). Running
// `go run ./internal/generate/modules gen` validates every manifest against
// schemas/module/v1alpha1.schema.json and emits registry_gen.go plus each
// module's frozen descriptor. Adding or removing a module is an edit to the
// module tree plus a regeneration.
package modules

import "encoding/json"

// Manifest is the on-disk module manifest (localmodelworks/v1alpha1).
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
