package backend

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runtime"
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
  "required": ["deployment_id", "languages"],
  "properties": {
    "deployment_id": {
      "type": "string",
      "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"
    },
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
    "model_name": {"type": "string"},
    "quantization": {"type": "string"},
    "reason": {"type": "string"}
  }
}`)

// outputSchema validates the executor's output object (one entry per
// language) before the run is marked succeeded.
var outputSchema = json.RawMessage(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
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
}`)

// benchmarkIn is the validated job input for one benchmark run.
type benchmarkIn struct {
	DeploymentID       string   `json:"deployment_id"`
	Languages          []string `json:"languages"`
	PromptsPerLanguage int      `json:"prompts_per_language"`
	MaxTokens          int      `json:"max_tokens"`
	Temperature        float64  `json:"temperature"`
	ModelName          string   `json:"model_name"`
	Quantization       string   `json:"quantization"`
	Reason             string   `json:"reason"`
}

// graderParams is the per-language environment for one grader container.
type graderParams struct {
	baseURL      string
	model        string
	prompts      int
	maxTokens    int
	temperature  float64
	quantization string
	reason       string
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

// dispatch is the per-run, per-node workload command channel.
type dispatch struct {
	env    *moduleapi.Env
	nodeID string
	runID  string
}

// op sends one workload command to the node and waits for its ack,
// bounded by timeout. A false Send (node offline) and a ctx cancel fail
// immediately; the broker waiter is released on every path.
func (d *dispatch) op(ctx context.Context, op agentv1.WorkloadOp, specJSON []byte, timeout time.Duration) (*agentv1.CommandResult, error) {
	cmdID, err := id.New()
	if err != nil {
		return nil, err
	}
	sent := d.env.Nodes.Send(d.nodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_WorkloadCommand{
		WorkloadCommand: &agentv1.WorkloadCommand{
			CommandId:     cmdID,
			Op:            op,
			DeploymentId:  "", // ad-hoc grader: not a deployment workload
			RunId:         d.runID,
			Rank:          0,
			ContainerSpec: specJSON,
		},
	}})
	if !sent {
		return nil, fmt.Errorf("node %s offline", d.nodeID)
	}
	ch, release := d.env.Commands.Wait(cmdID)
	defer release()
	select {
	case cr := <-ch:
		return cr, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("timed out after %s waiting for %s ack", timeout, op)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// parseInput re-renders the (already schema-validated) input map to a
// typed struct.
func parseInput(raw map[string]any) (*benchmarkIn, error) {
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	in := &benchmarkIn{}
	if err := json.Unmarshal(b, in); err != nil {
		return nil, err
	}
	return in, nil
}

// settingsDefaults reads the module's operator settings, tolerating
// absent or unregistered settings (first run, before any Set).
func (m *Module) settingsDefaults(ctx context.Context) (prompts, maxTokens int) {
	prompts, maxTokens = 4, 512
	set, _, err := m.env.Settings.Get(ctx, "benchmarks")
	if err != nil || set == nil {
		return
	}
	if v, ok := set["default_prompts_per_language"].(float64); ok && int(v) >= 1 {
		prompts = int(v)
	}
	if v, ok := set["default_max_tokens"].(float64); ok && int(v) >= 1 {
		maxTokens = int(v)
	}
	return
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
func graderBaseURL(port int32) (string, error) {
	if port == 0 {
		return "", errors.New("deployment endpoint has no port")
	}
	// The grader is dispatched to the endpoint's rank-0 node with host
	// networking. Node display names are inventory labels, not DNS names.
	return fmt.Sprintf("http://127.0.0.1:%d", port), nil
}

// runBenchmark is the benchmark job executor: one digest-pinned grader
// container per language, sequentially, against the deployment's rank-0
// endpoint on that node. Each completed language records one
// benchmark_results row; the output object mirrors them.
func (m *Module) runBenchmark(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	in, err := parseInput(c.Input)
	if err != nil {
		return nil, fmt.Errorf("benchmark input: %w", err)
	}
	defPrompts, defMaxTokens := m.settingsDefaults(ctx)
	if in.PromptsPerLanguage == 0 {
		in.PromptsPerLanguage = defPrompts
	}
	if in.MaxTokens == 0 {
		in.MaxTokens = defMaxTokens
	}
	model := in.ModelName
	if model == "" {
		model = "local"
	}

	dep, err := m.env.Deploy.Get(ctx, in.DeploymentID)
	if err != nil {
		return nil, err
	}
	if dep.Endpoint == nil {
		return nil, fmt.Errorf("deployment %s has no endpoint", dep.ID)
	}
	baseURL, err := graderBaseURL(dep.Endpoint.Port)
	if err != nil {
		return nil, fmt.Errorf("deployment %s %w", dep.ID, err)
	}
	nodeID, err := rankZeroNode(dep.Placements)
	if err != nil {
		return nil, err
	}
	if !m.env.Nodes.Online(nodeID) {
		return nil, fmt.Errorf("grader node %s is offline", nodeID)
	}

	d := &dispatch{env: m.env, nodeID: nodeID, runID: c.RunID}

	results := make([]map[string]any, 0, len(in.Languages))
	for _, lang := range in.Languages {
		out, err := m.runLanguage(ctx, d, c.Logf, lang, graderParams{
			baseURL:      baseURL,
			model:        model,
			prompts:      in.PromptsPerLanguage,
			maxTokens:    in.MaxTokens,
			temperature:  in.Temperature,
			quantization: in.Quantization,
			reason:       in.Reason,
		})
		if err != nil {
			return nil, fmt.Errorf("language %s: %w", lang, err)
		}
		results = append(results, out)
	}
	return map[string]any{"results": results}, nil
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
func (m *Module) runLanguage(ctx context.Context, d *dispatch, logf func(format string, args ...any), lang string, p graderParams) (map[string]any, error) {
	_, specJSON, err := graderSpec(d.runID, p, lang)
	if err != nil {
		return nil, err
	}
	removed := false
	defer func() {
		if removed {
			return
		}
		if _, err := d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, cleanupTimeout); err != nil {
			logf("[benchmark] %s: stop during teardown: %v", lang, err)
		}
		if _, err := d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, cleanupTimeout); err != nil {
			logf("[benchmark] %s: remove during teardown: %v", lang, err)
		}
	}()

	logf("[benchmark] %s: pulling grader image %s", lang, graderImages[lang])
	cr, err := d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_PULL, specJSON, pullTimeout)
	if err != nil {
		return nil, fmt.Errorf("pull: %w", err)
	}
	if !cr.Ok {
		return nil, fmt.Errorf("pull failed: %s", cr.Error)
	}
	logf("[benchmark] %s: creating and starting grader container", lang)
	if cr, err = d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, createTimeout); err != nil {
		return nil, fmt.Errorf("create: %w", err)
	}
	if !cr.Ok && !strings.Contains(cr.Error, "exists") {
		return nil, fmt.Errorf("create failed: %s", cr.Error)
	}
	if cr, err = d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, startTimeout); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	if !cr.Ok && !strings.Contains(cr.Error, "already running") {
		return nil, fmt.Errorf("start failed: %s", cr.Error)
	}

	// Poll to exit: the grader runs to completion on its own; INSPECT is
	// idempotent and the only safe way to observe the terminal state.
	deadline := time.Now().Add(languageCap)
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
poll:
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("grader did not exit within %s", languageCap)
			}
			cr, err := d.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, inspectTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				continue // transient ack delay; the next tick retries
			}
			if !cr.Ok {
				if strings.Contains(cr.Error, "no such") || strings.Contains(cr.Error, "missing") {
					return nil, fmt.Errorf("grader container disappeared: %s", cr.Error)
				}
				continue
			}
			if cr.ContainerState == "exited" || cr.ContainerState == "dead" {
				if cr.ExitCode != 0 {
					return nil, fmt.Errorf("grader exited with code %d: %s", cr.ExitCode, cr.Error)
				}
				break poll
			}
		}
	}

	// Post-exit harvest is bounded bookkeeping on an already-finished
	// container: it must not be starved by a run ctx cancelled during the
	// poll (the container has done its work; land the result).
	//
	// Deterministic EOF: the agent's tailer sends its terminal LogChunk
	// only after the container output fully drained; wait for it (2-min
	// cap) before reading the log.
	logEndCtx, cancelLogEnd := context.WithTimeout(context.Background(), logEndTimeout)
	_, werr := m.env.Runs.WaitLogEnd(logEndCtx, d.runID, "", 0, "stdout")
	cancelLogEnd()
	if werr != nil {
		return nil, fmt.Errorf("grader log did not drain: %w", werr)
	}
	log, err := m.readFullLog(d.runID)
	if err != nil {
		return nil, err
	}
	gr, err := parseResultLine(log)
	if err != nil {
		return nil, err
	}
	finishCtx, cancelFinish := context.WithTimeout(context.Background(), removeTimeout+time.Minute)
	defer cancelFinish()
	if err := m.record(finishCtx, d.runID, lang, p, gr); err != nil {
		return nil, err
	}
	logf("[benchmark] %s: %d requests, %d ok, %.1f tok/s (wall %.1fs)",
		lang, gr.Requests, gr.OKCount, gr.TokensPerSecond, gr.WallSeconds)

	if ctx.Err() != nil {
		return nil, ctx.Err() // cancelled while harvesting: let the SDK land the run cancelled
	}
	if _, err := d.op(finishCtx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, removeTimeout); err != nil {
		// Removal failure is logged, not run-failing: the grader is done
		// and its result is recorded.
		logf("[benchmark] %s: remove: %v", lang, err)
	} else {
		removed = true
	}
	return map[string]any{
		"language":          lang,
		"requests":          gr.Requests,
		"successes":         gr.Successes,
		"ok_count":          gr.OKCount,
		"prompt_tokens":     gr.PromptTokens,
		"completion_tokens": gr.CompletionTokens,
		"total_tokens":      gr.TotalTokens,
		"wall_seconds":      gr.WallSeconds,
		"tokens_per_second": gr.TokensPerSecond,
		"latency_ms":        gr.LatencyMS,
		"first_token_ms":    gr.FirstTokenMS,
	}, nil
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
func (m *Module) record(ctx context.Context, runID, lang string, p graderParams, gr *graderResult) error {
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
	if p.quantization != "" {
		params.Quantization = sql.NullString{String: p.quantization, Valid: true}
	}
	if p.reason != "" {
		reasoning, _ := json.Marshal(map[string]string{"reason": p.reason})
		params.Reasoning = sql.NullString{String: string(reasoning), Valid: true}
	}
	if path := m.env.Runs.LogPath(runID, "", 0, "stdout"); path != "" {
		params.ResultPath = sql.NullString{String: path, Valid: true}
	}
	if err := m.env.Q.InsertBenchmarkResult(ctx, params); err != nil {
		return fmt.Errorf("record benchmark result: %w", err)
	}
	return nil
}
