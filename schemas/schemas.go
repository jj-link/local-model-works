// Package schemas is the single home of the product's JSON Schemas. Every
// consumer (CLI, installer, scheduler, agents, UI) validates against these
// embedded copies; there are no duplicate validators elsewhere.
package schemas

import "embed"

// FS contains the recipe, catalog, and module manifests schemas.
//
//go:embed recipe catalog module
var FS embed.FS

const (
	RecipeSchema  = "recipe/v1alpha1.schema.json"
	CatalogSchema = "catalog/v1alpha1.schema.json"
	ModuleSchema  = "module/v1alpha1.schema.json"
)
