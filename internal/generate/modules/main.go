// Command modules validates the first-party module manifests and emits
// internal/modules/registry_gen.go plus modules/<id>/backend/descriptor_gen.go.
// The source of truth is modules/<id>/module.yaml; it fails when a manifest
// violates schemas/module/v1alpha1.schema.json, a settings schema does not
// compile as draft 2020-12 JSON Schema, or the declared API fragment is
// missing.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/jj-link/local-model-works/internal/modules"
)

const (
	outRegistry = "internal/modules/registry_gen.go"
	outBackends = "internal/modules/backends_gen.go"
	outBackend  = "modules/%s/backend/descriptor_gen.go"
)

func main() {
	root := "."
	args := os.Args[1:]
	if len(args) < 1 || args[0] != "gen" {
		fmt.Fprintln(os.Stderr, "usage: modules gen [root]")
		os.Exit(2)
	}
	// Optional root override: generate against an alternate tree (tests).
	// All input and output paths are joined under it; the default "." keeps
	// `go run ./internal/generate/modules gen` byte-identical.
	if len(args) > 1 {
		root = args[1]
	}
	if err := gen(root); err != nil {
		fmt.Fprintln(os.Stderr, "modules:", err)
		os.Exit(1)
	}
}

// manifestDTO is the on-disk YAML shape; settingsSchema is decoded as a map
// (YAML mappings do not unmarshal into json.RawMessage) and converted to a
// JSON canonical form for the Manifest.
type manifestDTO struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	ID             string         `yaml:"id"`
	Title          string         `yaml:"title"`
	Route          string         `yaml:"route"`
	Nav            modules.Nav    `yaml:"nav"`
	SettingsSchema map[string]any `yaml:"settingsSchema"`
	JobKinds       []string       `yaml:"jobKinds"`
	ArtifactKinds  []string       `yaml:"artifactKinds"`
	APIFragment    string         `yaml:"apiFragment"`
	Capabilities   []string       `yaml:"capabilities"`
}

func (d manifestDTO) toManifest() (modules.Manifest, error) {
	m := modules.Manifest{
		APIVersion:    d.APIVersion,
		Kind:          d.Kind,
		ID:            d.ID,
		Title:         d.Title,
		Route:         d.Route,
		Nav:           d.Nav,
		JobKinds:      d.JobKinds,
		ArtifactKinds: d.ArtifactKinds,
		APIFragment:   d.APIFragment,
		Capabilities:  d.Capabilities,
	}
	if len(d.SettingsSchema) > 0 {
		raw, err := json.Marshal(d.SettingsSchema)
		if err != nil {
			return m, fmt.Errorf("settingsSchema: %w", err)
		}
		m.SettingsSchema = raw
	}
	return m, nil
}

func gen(root string) error {
	manifests, err := loadManifests(root)
	if err != nil {
		return err
	}
	if len(manifests) == 0 {
		return fmt.Errorf("no modules/*/module.yaml found")
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].ID < manifests[j].ID })

	schema, err := compileModuleSchema(root)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range manifests {
		if seen[m.ID] {
			return fmt.Errorf("duplicate module id %q", m.ID)
		}
		seen[m.ID] = true
		data, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", m.ID, err)
		}
		var doc any
		if err := json.Unmarshal(data, &doc); err != nil {
			return err
		}
		if err := schema.Validate(doc); err != nil {
			return fmt.Errorf("module %s invalid: %v", m.ID, err)
		}
		if len(m.SettingsSchema) > 0 {
			sc := jsonschema.NewCompiler()
			var sdoc any
			if err := json.Unmarshal(m.SettingsSchema, &sdoc); err != nil {
				return fmt.Errorf("module %s settings schema: %v", m.ID, err)
			}
			if err := sc.AddResource("settings://"+m.ID, sdoc); err != nil {
				return fmt.Errorf("module %s settings schema: %v", m.ID, err)
			}
			if _, err := sc.Compile("settings://" + m.ID); err != nil {
				return fmt.Errorf("module %s settings schema: %v", m.ID, err)
			}
		}
		if m.APIFragment != "" {
			frag := filepath.Join(root, "modules", m.ID, m.APIFragment)
			if st, err := os.Stat(frag); err != nil || st.Size() == 0 {
				return fmt.Errorf("module %s: api fragment %q missing or empty", m.ID, frag)
			}
		}
	}
	if err := writeRegistry(root, manifests); err != nil {
		return err
	}
	// Import aliases must be valid, distinct Go identifiers (module ids may
	// contain hyphens, identifiers may not).
	aliases := map[string]string{}
	for _, m := range manifests {
		a := goIdent(m.ID)
		if prev, dup := aliases[a]; dup {
			return fmt.Errorf("module %s: go import alias %q collides with module %s", m.ID, a, prev)
		}
		aliases[a] = m.ID
	}
	if err := writeBackends(root, manifests); err != nil {
		return err
	}
	for _, m := range manifests {
		if err := writeDescriptor(root, m); err != nil {
			return err
		}
	}
	return nil
}

func loadManifests(root string) ([]modules.Manifest, error) {
	dirs, err := os.ReadDir(filepath.Join(root, "modules"))
	if err != nil {
		return nil, fmt.Errorf("read modules tree: %w", err)
	}
	var out []modules.Manifest
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, "modules", d.Name(), "module.yaml")
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var dto manifestDTO
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&dto); err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		m, err := dto.toManifest()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, m)
	}
	return out, nil
}

func compileModuleSchema(root string) (*jsonschema.Schema, error) {
	abs, err := filepath.Abs(filepath.Join(root, "schemas/module/v1alpha1.schema.json"))
	if err != nil {
		return nil, err
	}
	id := "file://" + filepath.ToSlash(abs)
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var doc any
	if err := json.NewDecoder(f).Decode(&doc); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(id, doc); err != nil {
		return nil, err
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile module schema: %w", err)
	}
	return schema, nil
}

// writeRegistry emits the frozen, ID-sorted module registry.
func writeRegistry(root string, list []modules.Manifest) error {
	var b bytes.Buffer
	b.WriteString("// Code generated by \"go run ./internal/generate/modules gen\". DO NOT EDIT.\n")
	b.WriteString("// Source: modules/<id>/module.yaml; schema: schemas/module/v1alpha1.schema.json.\n\n")
	b.WriteString("package modules\n\n")
	b.WriteString("import \"encoding/json\"\n\n")
	b.WriteString("// Registry is the frozen, ID-sorted set of first-party modules.\n")
	b.WriteString("var Registry = []Manifest{\n")
	for _, m := range list {
		b.WriteString("\t" + literal(m, "Manifest", "Nav") + ",\n")
	}
	b.WriteString("}\n")
	return writeFile(filepath.Join(root, outRegistry), b.Bytes())
}

// writeBackends emits the compile-time backend constructor list — the only
// place core references a module's Go package. Each module's backend
// package must export New(env *moduleapi.Env) moduleapi.Module.
func writeBackends(root string, list []modules.Manifest) error {
	var b bytes.Buffer
	b.WriteString("// Code generated by \"go run ./internal/generate/modules gen\". DO NOT EDIT.\n")
	b.WriteString("// Source: modules/<id>/module.yaml.\n\n")
	b.WriteString("package modules\n\n")
	b.WriteString("import (\n\t\"github.com/jj-link/local-model-works/internal/moduleapi\"\n")
	for _, m := range list {
		b.WriteString(fmt.Sprintf("\t%s \"github.com/jj-link/local-model-works/modules/%s/backend\"\n", goIdent(m.ID), m.ID))
	}
	b.WriteString(")\n\n")
	b.WriteString("// Constructors is the compile-time list of first-party module backends,\n")
	b.WriteString("// aligned with the Registry (same order).\n")
	b.WriteString("var Constructors = []func(*moduleapi.Env) moduleapi.Module{\n")
	for _, m := range list {
		b.WriteString(fmt.Sprintf("\t%s.New,\n", goIdent(m.ID)))
	}
	b.WriteString("}\n")
	return writeFile(filepath.Join(root, outBackends), b.Bytes())
}

// writeDescriptor emits the module's frozen manifest literal for its backend
// package, so runtime code never re-reads YAML.
func writeDescriptor(root string, m modules.Manifest) error {
	p := filepath.Join(root, fmt.Sprintf(outBackend, m.ID))
	var b bytes.Buffer
	b.WriteString("// Code generated by \"go run ./internal/generate/modules gen\". DO NOT EDIT.\n")
	b.WriteString("// Source: " + filepath.ToSlash(filepath.Join("modules", m.ID, "module.yaml")) + "\n\n")
	b.WriteString("package backend\n\n")
	if len(m.SettingsSchema) > 0 {
		b.WriteString("import (\n\t\"encoding/json\"\n\n\t\"github.com/jj-link/local-model-works/internal/moduleapi\"\n)\n")
	} else {
		b.WriteString("import \"github.com/jj-link/local-model-works/internal/moduleapi\"\n")
	}
	b.WriteString("\n// descriptor is this module's frozen manifest.\n")
	b.WriteString("var descriptor = " + literal(m, "moduleapi.Descriptor", "moduleapi.Nav") + "\n")
	return writeFile(p, b.Bytes())
}

func writeFile(path string, data []byte) error {
	old, err := os.ReadFile(path)
	if err == nil && bytes.Equal(old, data) {
		fmt.Println(path, "up to date")
		return nil
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

// goIdent maps a module id (^[a-z][a-z0-9-]*$, hyphens allowed) to a valid
// Go identifier for use as an import alias. Hyphens become underscores.
func goIdent(id string) string {
	return strings.ReplaceAll(id, "-", "_")
}

// literal renders a manifest as a Go composite literal for the named type;
// navType is the fully qualified Nav type for that target package.
func literal(m modules.Manifest, t, navType string) string {
	var sb strings.Builder
	write := func(k string, v string) {
		if v == "" {
			return
		}
		sb.WriteString(", " + k + ": " + strconv.Quote(v))
	}
	writeList := func(k string, vals []string) {
		if len(vals) == 0 {
			return
		}
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = strconv.Quote(v)
		}
		sb.WriteString(", " + k + ": []string{" + strings.Join(quoted, ", ") + "}")
	}
	sb.WriteString(t + "{")
	sb.WriteString("APIVersion: " + strconv.Quote(m.APIVersion) +
		", Kind: " + strconv.Quote(m.Kind) +
		", ID: " + strconv.Quote(m.ID) +
		", Title: " + strconv.Quote(m.Title) +
		", Route: " + strconv.Quote(m.Route) +
		", Nav: " + navType + "{Label: " + strconv.Quote(m.Nav.Label) +
		", Order: " + strconv.Itoa(m.Nav.Order) +
		", Icon: " + strconv.Quote(m.Nav.Icon) + "}")
	if len(m.SettingsSchema) > 0 {
		sb.WriteString(", SettingsSchema: json.RawMessage(" + strconv.Quote(string(m.SettingsSchema)) + ")")
	}
	writeList("JobKinds", m.JobKinds)
	writeList("ArtifactKinds", m.ArtifactKinds)
	write("APIFragment", m.APIFragment)
	writeList("Capabilities", m.Capabilities)
	sb.WriteString("}")
	return sb.String()
}
