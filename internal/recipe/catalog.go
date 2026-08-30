package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jj-link/local-model-works/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type recipeCatalog struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Recipes []catalogRecipe `json:"recipes"`
}

type catalogRecipe struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags,omitempty"`
	OCI     struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
	} `json:"oci"`
}

func compileCatalogSchema() (*jsonschema.Schema, error) {
	raw, err := schemas.FS.ReadFile(schemas.CatalogSchema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("localmodelworks/catalog/v1alpha1", doc); err != nil {
		return nil, err
	}
	return compiler.Compile("localmodelworks/catalog/v1alpha1")
}

func (s *Service) resolveCatalog(reference string) (catalogRecipe, error) {
	if s.catalogRoot == "" {
		return catalogRecipe{}, fmt.Errorf("%w: catalog is not configured", ErrUnknown)
	}
	indexRaw, err := os.ReadFile(filepath.Join(s.catalogRoot, "catalog.json"))
	if err != nil {
		return catalogRecipe{}, fmt.Errorf("catalog index: %w", err)
	}
	if len(indexRaw) > MaxConfigBytes {
		return catalogRecipe{}, fmt.Errorf("catalog index exceeds 1 MiB")
	}
	var raw any
	if err := json.Unmarshal(indexRaw, &raw); err != nil {
		return catalogRecipe{}, fmt.Errorf("catalog index: %w", err)
	}
	if err := s.catalogSchema.Validate(raw); err != nil {
		return catalogRecipe{}, fmt.Errorf("catalog schema: %w", err)
	}
	var catalog recipeCatalog
	if err := json.Unmarshal(indexRaw, &catalog); err != nil {
		return catalogRecipe{}, err
	}
	name, version := reference, ""
	if before, after, ok := strings.Cut(reference, ":"); ok {
		name, version = before, after
	}
	var matches []catalogRecipe
	for _, entry := range catalog.Recipes {
		if entry.Name == name && (version == "" || entry.Version == version) {
			matches = append(matches, entry)
		}
	}
	if len(matches) != 1 {
		return catalogRecipe{}, fmt.Errorf("%w: catalog reference %q resolved to %d entries", ErrUnknown, reference, len(matches))
	}
	return matches[0], nil
}
