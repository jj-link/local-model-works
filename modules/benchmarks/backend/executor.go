package backend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/workload"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// Timeout budget for one grader dispatch sequence. PULL may fetch the
// image from a remote registry; CREATE/START are local Docker operations;
// INSPECT is a state probe; the per-language cap bounds the whole grader
// run regardless of endpoint behavior.
const (
	pullTimeout    = 15 * time.Minute
	createTimeout  = 2 * time.Minute
	startTimeout   = 2 * time.Minute
	inspectTimeout = 10 * time.Second
	pollInterval   = 3 * time.Second
	languageCap    = 60 * time.Minute
	logEndTimeout  = 2 * time.Minute
	removeTimeout  = 2 * time.Minute
	cleanupTimeout = 30 * time.Second
	// ownershipCleanupTimeout bounds the runner's `cleanup
	// <ownership.json>` pass: inspect + `docker rm` per owned resource.
	ownershipCleanupTimeout = 10 * time.Minute
)

// supportedLanguages is the closed set the grader can target.
var supportedLanguages = []string{"python", "javascript", "go", "rust", "cpp", "java"}

// inputSchema validates the benchmark job input before a run is created.
// Defaults for the numeric knobs live in the executor (module settings),
// not here: the API boundary applies the fragment's defaults, and a direct
// submission may rely on the executor's fallback chain.
var inputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["benchmark_id", "version", "harness", "candidate_count", "concurrency", "seed"],
  "properties": {
    "benchmark_id": {"type": "string"},
    "version": {"type": "string"},
    "harness": {"type": "string"},
    "generation_deployment_id": {
      "type": "string",
      "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
    },
    "generation_model": {"type": "string", "minLength": 1},
    "verification_deployment_id": {
      "type": "string",
      "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
    },
    "verification_model": {"type": "string", "minLength": 1},
    "task_ids": {
      "type": "array",
      "uniqueItems": true,
      "items": {
        "type": "string",
        "enum": [
          "atrx-vep-crispr",
          "batched-eval-parity",
          "biped-contact-dynamics",
          "bun-sourcemap-leak",
          "cad-model",
          "cargo-flight-dispatch",
          "cli-2ph-simplex",
          "coq-block-bound",
          "ctr-optimization",
          "cumulative-layout-shift",
          "data-anonymization",
          "distributed-dedup",
          "embedding-drift-monitor",
          "erp-procurement-planning",
          "exam-pdf-eval",
          "fin-saccr-rwa",
          "fix-uautomizer-soundness",
          "foodstuff-beta-activity",
          "formal-crypto",
          "fp8-rmsnorm-gemm",
          "freecad-impeller",
          "freecad-platform-drawing",
          "freecad-spring-clip",
          "freight-dispatch-shift",
          "glycan-ms2-elucidation",
          "gpt2-codegolf",
          "gsea-proteomics",
          "heat-pump-warranty",
          "hof-topology-interpenetration",
          "html-js-filter",
          "ico-path-patch",
          "interleaved-vigenere",
          "intrastat-meldung",
          "jax-speedrun-gpu",
          "ks-solver-cpp",
          "kv-live-surgery",
          "lake-temp-glm",
          "layout-config-recreation",
          "layout-config-recreation2",
          "lean-midpoint-proof",
          "legacy-utility-triage",
          "live-database-cutover",
          "math-eval-grader",
          "medical-claims-processing",
          "memcached-backdoor",
          "mp-checkpoint-consolidation",
          "music-harmony",
          "mvcc-lsm-compaction",
          "nextjs-performance",
          "ontology-kg-querying",
          "payments-pipeline-fix",
          "photonic-waveguide-routing",
          "pretrain-shard-corruption",
          "production-planning",
          "protein-autointerp-disulfide",
          "react-lead-form",
          "retro-console-soc",
          "risk-scorer-replay",
          "roy-polymorph-cn",
          "rs-archive-clone",
          "satb-audio-transcription",
          "session-window-debug",
          "sglang-qwen-burst",
          "shadow-relay",
          "sound-change-cascade",
          "takens-embedding-lean",
          "telecom-entity-resolution",
          "uefi-bootkit",
          "vba-userform-port",
          "vf2-speedup-networkx",
          "vllm-deepseek-streaming",
          "vpp-loss-divergence",
          "wal-recovery-ordering",
          "wdm-design"
        ]
      }
    },
    "candidate_count": {"type": "integer", "minimum": 1, "maximum": 10},
    "concurrency": {"type": "integer", "minimum": 1, "maximum": 32},
    "seed": {"type": "integer"},
    "verifier_repetitions": {"type": "integer", "minimum": 1, "maximum": 32},
    "verifier_pivots": {"type": "integer", "minimum": 1, "maximum": 10},
    "languages": {
      "type": "array",
      "minItems": 1,
      "uniqueItems": true,
      "items": {
        "type": "string",
        "enum": ["python", "javascript", "go", "rust", "cpp", "java"]
      }
    },
    "prompts_per_language": {"type": "integer", "minimum": 1, "maximum": 256},
    "max_tokens": {"type": "integer", "minimum": 16, "maximum": 16384},
    "temperature": {"type": "number", "minimum": 0, "maximum": 2},
    "reason": {"type": "string"},
    "origin": {
      "type": "object",
      "additionalProperties": false,
      "required": ["client_id", "client_name"],
      "properties": {
        "client_id": {"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
        "client_name": {"type": "string", "minLength": 1},
        "run_id": {"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"},
        "project_id": {"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}
      },
      "dependentRequired": {"run_id": ["project_id"], "project_id": ["run_id"]}
    }
  },
  "oneOf": [
    {
      "required": ["generation_deployment_id", "generation_model", "languages", "prompts_per_language", "max_tokens", "temperature"],
      "properties": {
        "benchmark_id": {"const": "lmw-code-generation"},
        "version": {"const": "1"},
        "harness": {"const": "lmw-oneshot"},
        "candidate_count": {"const": 1}
      },
      "not": {"anyOf": [
        {"required": ["task_ids"]},
        {"required": ["verification_deployment_id"]},
        {"required": ["verification_model"]},
        {"required": ["verifier_repetitions"]},
        {"required": ["verifier_pivots"]}
      ]}
    },
    {
      "properties": {
        "benchmark_id": {"const": "terminal-bench"},
        "version": {"const": "3.0.0"},
        "harness": {"const": "oracle"}
      },
      "not": {"anyOf": [
        {"required": ["generation_deployment_id"]},
        {"required": ["generation_model"]},
        {"required": ["verification_deployment_id"]},
        {"required": ["verification_model"]},
        {"required": ["verifier_repetitions"]},
        {"required": ["verifier_pivots"]},
        {"required": ["languages"]},
        {"required": ["prompts_per_language"]},
        {"required": ["max_tokens"]},
        {"required": ["temperature"]}
      ]}
    },
    {
      "required": ["generation_deployment_id", "generation_model"],
      "properties": {
        "benchmark_id": {"const": "terminal-bench"},
        "version": {"const": "3.0.0"},
        "harness": {"enum": ["codex", "mini-swe-agent"]}
      },
      "not": {"anyOf": [
        {"required": ["languages"]},
        {"required": ["prompts_per_language"]},
        {"required": ["max_tokens"]},
        {"required": ["temperature"]}
      ]},
      "if": {"required": ["verification_deployment_id"]},
      "then": {
        "required": ["verification_model", "verifier_repetitions", "verifier_pivots"],
        "properties": {"candidate_count": {"minimum": 2}}
      },
      "else": {"not": {"anyOf": [
        {"required": ["verification_model"]},
        {"required": ["verifier_repetitions"]},
        {"required": ["verifier_pivots"]}
      ]}}
    }
  ]
}`)

// outputSchema validates the executor's output object before the run is
// marked succeeded. The two benchmark kinds own distinct output contracts:
// lmw-code-generation returns one result entry per language, while
// terminal-bench returns the coordinator's artifact references (the per-trial
// data lives in the benchmark_results table and the published artifacts).
var outputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "oneOf": [
    {
      "required": ["results"],
      "properties": {
        "results": {
          "type": "array",
          "items": {
            "type": "object",
            "required": ["language", "requests", "successes", "total_tokens", "tokens_per_second"],
            "properties": {
              "language": {"type": "string"},
              "requests": {"type": "integer"},
              "successes": {"type": "integer"},
              "prompt_tokens": {"type": "integer"},
              "completion_tokens": {"type": "integer"},
              "total_tokens": {"type": "integer"},
              "wall_seconds": {"type": "number"},
              "tokens_per_second": {"type": "number"}
            }
          }
        }
      }
    },
    {
      "required": ["summary", "trials", "bundle"],
      "properties": {
        "summary": {"type": "string"},
        "trials": {"type": "string"},
        "bundle": {"type": "string"}
      }
    }
  ]
}`)

type benchmarkEnvelope struct {
	BenchmarkID string `json:"benchmark_id"`
}

type codeGenerationBenchmarkIn struct {
	BenchmarkID            string   `json:"benchmark_id"`
	Version                string   `json:"version"`
	Harness                string   `json:"harness"`
	GenerationDeploymentID string   `json:"generation_deployment_id"`
	GenerationModel        string   `json:"generation_model"`
	CandidateCount         int      `json:"candidate_count"`
	Concurrency            int      `json:"concurrency"`
	Seed                   int      `json:"seed"`
	Languages              []string `json:"languages"`
	PromptsPerLanguage     int      `json:"prompts_per_language"`
	MaxTokens              int      `json:"max_tokens"`
	Temperature            float64  `json:"temperature"`
	Reason                 string   `json:"reason"`
}

// graderParams is the per-language environment for one grader container.
type graderParams struct {
	baseURL     string
	model       string
	prompts     int
	maxTokens   int
	temperature float64
	reason      string
}

// graderResult is the grader's terminal "RESULT:" JSON line.
type graderResult struct {
	Lang             string             `json:"lang"`
	Model            string             `json:"model"`
	Requests         int                `json:"requests"`
	Successes        int                `json:"successes"`
	OKCount          int                `json:"ok_count"`
	PromptTokens     int                `json:"prompt_tokens"`
	CompletionTokens int                `json:"completion_tokens"`
	TotalTokens      int                `json:"total_tokens"`
	WallSeconds      float64            `json:"wall_seconds"`
	TokensPerSecond  float64            `json:"tokens_per_second"`
	LatencyMS        map[string]float64 `json:"latency_ms"`
	FirstTokenMS     map[string]float64 `json:"first_token_ms"`
}

func decodeBenchmarkInput(raw map[string]any, target any) error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return err
	}
	return nil
}

func (m *Module) runBenchmark(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var envelope benchmarkEnvelope
	if err := decodeBenchmarkInput(c.Input, &envelope); err != nil {
		return nil, fmt.Errorf("benchmark input: %w", err)
	}
	if err := m.ensureBenchmarkRunResult(ctx, c.RunID, c.Input); err != nil {
		return nil, err
	}
	switch envelope.BenchmarkID {
	case "lmw-code-generation":
		return m.runCodeGenerationBenchmark(ctx, c)
	case "terminal-bench":
		return m.runTerminalBench(ctx, c)
	default:
		return nil, fmt.Errorf("unsupported benchmark_id %q", envelope.BenchmarkID)
	}
}

// rankZeroNode returns the node hosting rank 0 (the endpoint's node); the
// first placement entry is the fallback for documents without an explicit
// rank 0.
func rankZeroNode(placements []deploy.Placement) (string, error) {
	if len(placements) == 0 {
		return "", fmt.Errorf("deployment has no placements")
	}
	for _, pl := range placements {
		if pl.Rank == 0 {
			return pl.NodeID, nil
		}
	}
	return placements[0].NodeID, nil
}

// runCodeGenerationBenchmark executes one digest-pinned grader container per
// language against the selected deployment. Each completed language records a
// benchmark_results row; the output object mirrors them.
func (m *Module) runCodeGenerationBenchmark(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var in codeGenerationBenchmarkIn
	if err := decodeBenchmarkInput(c.Input, &in); err != nil {
		return nil, fmt.Errorf("benchmark input: %w", err)
	}
	dep, err := m.env.Deploy.Get(ctx, in.GenerationDeploymentID)
	if err != nil {
		return nil, err
	}
	if dep.Endpoint == nil {
		return nil, fmt.Errorf("deployment %s has no endpoint", dep.ID)
	}
	baseURL, err := deploy.OpenAIAPIBase(dep.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("deployment %s: %w", dep.ID, err)
	}
	nodeID, err := rankZeroNode(dep.Placements)
	if err != nil {
		return nil, err
	}
	if !m.env.Nodes.Online(nodeID) {
		return nil, fmt.Errorf("grader node %s is offline", nodeID)
	}

	d := workload.New(m.env.Nodes, m.env.Commands, nodeID, "", c.RunID, 0)
	results := make([]map[string]any, 0, len(in.Languages))
	completed := make([]codeGenerationResult, 0, len(in.Languages))
	for _, lang := range in.Languages {
		params := graderParams{
			baseURL:     baseURL,
			model:       in.GenerationModel,
			prompts:     in.PromptsPerLanguage,
			maxTokens:   in.MaxTokens,
			temperature: in.Temperature,
			reason:      in.Reason,
		}
		out, graderResult, err := m.runLanguage(ctx, d, c.RunID, c.Logf, lang, params)
		if err != nil {
			return nil, fmt.Errorf("language %s: %w", lang, err)
		}
		results = append(results, out)
		completed = append(completed, codeGenerationResult{language: lang, params: params, result: graderResult})
	}
	if err := m.completeCodeGenerationBenchmark(ctx, c, in, completed, results); err != nil {
		return nil, err
	}
	return map[string]any{"results": results}, nil
}

type codeGenerationResult struct {
	language string
	params   graderParams
	result   *graderResult
}

func publishCodeGenerationArtifacts(c *jobs.Context, in codeGenerationBenchmarkIn, results []map[string]any) (jobs.PublishedArtifact, jobs.PublishedArtifact, error) {
	summary, err := json.MarshalIndent(map[string]any{
		"schema_version":    1,
		"run_id":            c.RunID,
		"benchmark_id":      in.BenchmarkID,
		"benchmark_version": in.Version,
		"harness":           in.Harness,
		"results":           results,
	}, "", "  ")
	if err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("encode summary: %w", err)
	}
	summary = append(summary, '\n')
	const summaryName = "summary.json"
	if err := os.WriteFile(filepath.Join(c.Workspace, summaryName), summary, 0o600); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("write summary: %w", err)
	}

	var bundle bytes.Buffer
	gzipWriter, err := gzip.NewWriterLevel(&bundle, gzip.BestCompression)
	if err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("create bundle compressor: %w", err)
	}
	gzipWriter.Header.ModTime = time.Unix(0, 0).UTC()
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	header := &tar.Header{
		Name: summaryName, Mode: 0o600, Size: int64(len(summary)),
		ModTime: time.Unix(0, 0).UTC(), Typeflag: tar.TypeReg,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("write bundle header: %w", err)
	}
	if _, err := tarWriter.Write(summary); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("write bundle summary: %w", err)
	}
	if err := tarWriter.Close(); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("close bundle archive: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("close bundle compressor: %w", err)
	}
	const bundleName = "result-bundle.tar.gz"
	if err := os.WriteFile(filepath.Join(c.Workspace, bundleName), bundle.Bytes(), 0o600); err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("write bundle: %w", err)
	}

	summaryArtifact, err := c.PublishArtifact("benchmark-summary", summaryName)
	if err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("publish summary artifact: %w", err)
	}
	bundleArtifact, err := c.PublishArtifact("benchmark-result-bundle", bundleName)
	if err != nil {
		return jobs.PublishedArtifact{}, jobs.PublishedArtifact{}, fmt.Errorf("publish bundle artifact: %w", err)
	}
	return summaryArtifact, bundleArtifact, nil
}

func (m *Module) completeCodeGenerationBenchmark(ctx context.Context, c *jobs.Context, in codeGenerationBenchmarkIn, completed []codeGenerationResult, results []map[string]any) error {
	summaryArtifact, bundleArtifact, err := publishCodeGenerationArtifacts(c, in, results)
	if err != nil {
		return err
	}
	tx, err := m.env.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := m.env.Q.WithTx(tx)
	var passedCount, requests, successes, promptTokens, completionTokens, totalTokens int64
	var wallSeconds float64
	for _, completedLanguage := range completed {
		if err := m.record(ctx, queries, c.RunID, completedLanguage.language, completedLanguage.params, completedLanguage.result); err != nil {
			return err
		}
		result := completedLanguage.result
		if result.Successes > 0 {
			passedCount++
		}
		requests += int64(result.Requests)
		successes += int64(result.Successes)
		promptTokens += int64(result.PromptTokens)
		completionTokens += int64(result.CompletionTokens)
		totalTokens += int64(result.TotalTokens)
		wallSeconds += result.WallSeconds
	}
	passAtOne := sql.NullFloat64{}
	if requests > 0 {
		passAtOne = sql.NullFloat64{Float64: float64(successes) / float64(requests), Valid: true}
	}
	metrics, err := json.Marshal(map[string]any{"language_count": len(completed)})
	if err != nil {
		return err
	}

	if err := queries.CompleteBenchmarkRunResult(ctx, db.CompleteBenchmarkRunResultParams{
		PassedCount:       passedCount,
		PassAt1:           passAtOne,
		PromptTokens:      promptTokens,
		CompletionTokens:  completionTokens,
		TotalTokens:       totalTokens,
		WallSeconds:       wallSeconds,
		SummaryArtifactID: sql.NullString{String: summaryArtifact.ID, Valid: true},
		BundleArtifactID:  sql.NullString{String: bundleArtifact.ID, Valid: true},
		MetricsJson:       string(metrics),
		RunID:             c.RunID,
	}); err != nil {
		return fmt.Errorf("complete benchmark run result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// terminalBenchExecutor is the Terminal-Bench coordinator job input.
type terminalBenchExecutor struct {
	BenchmarkID              string   `json:"benchmark_id"`
	Version                  string   `json:"version"`
	Harness                  string   `json:"harness"`
	GenerationDeploymentID   string   `json:"generation_deployment_id,omitempty"`
	GenerationModel          string   `json:"generation_model"`
	VerificationDeploymentID string   `json:"verification_deployment_id,omitempty"`
	VerificationModel        string   `json:"verification_model"`
	TaskIDs                  []string `json:"task_ids"`
	CandidateCount           int      `json:"candidate_count"`
	Concurrency              int      `json:"concurrency"`
	Seed                     int      `json:"seed"`
	VerifierRepetitions      int      `json:"verifier_repetitions"`
	VerifierPivots           int      `json:"verifier_pivots"`
}

// benchmarkTerminalDataset is the pinned Terminal-Bench catalog locator.
const benchmarkTerminalDataset = "terminal-bench/terminal-bench@3.0.0"

// harborRunnerConfig is the coordinator's `run <config-json>` document.
// The backend copies the validated job input into it; the runner
// re-validates every field before touching Docker.
type harborRunnerConfig struct {
	SchemaVersion       int      `json:"schema_version"`
	RunID               string   `json:"run_id"`
	Dataset             string   `json:"dataset"`
	Harness             string   `json:"harness"`
	Workspace           string   `json:"workspace"`
	TaskIDs             []string `json:"task_ids"`
	CandidateCount      int      `json:"candidate_count"`
	Concurrency         int      `json:"concurrency"`
	Seed                int      `json:"seed"`
	GenerationModel     string   `json:"generation_model,omitempty"`
	GenerationAPIBase   string   `json:"generation_api_base,omitempty"`
	VerificationModel   string   `json:"verification_model,omitempty"`
	VerificationAPIBase string   `json:"verification_api_base,omitempty"`
	VerifierRepetitions int      `json:"verifier_repetitions,omitempty"`
	VerifierPivots      int      `json:"verifier_pivots,omitempty"`
}

// harborCommandTimeout caps one coordinator run; the code-generation
// per-language cap never applies because a Terminal-Bench job runs
// candidate_count Harbor attempts per task concurrently.
const harborCommandTimeout = 24 * time.Hour

// harborSpec builds the hardened coordinator container spec: either a
// digest-pinned registry image or an exact image ID already loaded on the
// execution machine, the job workspace mounted at the identical absolute
// path, the host Docker socket for Harbor's own container orchestration, and
// host networking (the coordinator reaches LMW deployments over the host
// loopback). configJSON is appended under the entrypoint as the `run
// <config-json>` document.
// workloadsuffix distinguishes the main coordinator from the cleanup
// coordinator sharing one protocol identity prefix; command "cleanup"
// expects the ownership manifest path instead of a config document.
func harborSpec(runSpec, imageRef, workspace string, configJSON []byte, command string, suffix string) (*runtime.ContainerSpec, []byte, error) {
	if !runtime.IsContentAddressedImage(imageRef) {
		return nil, nil, fmt.Errorf("harbor runner image must be content-addressed")
	}
	image := imageRef
	digest := ""
	if !runtime.IsSHA256Digest(imageRef) {
		image, digest, _ = strings.Cut(imageRef, "@")
	}
	if !uuidPattern.MatchString(runSpec) {
		return nil, nil, fmt.Errorf("harbor run id must be a UUID")
	}
	if !filepath.IsAbs(workspace) {
		return nil, nil, fmt.Errorf("harbor workspace must be absolute")
	}
	cmd := []string{"run", string(configJSON)}
	workloadIdentity := runSpec
	if suffix != "" && command == "cleanup" {
		workloadIdentity = suffix
	}
	labels := runtime.ManagedLabels("", workloadIdentity, "", "", 0, "benchmarks")
	labels[runtime.LabelRun] = workloadIdentity
	spec := &runtime.ContainerSpec{
		Image:       image,
		ImageDigest: digest,
		Cmd:         cmd,
		Env:         []string{"LMW_JOB_WORKSPACE=" + workspace},
		NetworkMode: "host",
		Mounts: []runtime.MountSpec{
			{Source: workspace, Dest: workspace},
			{Source: "/var/run/docker.sock", Dest: "/var/run/docker.sock"},
		},
		WorkingDir:      workspace,
		ReadonlyRootfs:  true,
		NoNewPrivileges: true,
		CapDrop:         []string{"ALL"},
		TmpfsBytes:      1 << 30,
		PidsLimit:       4096,
		MemoryBytes:     8 << 30,
		Labels:          labels,
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, err
	}
	return spec, b, nil
}

// cleanupHarborOwnership runs the coordinator's `cleanup <ownership.json>`
// command through a fresh coordinator container so Harbor-owned
// containers, networks, and volumes are removed on every exit path. The
// helper resolves the approved local runner node like the run itself,
// verifies the manifest's schema_version, then dispatches the cleanup
// command best effort: errors are logged, never propagated.
func (m *Module) cleanupHarborOwnership(ctx context.Context, imageRef string, c harborJobContext) {
	ownershipPath := filepath.Join(c.Workspace(), "ownership.json")
	data, err := os.ReadFile(ownershipPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.Logf("[benchmark] harbor ownership manifest unreadable: %v", err)
		}
		return
	}
	var manifest struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil || manifest.SchemaVersion != 1 {
		c.Logf("[benchmark] harbor ownership manifest unreadable: %v", err)
		return
	}
	availability, err := m.benchmarkRunnerAvailability(ctx)
	if err != nil {
		c.Logf("[benchmark] harbor ownership cleanup skipped: resolve runner: %v", err)
		return
	}
	if !availability.Online || availability.NodeID == "" {
		c.Logf("[benchmark] harbor ownership cleanup skipped: runner offline")
		return
	}
	d := workload.New(m.env.Nodes, m.env.Commands, availability.NodeID, "", c.RunID()+"-cleanup", 0)
	spec, _, err := harborSpec(c.RunID(), imageRef, c.Workspace(), nil, "cleanup", c.RunID()+"-cleanup")
	if err != nil {
		c.Logf("[benchmark] harbor ownership cleanup skipped: spec: %v", err)
		return
	}
	cleanupSpec := *spec
	cleanupSpec.Cmd = []string{"cleanup", ownershipPath}
	specJSON, err := json.Marshal(&cleanupSpec)
	if err != nil {
		c.Logf("[benchmark] harbor ownership cleanup skipped: encode: %v", err)
		return
	}
	c.Logf("[benchmark] harbor ownership cleanup starting")
	if spec.ImageDigest != "" {
		if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_PULL, specJSON, pullTimeout); err != nil {
			c.Logf("[benchmark] harbor ownership cleanup pull: %v", err)
		}
	}
	if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, createTimeout); err != nil {
		c.Logf("[benchmark] harbor ownership cleanup create: %v", err)
		return
	}
	if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, startTimeout); err != nil {
		c.Logf("[benchmark] harbor ownership cleanup start: %v", err)
		_, _ = d.Do(context.WithoutCancel(ctx), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout)
		return
	}
	result, err := waitCoordinator(ctx, d, ownershipCleanupTimeout)
	if err != nil {
		c.Logf("[benchmark] harbor ownership cleanup wait: %v", err)
		_, _ = d.Do(context.WithoutCancel(ctx), agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, cleanupTimeout)
		_, _ = d.Do(context.WithoutCancel(ctx), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout)
		return
	}
	if result.GetExitCode() != 0 {
		c.Logf("[benchmark] harbor ownership cleanup exited %d", result.GetExitCode())
	}
	_, _ = d.Do(context.WithoutCancel(ctx), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout)
	c.Logf("[benchmark] harbor ownership cleanup complete")
}

// harborJobContext is the minimal coordinator-cleanup view shared by the
// live job path (*jobs.Context wrapped once) and restart reconciliation.
type harborJobContext interface {
	RunID() string
	Workspace() string
	Logf(format string, args ...any)
}

// jobContextHarborAdapter wraps *jobs.Context fields into the method view.
type jobContextHarborAdapter struct {
	runID     string
	workspace string
	logf      func(string, ...any)
}

func adaptJobsContext(c *jobs.Context) jobContextHarborAdapter {
	return jobContextHarborAdapter{runID: c.RunID, workspace: c.Workspace, logf: c.Logf}
}

func (a jobContextHarborAdapter) RunID() string                   { return a.runID }
func (a jobContextHarborAdapter) Workspace() string               { return a.workspace }
func (a jobContextHarborAdapter) Logf(format string, args ...any) { a.logf(format, args...) }

// prepareCoordinatorWorkspace widens the private workspace so the
// coordinator container can write its config, artifacts, and ownership
// manifest. Server-owned 0700 blocks the container because its UID 0 runs
// without CAP_DAC_OVERRIDE after the capability drop, so plain DAC checks
// decide access.
func prepareCoordinatorWorkspace(workspace string) error {
	if err := os.Chmod(workspace, 0o777); err != nil {
		return fmt.Errorf("prepare coordinator workspace %s: %w", workspace, err)
	}
	return nil
}

// runTerminalBench executes one Terminal-Bench benchmark run through the
// trusted digest-pinned Harbor coordinator dispatched to the approved
// local node. Lifecycle: resolve settings/node → PULL the pinned image
// → CREATE + START the coordinator (the agent's tailer mirrors runner
// stdout/stderr into the run log, including the runner's terminal
// RESULT: line) → poll INSPECT until exit → parse the RESULT: line →
// import summary/trials + publish the summary and bundle artifacts in
// one transaction → on every exit path, STOP/REMOVE the coordinator and
// run the runner's `cleanup <ownership.json>` pass so Harbor-owned
// containers, networks, and volumes are deleted.
func (m *Module) runTerminalBench(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var in terminalBenchExecutor
	if err := decodeBenchmarkInput(c.Input, &in); err != nil {
		return nil, fmt.Errorf("benchmark input: %w", err)
	}
	availability, err := m.benchmarkRunnerAvailability(ctx)
	if err != nil {
		return nil, err
	}
	if !availability.Configured || !availability.Online || availability.NodeID == "" {
		return nil, fmt.Errorf("benchmarks.runner_unavailable")
	}
	values, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return nil, err
	}
	imageRef, _ := values["harbor_runner_image"].(string)
	imageRef = strings.TrimSpace(imageRef)

	// One generation/verification deployment: resolve-spec of the job input's
	// healthy deployments to OpenAI-compatible /v1 API bases on the host.
	generationAPIBase := ""
	if in.Harness != "oracle" {
		dep, err := m.env.Deploy.Get(ctx, in.GenerationDeploymentID)
		if err != nil {
			return nil, fmt.Errorf("generation deployment: %w", err)
		}
		if dep.ObservedState != "healthy" || dep.Endpoint == nil {
			return nil, fmt.Errorf("generation deployment %s is not healthy", in.GenerationDeploymentID)
		}
		base, err := deploy.OpenAIAPIBase(dep.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("generation deployment %s: %w", in.GenerationDeploymentID, err)
		}
		generationAPIBase = base
	}
	verificationAPIBase := ""
	if in.VerificationDeploymentID != "" {
		dep, err := m.env.Deploy.Get(ctx, in.VerificationDeploymentID)
		if err != nil {
			return nil, fmt.Errorf("verification deployment: %w", err)
		}
		if dep.ObservedState != "healthy" || dep.Endpoint == nil {
			return nil, fmt.Errorf("verification deployment %s is not healthy", in.VerificationDeploymentID)
		}
		base, err := deploy.OpenAIAPIBase(dep.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("verification deployment %s: %w", in.VerificationDeploymentID, err)
		}
		verificationAPIBase = base
	}
	// The coordinator container mounts the run's private workspace at the
	// identical absolute path, so the runner's config can use c.Workspace
	// directly and ownership/trial paths stay workspace-relative. The
	// workspace itself is server-owned 0700. The container process runs as
	// its own UID 0 but starts with all capabilities dropped, including
	// CAP_DAC_OVERRIDE, so plain DAC checks apply and the per-run directory
	// must be writable for it without privilege.
	if err := prepareCoordinatorWorkspace(c.Workspace); err != nil {
		return nil, err
	}
	run := harborRunnerConfig{
		SchemaVersion:       1,
		RunID:               c.RunID,
		Dataset:             benchmarkTerminalDataset,
		Harness:             in.Harness,
		Workspace:           c.Workspace,
		TaskIDs:             in.TaskIDs,
		CandidateCount:      in.CandidateCount,
		Concurrency:         in.Concurrency,
		Seed:                in.Seed,
		GenerationModel:     in.GenerationModel,
		GenerationAPIBase:   generationAPIBase,
		VerificationModel:   in.VerificationModel,
		VerificationAPIBase: verificationAPIBase,
		VerifierRepetitions: in.VerifierRepetitions,
		VerifierPivots:      in.VerifierPivots,
	}
	configJSON, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	spec, specJSON, err := harborSpec(c.RunID, imageRef, c.Workspace, configJSON, "run", "")
	if err != nil {
		return nil, err
	}
	d := workload.New(m.env.Nodes, m.env.Commands, availability.NodeID, "", c.RunID, 0)

	if spec.ImageDigest != "" {
		c.Logf("[benchmark] harbor runner pull %s", imageRef)
		if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_PULL, specJSON, pullTimeout); err != nil {
			return nil, fmt.Errorf("pull coordinator: %w", err)
		}
	} else {
		c.Logf("[benchmark] harbor runner using local image %s", imageRef)
	}

	removed := false
	defer func() {
		if removed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		_, _ = d.Do(cleanupCtx, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, cleanupTimeout)
		_, _ = d.Do(cleanupCtx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout)
	}()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), ownershipCleanupTimeout)
		defer cancel()
		m.cleanupHarborOwnership(cleanupCtx, imageRef, adaptJobsContext(c))
	}()

	c.Logf("[benchmark] harbor coordinator starting (workspace %s)", c.Workspace)
	if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, createTimeout); err != nil {
		return nil, fmt.Errorf("create coordinator: %w", err)
	}
	if _, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, startTimeout); err != nil {
		return nil, fmt.Errorf("start coordinator: %w", err)
	}
	result, err := waitCoordinator(ctx, d, harborCommandTimeout)
	if err != nil {
		return nil, err
	}

	drainCtx, cancelLogEnd := context.WithTimeout(context.Background(), logEndTimeout)
	_, waitErr := m.env.Runs.WaitLogEnd(drainCtx, c.RunID, "", 0, "stdout")
	cancelLogEnd()
	if waitErr != nil {
		return nil, fmt.Errorf("coordinator log did not drain: %w", waitErr)
	}
	log, err := m.readFullLog(c.RunID)
	if err != nil {
		return nil, err
	}
	if result.GetExitCode() != 0 {
		message := extractRunnerError(log)
		if persisted := readRunnerError(c.Workspace); persisted != "" {
			if message != "" {
				message += "; "
			}
			message += persisted
		}
		if message == "" {
			message = "no diagnostic captured"
		}
		return nil, fmt.Errorf("harbor runner exited %d: %s", result.GetExitCode(), message)
	}
	payload, err := parseHarborResult(log)
	if err != nil {
		return nil, err
	}
	logf := c.Logf
	if err := m.importTerminalBenchResults(ctx, c, payload); err != nil {
		return nil, err
	}
	logf("[benchmark] harbor run imported (%s)", payload.Bundle)
	removed = true
	if _, err := d.Do(context.Background(), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, removeTimeout); err != nil {
		logf("[benchmark] harbor: remove: %v", err)
	}
	return map[string]any{
		"summary": payload.Summary,
		"trials":  payload.Trials,
		"bundle":  payload.Bundle,
	}, nil
}

// runWorkloadOp wraps Client.Do with result acknowledgement handling.
func runWorkloadOp(ctx context.Context, d *workload.Client, op agentv1.WorkloadOp, spec []byte, timeout time.Duration) (*agentv1.CommandResult, error) {
	result, err := d.Do(ctx, op, spec, timeout)
	if err != nil {
		return nil, err
	}
	if !result.GetOk() {
		return nil, fmt.Errorf("%s failed: %s", op, result.GetError())
	}
	return result, nil
}

// waitCoordinator polls the coordinator container until it exits, enforces
// the harbor timeout, and returns the terminal INSPECT result.
func waitCoordinator(ctx context.Context, d *workload.Client, limit time.Duration) (*agentv1.CommandResult, error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("harbor coordinator exceeded %s", harborCommandTimeout)
		case <-ticker.C:
			result, err := runWorkloadOp(ctx, d, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, inspectTimeout)
			if err != nil {
				return nil, err
			}
			switch result.GetContainerState() {
			case "exited", "dead":
				return result, nil
			case "running", "created", "paused":
			default:
				return nil, fmt.Errorf("harbor coordinator state invalid: %s", result.GetContainerState())
			}
		}
	}
}

// extractRunnerError finds the runner's most recent stderr diagnostic line
// from the run log (the tailer mirrors both streams into stdout/stderr).
func extractRunnerError(log string) string {
	found := ""
	for _, line := range strings.Split(log, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(trimmed, "benchmarks.") {
			found = trimmed
		}
	}
	return found
}

// readRunnerError reads the runner's persisted workspace diagnostic: the
// stderr tailer can lose the final lines when the container exits, but the
// workspace bind outlives the container.
func readRunnerError(workspace string) string {
	raw, err := os.ReadFile(filepath.Join(workspace, "runner-error.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// harborResultLine is the runner's terminal RESULT:<json> output.
type harborResultLine struct {
	Summary   string `json:"summary"`
	Trials    string `json:"trials"`
	Bundle    string `json:"bundle"`
	Ownership string `json:"ownership"`
}

// parseHarborResult extracts the runner's final RESULT: payload — the last
// such line wins, mirroring parseResultLine.
func parseHarborResult(log string) (*harborResultLine, error) {
	raw := ""
	for _, line := range strings.Split(log, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "RESULT:")
		if ok {
			raw = rest
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("harbor runner emitted no result")
	}
	out := &harborResultLine{}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, fmt.Errorf("harbor runner result undecodable: %w", err)
	}
	return out, nil
}

// importTerminalBenchResults persists the coordinator output in one
// transaction: completes the run-result row, replaces trial rows, publishes
// the summary and bundle artifacts, and records which files live where.
func (m *Module) importTerminalBenchResults(ctx context.Context, c *jobs.Context, payload *harborResultLine) error {
	summaryPath := filepath.Join(c.Workspace, filepath.FromSlash(payload.Summary))
	trialsPath := filepath.Join(c.Workspace, filepath.FromSlash(payload.Trials))
	rawSummary, err := os.ReadFile(summaryPath)
	if err != nil {
		return fmt.Errorf("read summary: %w", err)
	}
	rawTrials, err := os.ReadFile(trialsPath)
	if err != nil {
		return fmt.Errorf("read trials: %w", err)
	}
	var summary terminalBenchSummary
	var trials []terminalBenchTrialRow
	if err := json.Unmarshal(rawSummary, &summary); err != nil {
		return fmt.Errorf("decode summary: %w", err)
	}
	if err := json.Unmarshal(rawTrials, &trials); err != nil {
		return fmt.Errorf("decode trials: %w", err)
	}

	summaryArtifact, err := c.PublishArtifact("benchmark-summary", payload.Summary)
	if err != nil {
		return fmt.Errorf("publish summary artifact: %w", err)
	}
	bundleArtifact, err := c.PublishArtifact("benchmark-result-bundle", payload.Bundle)
	if err != nil {
		return fmt.Errorf("publish bundle artifact: %w", err)
	}

	tx, err := m.env.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	queries := m.env.Q.WithTx(tx)
	if err := queries.DeleteBenchmarkTrialResultsByRun(ctx, c.RunID); err != nil {
		return fmt.Errorf("clear trials: %w", err)
	}
	for _, trial := range trials {
		metrics, err := json.Marshal(trial.Metrics)
		if err != nil {
			return fmt.Errorf("encode trial metrics: %w", err)
		}
		if err := queries.InsertBenchmarkTrialResult(ctx, db.InsertBenchmarkTrialResultParams{
			RunID:            c.RunID,
			TaskID:           trial.TaskID,
			CandidateIndex:   int64(trial.CandidateIndex),
			OfficialPass:     boolToInt(trial.OfficialPass),
			VerifierSelected: boolToInt(trial.VerifierSelected),
			VerifierScore:    nullFloat(trial.VerifierScore),
			TrajectoryPath:   trial.TrajectoryPath,
			MetricsJson:      string(metrics),
		}); err != nil {
			return fmt.Errorf("insert trial: %w", err)
		}
	}
	if err := queries.CompleteBenchmarkRunResult(ctx, db.CompleteBenchmarkRunResultParams{
		PassedCount:       int64(summary.PassedCount),
		PassAt1:           nullFloat(summary.PassAt1),
		VerifierPassRate:  nullFloat(summary.VerifierPassRate),
		OraclePassRate:    nullFloat(summary.OraclePassRate),
		PromptTokens:      summary.PromptTokens,
		CompletionTokens:  summary.CompletionTokens,
		TotalTokens:       summary.TotalTokens,
		WallSeconds:       summary.WallSeconds,
		SummaryArtifactID: sql.NullString{String: summaryArtifact.ID, Valid: true},
		BundleArtifactID:  sql.NullString{String: bundleArtifact.ID, Valid: true},
		MetricsJson:       string(rawSummary),
		RunID:             c.RunID,
	}); err != nil {
		return fmt.Errorf("complete benchmark run result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// terminalBenchSummary mirrors the runner's summary.json top level.
type terminalBenchSummary struct {
	SchemaVersion    int            `json:"schema_version"`
	BenchmarkID      string         `json:"benchmark_id"`
	BenchmarkVersion string         `json:"benchmark_version"`
	Dataset          string         `json:"dataset"`
	SourceChecksum   string         `json:"source_checksum"`
	Harness          string         `json:"harness"`
	TaskCount        int            `json:"task_count"`
	CandidateCount   int            `json:"candidate_count"`
	PassedCount      int            `json:"passed_count"`
	PassAt1          *float64       `json:"pass_at_1"`
	VerifierPassRate *float64       `json:"verifier_pass_rate"`
	OraclePassRate   *float64       `json:"oracle_pass_rate"`
	PromptTokens     int64          `json:"prompt_tokens"`
	CompletionTokens int64          `json:"completion_tokens"`
	TotalTokens      int64          `json:"total_tokens"`
	WallSeconds      float64        `json:"wall_seconds"`
	Metrics          map[string]any `json:"metrics"`
}

type terminalBenchTrialRow struct {
	TaskID           string         `json:"task_id"`
	CandidateIndex   int            `json:"candidate_index"`
	OfficialPass     bool           `json:"official_pass"`
	VerifierSelected bool           `json:"verifier_selected"`
	VerifierScore    *float64       `json:"verifier_score"`
	TrajectoryPath   string         `json:"trajectory_path"`
	Metrics          map[string]any `json:"metrics"`
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func nullFloat(value *float64) sql.NullFloat64 {
	if value == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *value, Valid: true}
}

// graderSpec builds a hardened, language-specific compiler container. Source,
// compiler caches, and test executables are confined to a bounded tmpfs.
func graderSpec(runID string, p graderParams, lang string) (*runtime.ContainerSpec, []byte, error) {
	imageRef, ok := graderImages[lang]
	if !ok {
		return nil, nil, fmt.Errorf("unsupported grader language %q", lang)
	}
	image, digest, _ := strings.Cut(imageRef, "@")
	spec := &runtime.ContainerSpec{
		Image:       image,
		ImageDigest: digest,
		Cmd:         []string{"python3", "-c", "import base64,sys;exec(base64.b64decode(sys.argv[1]).decode())", graderScriptB64},
		Env: []string{
			"LMW_BASE_URL=" + p.baseURL,
			"LMW_MODEL=" + p.model,
			"LMW_LANG=" + lang,
			fmt.Sprintf("LMW_PROMPTS=%d", p.prompts),
			fmt.Sprintf("LMW_MAX_TOKENS=%d", p.maxTokens),
			fmt.Sprintf("LMW_TEMPERATURE=%v", p.temperature),
			"LMW_RUN_ID=" + runID,
		},
		NetworkMode:     "host",
		ReadonlyRootfs:  true,
		NoNewPrivileges: true,
		CapDrop:         []string{"ALL"},
		TmpfsBytes:      1 << 30,
		PidsLimit:       256,
		MemoryBytes:     4 << 30,
		Labels:          runtime.ManagedLabels("", runID, "", "", 0, "benchmarks"),
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return nil, nil, err
	}
	return spec, b, nil
}

// runLanguage dispatches one grader container through PULL/CREATE/START,
// polls INSPECT until it exits, harvests the RESULT line from the run's
// ad-hoc log stream (the same stream Logf writes), records the result
// row, and removes the container. Every failure and cancellation path
// tears the container down (STOP then REMOVE, best effort) before
// returning.
func (m *Module) runLanguage(ctx context.Context, d *workload.Client, runID string, logf func(format string, args ...any), lang string, p graderParams) (map[string]any, *graderResult, error) {
	_, specJSON, err := graderSpec(runID, p, lang)
	if err != nil {
		return nil, nil, err
	}
	removed := false
	defer func() {
		if removed {
			return
		}
		if _, err := d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, cleanupTimeout); err != nil {
			logf("[benchmark] %s: stop during teardown: %v", lang, err)
		}
		if _, err := d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout); err != nil {
			logf("[benchmark] %s: remove during teardown: %v", lang, err)
		}
	}()

	logf("[benchmark] %s: pulling grader image %s", lang, graderImages[lang])
	cr, err := d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_PULL, specJSON, pullTimeout)
	if err != nil {
		return nil, nil, fmt.Errorf("pull: %w", err)
	}
	if !cr.Ok {
		return nil, nil, fmt.Errorf("pull failed: %s", cr.Error)
	}
	logf("[benchmark] %s: creating and starting grader container", lang)
	if cr, err = d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, createTimeout); err != nil {
		return nil, nil, fmt.Errorf("create: %w", err)
	}
	if !cr.Ok && !strings.Contains(cr.Error, "exists") {
		return nil, nil, fmt.Errorf("create failed: %s", cr.Error)
	}
	if cr, err = d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, startTimeout); err != nil {
		return nil, nil, fmt.Errorf("start: %w", err)
	}
	if !cr.Ok && !strings.Contains(cr.Error, "already running") {
		return nil, nil, fmt.Errorf("start failed: %s", cr.Error)
	}

	deadline := time.Now().Add(languageCap)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
poll:
	for {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-tick.C:
			if time.Now().After(deadline) {
				return nil, nil, fmt.Errorf("grader did not exit within %s", languageCap)
			}
			cr, err := d.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, inspectTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				continue
			}
			if !cr.Ok {
				if strings.Contains(cr.Error, "no such") || strings.Contains(cr.Error, "missing") {
					return nil, nil, fmt.Errorf("grader container disappeared: %s", cr.Error)
				}
				continue
			}
			if cr.ContainerState == "exited" || cr.ContainerState == "dead" {
				if cr.ExitCode != 0 {
					return nil, nil, fmt.Errorf("grader exited with code %d: %s", cr.ExitCode, cr.Error)
				}
				break poll
			}
		}
	}

	logEndCtx, cancelLogEnd := context.WithTimeout(context.Background(), logEndTimeout)
	_, waitErr := m.env.Runs.WaitLogEnd(logEndCtx, runID, "", 0, "stdout")
	cancelLogEnd()
	if waitErr != nil {
		return nil, nil, fmt.Errorf("grader log did not drain: %w", waitErr)
	}
	log, err := m.readFullLog(runID)
	if err != nil {
		return nil, nil, err
	}
	result, err := parseResultLine(log)
	if err != nil {
		return nil, nil, err
	}
	finishCtx, cancelFinish := context.WithTimeout(context.Background(), removeTimeout+time.Minute)
	defer cancelFinish()
	logf("[benchmark] %s: %d requests, %d ok, %.1f tok/s (wall %.1fs)",
		lang, result.Requests, result.OKCount, result.TokensPerSecond, result.WallSeconds)

	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if _, err := d.Do(finishCtx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, removeTimeout); err != nil {
		logf("[benchmark] %s: remove: %v", lang, err)
	} else {
		removed = true
	}
	return map[string]any{
		"language":          lang,
		"requests":          result.Requests,
		"successes":         result.Successes,
		"ok_count":          result.OKCount,
		"prompt_tokens":     result.PromptTokens,
		"completion_tokens": result.CompletionTokens,
		"total_tokens":      result.TotalTokens,
		"wall_seconds":      result.WallSeconds,
		"tokens_per_second": result.TokensPerSecond,
		"latency_ms":        result.LatencyMS,
		"first_token_ms":    result.FirstTokenMS,
	}, result, nil
}

// readFullLog reads the run's ad-hoc stdout log (the same stream Logf
// writes) to EOF.
func (m *Module) readFullLog(runID string) (string, error) {
	var buf bytes.Buffer
	offset := uint64(0)
	for {
		chunk, next, _, err := m.env.Runs.ReadLog(runID, "", 0, "stdout", offset, 0)
		if err != nil {
			return "", fmt.Errorf("read grader log: %w", err)
		}
		buf.Write(chunk)
		offset = next
		if len(chunk) == 0 {
			return buf.String(), nil
		}
	}
}

// parseResultLine extracts the grader's terminal RESULT: line — the last
// such line wins, so progress lines and earlier attempts can never
// shadow it.
func parseResultLine(log string) (*graderResult, error) {
	raw := ""
	for _, line := range strings.Split(log, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSuffix(line, "\r"), "RESULT:")
		if ok {
			raw = rest
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("grader emitted no result")
	}
	gr := &graderResult{}
	if err := json.Unmarshal([]byte(raw), gr); err != nil {
		return nil, fmt.Errorf("grader result undecodable: %w", err)
	}
	return gr, nil
}

// record persists one language's benchmark_results row.
func (m *Module) record(ctx context.Context, queries *db.Queries, runID, lang string, p graderParams, gr *graderResult) error {
	latency, _ := json.Marshal(gr.LatencyMS)
	firstToken, _ := json.Marshal(gr.FirstTokenMS)
	params := db.InsertBenchmarkResultParams{
		RunID:            runID,
		Language:         lang,
		Endpoint:         sql.NullString{String: p.baseURL, Valid: true},
		Model:            sql.NullString{String: p.model, Valid: true},
		Requests:         int64(gr.Requests),
		Successes:        int64(gr.Successes),
		PromptTokens:     int64(gr.PromptTokens),
		CompletionTokens: int64(gr.CompletionTokens),
		TotalTokens:      int64(gr.TotalTokens),
		WallSeconds:      gr.WallSeconds,
		TokensPerSecond:  gr.TokensPerSecond,
		Latency:          string(latency),
		FirstToken:       string(firstToken),
	}
	if gr.Requests > 0 {
		grading, _ := json.Marshal(map[string]any{
			"ok_count":     gr.OKCount,
			"success_rate": float64(gr.Successes) / float64(gr.Requests),
		})
		params.Grading = sql.NullString{String: string(grading), Valid: true}
	}
	if p.reason != "" {
		reasoning, _ := json.Marshal(map[string]string{"reason": p.reason})
		params.Reasoning = sql.NullString{String: string(reasoning), Valid: true}
	}
	if path := m.env.Runs.LogPath(runID, "", 0, "stdout"); path != "" {
		params.ResultPath = sql.NullString{String: path, Valid: true}
	}
	if err := queries.InsertBenchmarkResult(ctx, params); err != nil {
		return fmt.Errorf("record benchmark result: %w", err)
	}
	return nil
}
