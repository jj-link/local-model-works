package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Diagnostic is a stable validation finding. Code is machine-readable and
// stable across versions; Message is human-readable.
type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // error | warning
	Message  string `json:"message"`
	Path     string `json:"path,omitempty"`
}

func errDiag(code, msg, path string) Diagnostic {
	return Diagnostic{Code: code, Severity: "error", Message: msg, Path: path}
}

// Validator compiles the recipe schema once and reuses it.
type Validator struct {
	schema *jsonschema.Schema
}

func NewValidator() (*Validator, error) {
	raw, err := schemas.FS.ReadFile(schemas.RecipeSchema)
	if err != nil {
		return nil, fmt.Errorf("embed recipe schema: %w", err)
	}
	// Decoded before AddResource: the v6 compiler validates resources
	// against the meta-schema and does not decode raw io.Reader documents
	// itself.
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("compile recipe schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("localmodelworks/v1alpha1", doc); err != nil {
		return nil, fmt.Errorf("compile recipe schema: %w", err)
	}
	s, err := compiler.Compile("localmodelworks/v1alpha1")
	if err != nil {
		return nil, fmt.Errorf("compile recipe schema: %w", err)
	}
	return &Validator{schema: s}, nil
}

// Validate runs the JSON schema plus every semantic rule and returns all
// diagnostics. An empty result means the recipe is launchable.
func (v *Validator) Validate(doc []byte) ([]Diagnostic, error) {
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return []Diagnostic{errDiag("recipe.parse", "recipe is not valid JSON: "+err.Error(), "")}, nil
	}
	var diags []Diagnostic
	if verr := v.schema.Validate(raw); verr != nil {
		for _, line := range strings.Split(verr.Error(), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			diags = append(diags, errDiag("recipe.schema", line, ""))
		}
		return diags, nil
	}
	m, err := Parse(doc)
	if err != nil {
		return []Diagnostic{errDiag("recipe.parse", err.Error(), "")}, nil
	}
	diags = append(diags, semanticDiagnostics(m)...)
	sort.Slice(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		return diags[i].Code < diags[j].Code
	})
	return diags, nil
}

// ValidateStrict validates and additionally requires zero diagnostics.
func (v *Validator) ValidateStrict(doc []byte) (*Manifest, []Diagnostic, error) {
	diags, err := v.Validate(doc)
	if err != nil {
		return nil, diags, err
	}
	if len(diags) > 0 {
		return nil, diags, nil
	}
	m, err := Parse(doc)
	if err != nil {
		return nil, []Diagnostic{errDiag("recipe.parse", err.Error(), "")}, nil
	}
	return m, diags, nil
}

var (
	latestTag   = regexp.MustCompile(`(^|:)latest$`)
	traversalRe = regexp.MustCompile(`(^|/)\.\.(/|$)`)
	sha40       = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

func semanticDiagnostics(m *Manifest) []Diagnostic {
	var diags []Diagnostic
	add := func(code, msg, path string) { diags = append(diags, errDiag(code, msg, path)) }

	// Image references: mutable :latest tags are rejected outright.
	checkImage := func(img Image, path string) {
		if latestTag.MatchString(img.Reference) {
			add("recipe.image-latest", "mutable :latest image tag is not launchable; pin a digest", path+".image.reference")
		}
	}
	for i := range m.Workloads {
		checkImage(m.Workloads[i].Image, fmt.Sprintf("workloads[%d]", i))
	}
	if m.Prepare != nil {
		checkImage(m.Prepare.Image, "prepare")
	}
	if m.Verify != nil {
		checkImage(m.Verify.Image, "verify")
	}

	// Mounts: no traversal, no duplicates.
	seenMounts := map[string]bool{}
	for i := range m.Artifacts {
		a := m.Artifacts[i]
		p := fmt.Sprintf("artifacts[%d]", i)
		if traversalRe.MatchString(a.Mount) {
			add("recipe.mount-traversal", "artifact mount contains path traversal", p+".mount")
		}
		if sensitiveMount(a.Mount) {
			add("recipe.mount-sensitive", fmt.Sprintf("artifact mount %q targets a sensitive host path", a.Mount), p+".mount")
		}
		if seenMounts[a.Mount] {
			add("recipe.mount-duplicate", "two artifacts mount the same in-container path", p+".mount")
		}
		seenMounts[a.Mount] = true
		checkSrc := func(src ArtSource, path string) {
			if src.Type == "huggingface" && !sha40.MatchString(src.Revision) {
				add("recipe.hf-unpinned", "huggingface artifact requires a 40-hex revision", path+".revision")
			}
			if src.Type == "file" && src.Digest == "" {
				add("recipe.file-unpinned", "file artifact requires a sha256 digest", path+".digest")
			}
		}
		if len(a.Variants) > 0 {
			if a.DefaultVariant == "" {
				add("recipe.variant-default", "artifact declares variants but no defaultVariant", p+".defaultVariant")
			}
			seenVariants := map[string]bool{}
			for vi, v := range a.Variants {
				if seenVariants[v.Name] {
					add("recipe.variant-duplicate", "duplicate variant name", fmt.Sprintf("%s.variants[%d].name", p, vi))
				}
				seenVariants[v.Name] = true
				checkSrc(v.Source, fmt.Sprintf("%s.variants[%d].source", p, vi))
			}
			if a.DefaultVariant != "" && !seenVariants[a.DefaultVariant] {
				add("recipe.variant-default-unknown", "defaultVariant does not match any variant name", p+".defaultVariant")
			}
		} else if a.Source != nil {
			checkSrc(*a.Source, p+".source")
		}
	}

	// Assets: relative, no traversal (re-checked here because the schema
	// `not` is syntax-level; the packer re-checks after normalization).
	seenAssets := map[string]bool{}
	for i, a := range m.Assets {
		p := fmt.Sprintf("assets[%d]", i)
		if strings.HasPrefix(a, "/") || traversalRe.MatchString(a) {
			add("recipe.asset-path", "asset paths must be relative without traversal", p)
		}
		if seenAssets[a] {
			add("recipe.asset-duplicate", "duplicate asset path", p)
		}
		seenAssets[a] = true
	}

	// Parameters: unique names; defaults within declared ranges.
	paramNames := map[string]bool{}
	for i, p := range m.Parameters {
		px := fmt.Sprintf("parameters[%d]", i)
		if paramNames[p.Name] {
			add("recipe.param-duplicate", "duplicate parameter name", px+".name")
		}
		paramNames[p.Name] = true
		if p.Default != nil {
			if d := checkParamValue(p, p.Default); d != "" {
				add("recipe.param-default", d, px+".default")
			}
		}
	}

	// Profiles: only declared parameters, values within ranges.
	profileNames := make([]string, 0, len(m.Profiles))
	for name, pv := range m.Profiles {
		profileNames = append(profileNames, name)
		px := fmt.Sprintf("profiles[%s]", name)
		obj, ok := pv.(map[string]any)
		if !ok {
			add("recipe.profile-shape", "profile must be an object", px)
			continue
		}
		for k, val := range obj {
			p := findParam(m.Parameters, k)
			if p == nil {
				add("recipe.profile-undeclared", fmt.Sprintf("profile sets undeclared parameter %q", k), px+"."+k)
				continue
			}
			if d := checkParamValue(*p, val); d != "" {
				add("recipe.profile-value", d, px+"."+k)
			}
		}
	}
	sort.Strings(profileNames)

	// Templates: every ${...} used must be declared and resolvable.
	checkTempl := func(vals []string, path string, env map[string]string) {
		for _, s := range vals {
			for _, tv := range TemplateVars(s) {
				if err := checkTemplateVar(tv, m, profileNames); err != nil {
					add("recipe.template", fmt.Sprintf("%s in %s", err.Error(), tv), path)
				}
			}
		}
		for k, v := range env {
			for _, tv := range TemplateVars(v) {
				if err := checkTemplateVar(tv, m, profileNames); err != nil {
					add("recipe.template", fmt.Sprintf("%s in %s", err.Error(), tv), path+".env."+k)
				}
			}
		}
	}
	for i := range m.Workloads {
		w := m.Workloads[i]
		checkTempl(w.Args, fmt.Sprintf("workloads[%d].args", i), w.Env)
	}
	if m.Prepare != nil {
		checkTempl(m.Prepare.Args, "prepare.args", m.Prepare.Env)
	}
	if m.Verify != nil {
		checkTempl(m.Verify.Args, "verify.args", m.Verify.Env)
	}

	// Workloads: duplicate container ports, declared permission coverage.
	for i := range m.Workloads {
		w := m.Workloads[i]
		p := fmt.Sprintf("workloads[%d]", i)
		ports := map[int]bool{}
		for _, pt := range w.Ports {
			if ports[pt.Container] {
				add("recipe.port-duplicate", fmt.Sprintf("container port %d declared twice", pt.Container), p+".ports")
			}
			ports[pt.Container] = true
		}
		perms := map[string]bool{}
		for _, x := range w.Permissions {
			perms[x] = true
		}
		if w.NetworkMode == "host" && !perms["network.host"] {
			add("recipe.permission-missing", "host networking requires the network.host permission", p+".permissions")
		}
		if w.Devices != nil && w.Devices.RDMA != nil && (w.Devices.RDMA.All || len(w.Devices.RDMA.Devices) > 0) && !perms["devices.rdma"] {
			add("recipe.permission-missing", "RDMA devices require the devices.rdma permission", p+".permissions")
		}
		if w.Resources.ShmBytes >= 1024*1024*1024 && !perms["memory.shm-large"] {
			add("recipe.permission-missing", "shared memory >= 1 GiB requires the memory.shm-large permission", p+".permissions")
		}
		if w.HostPreparation != nil && !perms["host.memory-tuning"] {
			add("recipe.permission-missing", "host memory preparation requires the host.memory-tuning permission", p+".permissions")
		}
	}

	// Variant predicates: duplicates rejected; empty predicate only last.
	seenPred := map[string]bool{}
	for i := range m.Workloads {
		w := m.Workloads[i]
		p := fmt.Sprintf("workloads[%d].match", i)
		key, _ := json.Marshal(w.Match)
		if string(key) == "null" {
			if i != len(m.Workloads)-1 {
				add("recipe.variant-ambiguous", "catch-all variant (no match predicate) must be the last variant", p)
				continue
			}
			if seenPred["null"] {
				add("recipe.variant-ambiguous", "multiple catch-all variants are ambiguous", p)
			}
			seenPred["null"] = true
			continue
		}
		if seenPred[string(key)] {
			add("recipe.variant-ambiguous", "duplicate capability predicate makes variant selection ambiguous", p)
		}
		seenPred[string(key)] = true
	}

	// Multi-node recipes must declare rank handling in at least one variant.
	if m.Compatibility.NodeCount > 1 {
		anyRanks := false
		for i := range m.Workloads {
			if len(m.Workloads[i].Ranks) > 0 {
				anyRanks = true
				break
			}
		}
		if !anyRanks {
			add("recipe.ranks-missing", "multi-node recipe must declare ranks on at least one variant", "workloads")
		}
	}
	return diags
}

func findParam(ps []Parameter, name string) *Parameter {
	for i := range ps {
		if ps[i].Name == name {
			return &ps[i]
		}
	}
	return nil
}

// sensitiveMount reports whether the in-container mount path overlaps a
// host path recipes must never bind: the runtime Docker socket (Docker-in-
// Docker) and the kernel pseudo-filesystem roots. The schema's mount
// pattern admits all of these, so the policy rejects them here.
func sensitiveMount(mount string) bool {
	switch mount {
	case "/var/run/docker.sock", "/run/docker.sock":
		return true
	}
	for _, root := range []string{"/dev", "/proc", "/sys"} {
		if strings.HasPrefix(mount, root+"/") {
			return true
		}
	}
	return false
}

func checkParamValue(p Parameter, val any) string {
	switch p.Type {
	case "int":
		n, ok := asNumber(val)
		if !ok {
			return fmt.Sprintf("parameter %q expects int", p.Name)
		}
		if n != float64(int64(n)) {
			return fmt.Sprintf("parameter %q expects integer value", p.Name)
		}
		if p.Min != nil && n < *p.Min {
			return fmt.Sprintf("parameter %q below minimum", p.Name)
		}
		if p.Max != nil && n > *p.Max {
			return fmt.Sprintf("parameter %q above maximum", p.Name)
		}
	case "float":
		n, ok := asNumber(val)
		if !ok {
			return fmt.Sprintf("parameter %q expects number", p.Name)
		}
		if p.Min != nil && n < *p.Min {
			return fmt.Sprintf("parameter %q below minimum", p.Name)
		}
		if p.Max != nil && n > *p.Max {
			return fmt.Sprintf("parameter %q above maximum", p.Name)
		}
	case "bool":
		if _, ok := val.(bool); !ok {
			return fmt.Sprintf("parameter %q expects bool", p.Name)
		}
	case "string":
		s, ok := val.(string)
		if !ok {
			return fmt.Sprintf("parameter %q expects string", p.Name)
		}
		if p.MinLength != nil && len(s) < *p.MinLength {
			return fmt.Sprintf("parameter %q shorter than minLength", p.Name)
		}
		if p.MaxLength != nil && len(s) > *p.MaxLength {
			return fmt.Sprintf("parameter %q longer than maxLength", p.Name)
		}
	case "enum":
		for _, e := range p.Enum {
			if fmt.Sprintf("%v", e) == fmt.Sprintf("%v", val) {
				return ""
			}
		}
		return fmt.Sprintf("parameter %q not in enum", p.Name)
	}
	return ""
}

func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

func checkTemplateVar(tv string, m *Manifest, profileNames []string) error {
	switch tv {
	case TemplNodeID, TemplNodeRank, TemplNodeAddress:
		return nil
	case TemplFabricAddr, TemplFabricNodeAddr, TemplFabricInterface, TemplFabricRDMADevice, TemplFabricGIDIndex:
		if !m.HasFabricRequirement() {
			return fmt.Errorf("fabric template used by a recipe without a fabric requirement")
		}
		return nil
	}
	if strings.HasPrefix(tv, TemplArtifact) && strings.HasSuffix(tv, ".path}") {
		name := strings.TrimSuffix(strings.TrimPrefix(tv, TemplArtifact), ".path}")
		if name == "package" {
			return nil
		}
		if m.ArtifactByName(name) == nil {
			return fmt.Errorf("template references undeclared artifact %q", name)
		}
		return nil
	}
	if strings.HasPrefix(tv, TemplProfile) && strings.HasSuffix(tv, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(tv, TemplProfile), "}")
		for _, p := range profileNames {
			if p == name {
				return nil
			}
		}
		return fmt.Errorf("template references undeclared profile %q", name)
	}
	return fmt.Errorf("undeclared template variable %s", tv)
}
