// Package migrate implements `lmw migrate dgx-dashboard scan|import`: a
// deterministic, read-only scan of the legacy DGX-Dashboard serving catalog
// and production state into a digest-addressed migration plan, and an import
// that loads recipes, historical runs, benchmark results, and model cache
// placements into the new control plane's state with full verification.
package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jj-link/local-model-works/internal/cjson"
)

// PlanSchema is the migration plan document version.
const PlanSchema = "lmw/migration-plan/v1"

// LegacyModule is the module identity for imported historical runs.
const LegacyModule = "legacy"

// PlanDigestOf computes sha256 over the canonical JSON of the plan with the
// digest field cleared: the same resolved inputs always yield the same
// digest.
func PlanDigestOf(p *Plan) string {
	cp := *p
	cp.Digest = ""
	b, err := cjson.Marshal(cp)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ScanOptions are the resolved inputs for one scan (or the import's
// re-scan). Every field is explicit so a re-scan can be byte-identical.
type ScanOptions struct {
	LegacyDir  string          // legacy repository checkout (contains control/)
	StateDir   string          // legacy state root (runs/, benchmark-results/, ...)
	INIPath    string          // production INI; default <StateDir>/config-production.ini
	CacheRoots []CacheRootSpec // explicit --cache-root node=path entries
	Docker     bool            // query the docker daemon for mutable image digests
}

// CacheRootSpec is one explicit node=root binding.
type CacheRootSpec struct {
	Node string
	Path string
}

// PlanNode is one resolved node with its cache roots and placements.
type PlanNode struct {
	ID         string      `json:"id"`
	CacheRoots []CacheRoot `json:"cache_roots"`
}

// CacheRoot is one resolved model/cache root.
type CacheRoot struct {
	Path         string      `json:"path"`
	Exists       bool        `json:"exists"`
	Backend      string      `json:"backend"`
	Repositories []string    `json:"repositories,omitempty"`
	Placements   []Placement `json:"placements"`
}

// Placement is one existing model tree registered as an artifact placement.
type Placement struct {
	Node        string   `json:"node"`
	Identity    string   `json:"identity"`
	Path        string   `json:"path"`
	Revision    string   `json:"revision,omitempty"`
	SizeBytes   int64    `json:"size_bytes"`
	State       string   `json:"state"` // verified | failed
	Diagnostics []string `json:"diagnostics,omitempty"`
}

// ImageRef is one container image reference with its resolved digest.
type ImageRef struct {
	Reference    string `json:"reference"`
	Digest       string `json:"digest"`
	Mutable      bool   `json:"mutable"`
	DigestSource string `json:"digest_source"` // pinned | docker | derived
}

// FileRef identifies one legacy file by relative path and content digest.
type FileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// RecipeEntry is one converted single-node recipe: either a single package
// or the capability-merged variants of identical target copies.
type RecipeEntry struct {
	Name          string          `json:"name"`
	Engine        string          `json:"engine"`
	Packages      []string        `json:"packages"`
	Targets       []string        `json:"targets"`
	Architectures []string        `json:"architectures"`
	Served        string          `json:"served"`
	Model         string          `json:"model"`
	ModelRevision string          `json:"model_revision,omitempty"`
	Image         ImageRef        `json:"image"`
	Merged        bool            `json:"merged"`
	Contract      json.RawMessage `json:"contract"`
	Digest        string          `json:"digest"`
	Document      json.RawMessage `json:"document"`
}

// ClusterDraft is one catalog cluster package awaiting hand conversion to
// ranked declarative workloads; the file fixtures carry the legacy contract
// for comparison.
type ClusterDraft struct {
	Name           string           `json:"name"`
	Engine         string           `json:"engine"`
	Served         string           `json:"served"`
	ContainerName  string           `json:"container_name"`
	Image          ImageRef         `json:"image"`
	Model          string           `json:"model"`
	ModelRevision  string           `json:"model_revision,omitempty"`
	HeadHost       string           `json:"head_host"`
	WorkerHost     string           `json:"worker_host"`
	APIPort        int              `json:"api_port"`
	DefaultProfile string           `json:"default_profile,omitempty"`
	Profiles       []ClusterProfile `json:"profiles,omitempty"`
	Capabilities   json.RawMessage  `json:"capabilities,omitempty"`
	Ranks          []ClusterRank    `json:"ranks"`
	Files          []FileRef        `json:"files"`
}

// ClusterProfile is one parsed launch profile (the four catalog fields).
type ClusterProfile struct {
	Name              string `json:"name"`
	KVCacheDtype      string `json:"kv_cache_dtype"`
	ContextLength     int    `json:"context_length"`
	MaxSequences      int    `json:"max_sequences"`
	SpeculativeTokens int    `json:"speculative_tokens"`
}

// ClusterRank is one fabric rank's legacy host assignment.
type ClusterRank struct {
	Rank          int    `json:"rank"`
	Host          string `json:"host"`
	ContainerName string `json:"container_name"`
}

// RunEntry is one legacy run classified for import.
type RunEntry struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	LegacyState    string          `json:"legacy_state"`
	State          string          `json:"state"`
	ErrorCode      string          `json:"error_code,omitempty"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Nonterminal    bool            `json:"nonterminal,omitempty"`
	RecipeDigest   string          `json:"recipe_digest,omitempty"`
	LegacyIdentity string          `json:"legacy_identity,omitempty"`
	CreatedAt      string          `json:"created_at"`
	StartedAt      string          `json:"started_at,omitempty"`
	FinishedAt     string          `json:"finished_at,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
	Resources      []string        `json:"resources,omitempty"`
	LogSize        int64           `json:"log_size"`
	LogSHA256      string          `json:"log_sha256"`
}

// IndexEntry is one result-index.json entry.
type IndexEntry struct {
	File    string        `json:"file"`
	Size    int64         `json:"size"`
	MTimeNS int64         `json:"mtime_ns"`
	Summary *IndexSummary `json:"summary,omitempty"`
}

// IndexSummary is the subset of an index summary the migration consumes.
type IndexSummary struct {
	RunID     string `json:"run_id,omitempty"`
	Language  string `json:"language,omitempty"`
	Served    string `json:"served,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Quant     string `json:"quant,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
}

// BenchmarkPlan carries the benchmark state scan.
type BenchmarkPlan struct {
	IndexVersion int          `json:"index_version"`
	IndexEntries int          `json:"index_entries"`
	ResultsFiles int          `json:"results_files"`
	AiderFiles   int          `json:"aider_files"`
	ResultsDir   string       `json:"results_dir"`
	AiderDir     string       `json:"aider_dir"`
	Index        []IndexEntry `json:"index"`
}

// Stray is one discovered non-catalog tree the migration never imports.
type Stray struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Counts are the plan's category tallies.
type Counts struct {
	SingleNodePackages    int `json:"single_node_packages"`
	SingleNodeRecipes     int `json:"single_node_recipes"`
	MergedRecipes         int `json:"merged_recipes"`
	ClusterPackages       int `json:"cluster_packages"`
	RunsTerminal          int `json:"runs_terminal"`
	RunsNonterminal       int `json:"runs_nonterminal"`
	Strays                int `json:"strays"`
	MutableImages         int `json:"mutable_images"`
	Placements            int `json:"placements"`
	PlacementFailures     int `json:"placement_failures"`
	BenchmarkIndexEntries int `json:"benchmark_index_entries"`
	BenchmarkResultsFiles int `json:"benchmark_results_files"`
	AiderBenchmarkFiles   int `json:"aider_benchmark_files"`
}

// Plan is the full migration plan. Every field is deterministic for a fixed
// set of inputs; Digest covers the canonical JSON of the rest.
type Plan struct {
	Schema        string         `json:"schema"`
	Digest        string         `json:"digest"`
	Nodes         []PlanNode     `json:"nodes"`
	Recipes       []RecipeEntry  `json:"recipes"`
	ClusterDrafts []ClusterDraft `json:"cluster_drafts"`
	Runs          []RunEntry     `json:"runs"`
	Benchmarks    BenchmarkPlan  `json:"benchmarks"`
	Strays        []Stray        `json:"strays"`
	Counts        Counts         `json:"counts"`
}

// Report is the operator-facing scan result: the plan plus report-only
// (non-digest) observations.
type Report struct {
	Plan       Plan
	Digest     string
	Containers []LegacyContainer
	Warnings   []string
}

// LegacyContainer is one currently running legacy serving container.
type LegacyContainer struct {
	Name   string `json:"name"`
	Image  string `json:"image"`
	Status string `json:"status"`
}

// SortPlan normalizes every plan slice in place so two scans of the same
// inputs render byte-identical canonical JSON.
func SortPlan(p *Plan) {
	sort.Slice(p.Nodes, func(i, j int) bool { return p.Nodes[i].ID < p.Nodes[j].ID })
	for i := range p.Nodes {
		sort.Slice(p.Nodes[i].CacheRoots, func(a, b int) bool {
			return p.Nodes[i].CacheRoots[a].Path < p.Nodes[i].CacheRoots[b].Path
		})
		for j := range p.Nodes[i].CacheRoots {
			r := &p.Nodes[i].CacheRoots[j]
			sort.Strings(r.Repositories)
			sort.Slice(r.Placements, func(a, b int) bool {
				aa, bb := r.Placements[a], r.Placements[b]
				return aa.Identity < bb.Identity ||
					(aa.Identity == bb.Identity && aa.Path < bb.Path)
			})
		}
	}
	sort.Slice(p.Recipes, func(i, j int) bool { return p.Recipes[i].Name < p.Recipes[j].Name })
	sort.Slice(p.ClusterDrafts, func(i, j int) bool {
		return p.ClusterDrafts[i].Engine+"/"+p.ClusterDrafts[i].Name <
			p.ClusterDrafts[j].Engine+"/"+p.ClusterDrafts[j].Name
	})
	sort.Slice(p.Runs, func(i, j int) bool { return p.Runs[i].ID < p.Runs[j].ID })
	sort.Slice(p.Strays, func(i, j int) bool { return p.Strays[i].Path < p.Strays[j].Path })
	sort.Slice(p.Benchmarks.Index, func(i, j int) bool {
		return p.Benchmarks.Index[i].File < p.Benchmarks.Index[j].File
	})
}

// ValidatePlanDigest re-checks the plan digest (loaded from disk).
func ValidatePlanDigest(p *Plan) error {
	got := PlanDigestOf(p)
	if got != p.Digest {
		return fmt.Errorf("plan digest mismatch: file says %s, computed %s", p.Digest, got)
	}
	return nil
}
