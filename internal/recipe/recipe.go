// Package recipe is the canonical, content-addressed unit of installable
// local-AI capability. A recipe is strict localmodelworks/v1alpha1 YAML/JSON,
// validated against the embedded schema plus semantic rules, canonicalized
// to sorted-key JSON, and digested. Packaging produces a deterministic OCI
// layout; installation pulls from catalogs, OCI references, or pinned Git.
package recipe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// APIVersion is the recipe API version for this release.
const APIVersion = "localmodelworks/v1alpha1"

// MountPath is the fixed in-container mount for read-only package assets.
const MountPath = "/lmw/assets"

// Template variables allowed in workload/extension args and env.
const (
	TemplNodeID           = "${node.id}"
	TemplNodeRank         = "${node.rank}"
	TemplNodeAddress      = "${node.address}"
	TemplFabricAddr       = "${fabric.address}"
	TemplFabricNodeAddr   = "${fabric.node_address}"
	TemplFabricInterface  = "${fabric.interface}"
	TemplFabricRDMADevice = "${fabric.rdma_device}"
	TemplFabricGIDIndex   = "${fabric.gid_index}"
	TemplArtifact         = "${artifact." // + <name> + ".path}"
	TemplProfile          = "${profile."  // + <name> + "}"
)

// Manifest is the typed view of a validated recipe document. Field names
// mirror the JSON Schema (camelCase on the wire).
type Manifest struct {
	APIVersion    string         `json:"apiVersion"`
	Kind          string         `json:"kind"`
	Metadata      Metadata       `json:"metadata"`
	Compatibility Compatibility  `json:"compatibility"`
	Artifacts     []Artifact     `json:"artifacts"`
	Parameters    []Parameter    `json:"parameters,omitempty"`
	Profiles      map[string]any `json:"profiles,omitempty"`
	Workloads     []Workload     `json:"workloads"`
	Assets        []string       `json:"assets,omitempty"`
	Prepare       *Extension     `json:"prepare,omitempty"`
	Verify        *Extension     `json:"verify,omitempty"`
}

type Metadata struct {
	Name        string  `json:"name"`
	Version     string  `json:"version"`
	Model       string  `json:"model,omitempty"`
	Engine      string  `json:"engine,omitempty"`
	Description string  `json:"description,omitempty"`
	License     string  `json:"license,omitempty"`
	Source      *Source `json:"source,omitempty"`
}

type Source struct {
	URL      string `json:"url"`
	Revision string `json:"revision,omitempty"`
	Path     string `json:"path,omitempty"`
}

// RepositoryIdentity returns the stable identity of a repository-backed
// recipe. Commits are versions and are deliberately excluded from identity.
func RepositoryIdentity(source Source) (id, normalizedURL, normalizedPath string, err error) {
	rawURL := strings.TrimSpace(source.URL)
	if rawURL == "" {
		return "", "", "", fmt.Errorf("recipe repository: source URL is required")
	}

	normalizedURL = strings.TrimRight(rawURL, "/")
	parsed, parseErr := url.Parse(rawURL)
	if parseErr == nil && strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), "github.com") {
		if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", "", "", fmt.Errorf("recipe repository: GitHub URL must not contain credentials, query, or fragment")
		}
		parsed.Scheme = "https"
		parsed.Host = "github.com"
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		if strings.HasSuffix(strings.ToLower(parsed.Path), ".git") {
			parsed.Path = parsed.Path[:len(parsed.Path)-4]
		}
		normalizedURL = parsed.String()
	}
	if normalizedURL == "" {
		return "", "", "", fmt.Errorf("recipe repository: source URL is required")
	}

	rawPath := strings.ReplaceAll(strings.TrimSpace(source.Path), "\\", "/")
	normalizedPath = path.Clean(rawPath)
	if rawPath == "" {
		normalizedPath = "."
	}
	if normalizedPath == ".." || strings.HasPrefix(normalizedPath, "../") || path.IsAbs(normalizedPath) {
		return "", "", "", fmt.Errorf("recipe repository: source path %q escapes the repository", source.Path)
	}
	id = "repo-" + hex.EncodeToString([]byte(normalizedURL+"\n"+normalizedPath))
	return id, normalizedURL, normalizedPath, nil
}

type Compatibility struct {
	NodeCount   int           `json:"nodeCount"`
	Accelerator *AccCompat    `json:"accelerator,omitempty"`
	Fabric      *FabricCompat `json:"fabric,omitempty"`
}

type AccCompat struct {
	Vendor         string   `json:"vendor,omitempty"`
	Architectures  []string `json:"architectures,omitempty"`
	Count          int      `json:"count,omitempty"`
	MinMemoryBytes int64    `json:"minMemoryBytes,omitempty"`
	Features       []string `json:"features,omitempty"`
}

type FabricCompat struct {
	Transport        string `json:"transport,omitempty"`
	MinBandwidthGbps int    `json:"minBandwidthGbps,omitempty"`
}

type Artifact struct {
	Name           string       `json:"name"`
	Kind           string       `json:"kind"`
	SizeBytes      int64        `json:"sizeBytes,omitempty"`
	Source         *ArtSource   `json:"source,omitempty"`
	DefaultVariant string       `json:"defaultVariant,omitempty"`
	Variants       []ArtVariant `json:"variants,omitempty"`
	Mount          string       `json:"mount"`
	Validation     string       `json:"validation,omitempty"`
}

// ArtVariant is one selectable model variant of an artifact. Exactly one of
// Artifact.Source or Artifact.Variants is present (schema oneOf).
type ArtVariant struct {
	Name        string    `json:"name"`
	Label       string    `json:"label,omitempty"`
	Description string    `json:"description,omitempty"`
	Source      ArtSource `json:"source"`
}

// EffectiveSource resolves the artifact's source for a deployment: the
// static source when the artifact has no variants, otherwise the named
// variant's source (defaultVariant when variant is empty). Returns an error
// for a missing source or an unknown variant so the wrong model is never
// silently deployed.
func (a *Artifact) EffectiveSource(variant string) (*ArtSource, error) {
	if len(a.Variants) == 0 {
		if a.Source == nil {
			return nil, fmt.Errorf("artifact %q has no source or variants", a.Name)
		}
		return a.Source, nil
	}
	name := variant
	if name == "" {
		name = a.DefaultVariant
	}
	if name == "" {
		return nil, fmt.Errorf("artifact %q declares variants but no defaultVariant", a.Name)
	}
	for _, v := range a.Variants {
		if v.Name == name {
			return &v.Source, nil
		}
	}
	return nil, fmt.Errorf("artifact %q has no variant %q", a.Name, name)
}

type ArtSource struct {
	Type     string `json:"type"`
	Identity string `json:"identity"`
	Revision string `json:"revision,omitempty"`
	Digest   string `json:"digest,omitempty"`
}

type Parameter struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Default     any      `json:"default,omitempty"`
	Description string   `json:"description,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	MinLength   *int     `json:"minLength,omitempty"`
	MaxLength   *int     `json:"maxLength,omitempty"`
	Enum        []any    `json:"enum,omitempty"`
}

type Workload struct {
	Match           *Match            `json:"match,omitempty"`
	Image           Image             `json:"image"`
	Command         []string          `json:"command"`
	Args            []string          `json:"args"`
	Env             map[string]string `json:"env,omitempty"`
	Ports           []Port            `json:"ports,omitempty"`
	Resources       Resources         `json:"resources"`
	Devices         *Devices          `json:"devices,omitempty"`
	NetworkMode     string            `json:"networkMode,omitempty"`
	Readiness       *Probe            `json:"readiness,omitempty"`
	Verify          *Probe            `json:"verify,omitempty"`
	Ranks           []int             `json:"ranks,omitempty"`
	StartOrder      string            `json:"startOrder,omitempty"`
	HostPreparation *HostPreparation  `json:"hostPreparation,omitempty"`
	Permissions     []string          `json:"permissions,omitempty"`
}

// HostPreparation declares the narrow host-memory controls LMW applies after
// image pull and container creation, immediately before model loading.
type HostPreparation struct {
	RequireSwap   bool `json:"requireSwap,omitempty"`
	Swappiness    *int `json:"swappiness,omitempty"`
	DropPageCache bool `json:"dropPageCache,omitempty"`
}

type Match struct {
	Accelerator *MatchAcc `json:"accelerator,omitempty"`
	NodeCount   int       `json:"nodeCount,omitempty"`
}

type MatchAcc struct {
	Vendor        string   `json:"vendor,omitempty"`
	Architectures []string `json:"architectures,omitempty"`
	Features      []string `json:"features,omitempty"`
}

type Image struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type Port struct {
	Container int    `json:"container"`
	Host      int    `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

type Resources struct {
	CPU         float64 `json:"cpu,omitempty"`
	CPUSetCpus  string  `json:"cpusetCpus,omitempty"`
	MemoryBytes int64   `json:"memoryBytes,omitempty"`
	ShmBytes    int64   `json:"shmBytes,omitempty"`
	TmpfsBytes  int64   `json:"tmpfsBytes,omitempty"`
	Pids        int     `json:"pids,omitempty"`
}

type Devices struct {
	Accelerator *DevAcc  `json:"accelerator,omitempty"`
	RDMA        *DevRdma `json:"rdma,omitempty"`
}

type DevAcc struct {
	All     bool  `json:"all,omitempty"`
	Indices []int `json:"indices,omitempty"`
}

type DevRdma struct {
	All     bool     `json:"all,omitempty"`
	Devices []string `json:"devices,omitempty"`
}

type Probe struct {
	HTTPGet          *HTTPGet `json:"httpGet,omitempty"`
	Exec             []string `json:"exec,omitempty"`
	IntervalSeconds  int      `json:"intervalSeconds,omitempty"`
	TimeoutSeconds   int      `json:"timeoutSeconds,omitempty"`
	FailureThreshold int      `json:"failureThreshold,omitempty"`
	Expect           *Expect  `json:"expect,omitempty"`
}

type HTTPGet struct {
	Path   string `json:"path"`
	Port   int    `json:"port"`
	Method string `json:"method,omitempty"`
	Body   string `json:"body,omitempty"`
}

type Expect struct {
	StatusCode   *int           `json:"statusCode,omitempty"`
	BodyContains string         `json:"bodyContains,omitempty"`
	JSON         map[string]any `json:"json,omitempty"`
}

type Extension struct {
	Image            Image             `json:"image"`
	Command          []string          `json:"command"`
	Args             []string          `json:"args"`
	Env              map[string]string `json:"env,omitempty"`
	Network          string            `json:"network,omitempty"`
	TimeoutSeconds   int               `json:"timeoutSeconds,omitempty"`
	OutputLimitBytes int               `json:"outputLimitBytes,omitempty"`
	OutputSchema     map[string]any    `json:"outputSchema"`
}

// YAMLOrJSON converts a recipe document (YAML preferred, JSON accepted) to
// raw JSON bytes.
func YAMLOrJSON(data []byte) ([]byte, error) {
	var v any
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("recipe JSON: %w", err)
		}
	} else {
		if err := yaml.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("recipe YAML: %w", err)
		}
	}
	return json.Marshal(v)
}

// Canonical returns the canonical (sorted-key, compact) JSON of a document.
func Canonical(doc []byte) ([]byte, error) {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("recipe canonical: %w", err)
	}
	return json.Marshal(v)
}

// DigestOf returns sha256:<hex> over the canonical manifest bytes.
func DigestOf(doc []byte) (string, error) {
	canon, err := Canonical(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Parse decodes a canonical document into the typed manifest.
func Parse(doc []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, fmt.Errorf("recipe parse: %w", err)
	}
	return &m, nil
}

// HighRiskPermissions returns the permission set implied by the manifest that
// install/launch previews must surface to the operator.
func (m *Manifest) HighRiskPermissions() []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, w := range m.Workloads {
		if w.NetworkMode == "host" {
			add("network.host")
		}
		if w.Devices != nil && (w.Devices.RDMA != nil && (w.Devices.RDMA.All || len(w.Devices.RDMA.Devices) > 0)) {
			add("devices.rdma")
		}
		if w.Resources.ShmBytes >= 1024*1024*1024 {
			add("memory.shm-large")
		}
		for _, p := range w.Permissions {
			add(p)
		}
	}
	if m.Prepare != nil && m.Prepare.Network == "egress" {
		add("network.egress")
	}
	if m.Verify != nil && m.Verify.Network == "egress" {
		add("network.egress")
	}
	sort.Strings(out)
	return out
}

var templateVar = regexp.MustCompile(`\$\{([^}]*)\}`)

// TemplateVars lists every ${...} occurrence in the given strings.
func TemplateVars(vals ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vals {
		for _, m := range templateVar.FindAllStringSubmatch(v, -1) {
			name := "${" + m[1] + "}"
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// IsTemplateVar reports whether s is one of the declared template variables.
func IsTemplateVar(s string) bool {
	switch s {
	case TemplNodeID, TemplNodeRank, TemplNodeAddress, TemplFabricAddr,
		TemplFabricNodeAddr, TemplFabricInterface, TemplFabricRDMADevice, TemplFabricGIDIndex:
		return true
	}
	if strings.HasPrefix(s, TemplArtifact) && strings.HasSuffix(s, ".path}") {
		name := strings.TrimSuffix(strings.TrimPrefix(s, TemplArtifact), ".path}")
		return assetNamePattern.MatchString(name)
	}
	if strings.HasPrefix(s, TemplProfile) && strings.HasSuffix(s, "}") {
		return true
	}
	return false
}

var assetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,62}$`)

// Render replaces declared template variables with concrete values.
func (m *Manifest) Render(v string, ctx RenderContext) (string, error) {
	var err error
	out := templateVar.ReplaceAllStringFunc(v, func(t string) string {
		got, ok := ctx.Resolve(t)
		if !ok {
			err = fmt.Errorf("unresolved template %s", t)
			return t
		}
		return got
	})
	if err != nil {
		return "", err
	}
	return out, nil
}

// RenderContext resolves template variables for a specific rank/node.
type RenderContext struct {
	NodeID           string
	NodeRank         int
	NodeAddress      string
	FabricAddr       string
	FabricNodeAddr   string
	FabricInterface  string
	FabricRDMADevice string
	FabricGIDIndex   string
	Artifacts        map[string]string // artifact name -> node-local path
	Profiles         map[string]any
}

// Resolve returns the concrete value for a template variable.
func (c RenderContext) Resolve(v string) (string, bool) {
	switch v {
	case TemplNodeID:
		return c.NodeID, true
	case TemplNodeRank:
		return fmt.Sprintf("%d", c.NodeRank), true
	case TemplNodeAddress:
		return c.NodeAddress, true
	case TemplFabricAddr:
		return c.FabricAddr, true
	case TemplFabricNodeAddr:
		return c.FabricNodeAddr, true
	case TemplFabricInterface:
		return c.FabricInterface, true
	case TemplFabricRDMADevice:
		return c.FabricRDMADevice, true
	case TemplFabricGIDIndex:
		return c.FabricGIDIndex, true
	}
	if strings.HasPrefix(v, TemplArtifact) && strings.HasSuffix(v, ".path}") {
		name := strings.TrimSuffix(strings.TrimPrefix(v, TemplArtifact), ".path}")
		p, ok := c.Artifacts[name]
		return p, ok
	}
	if strings.HasPrefix(v, TemplProfile) && strings.HasSuffix(v, "}") {
		name := strings.TrimSuffix(strings.TrimPrefix(v, TemplProfile), "}")
		vv, ok := c.Profiles[name]
		return formatProfileValue(vv), ok
	}
	return "", false
}

func formatProfileValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return fmt.Sprintf("%v", t)
	case int:
		return fmt.Sprintf("%d", t)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// SelectWorkload picks the first variant whose capability predicate is
// satisfied by the target. Predicate semantics are empty-match-everything;
// an empty predicate may only appear on the last variant (validated).
func (m *Manifest) SelectWorkload(target Target) (int, *Workload, error) {
	for i := range m.Workloads {
		w := &m.Workloads[i]
		ok, err := w.Match.SatisfiedBy(target)
		if err != nil {
			return -1, nil, err
		}
		if ok {
			return i, w, nil
		}
	}
	return -1, nil, fmt.Errorf("no workload variant matches target")
}

// Target describes one node's capabilities for variant selection.
type Target struct {
	NodeCount    int
	Vendor       string
	Architecture string
	Features     []string
}

// SatisfiedBy reports whether the variant predicate holds for t.
func (mt *Match) SatisfiedBy(t Target) (bool, error) {
	if mt == nil {
		return true, nil
	}
	if mt.NodeCount != 0 && mt.NodeCount != t.NodeCount {
		return false, nil
	}
	a := mt.Accelerator
	if a == nil {
		return true, nil
	}
	if a.Vendor != "" && a.Vendor != t.Vendor {
		return false, nil
	}
	if len(a.Architectures) > 0 && !contains(a.Architectures, t.Architecture) {
		return false, nil
	}
	for _, f := range a.Features {
		if !contains(t.Features, f) {
			return false, nil
		}
	}
	return true, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ProfileValues returns the effective parameter values: schema defaults,
// overridden by the named profile's validated values.
func (m *Manifest) ProfileValues(profile string) (map[string]any, error) {
	out := map[string]any{}
	for _, p := range m.Parameters {
		if p.Default != nil {
			out[p.Name] = p.Default
		}
	}
	if profile == "" {
		return out, nil
	}
	pv, ok := m.Profiles[profile]
	if !ok {
		return nil, fmt.Errorf("profile %q not defined", profile)
	}
	obj, ok := pv.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("profile %q malformed", profile)
	}
	known := map[string]bool{}
	for _, p := range m.Parameters {
		known[p.Name] = true
	}
	for k := range obj {
		if !known[k] {
			return nil, fmt.Errorf("profile %q sets undeclared parameter %q", profile, k)
		}
	}
	for k, v := range obj {
		out[k] = v
	}
	return out, nil
}

// ArtifactByName returns the named artifact definition.
func (m *Manifest) ArtifactByName(name string) *Artifact {
	for i := range m.Artifacts {
		if m.Artifacts[i].Name == name {
			return &m.Artifacts[i]
		}
	}
	return nil
}

// HasFabricRequirement reports whether the recipe demands a fabric.
func (m *Manifest) HasFabricRequirement() bool {
	return m.Compatibility.Fabric != nil
}
