package servingtelemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/telemetry"
)

// Backend is the detected serving engine class.
type Backend string

const (
	BackendUnknown    Backend = "unknown"
	BackendVLLM       Backend = "vllm"
	BackendSGLang     Backend = "sglang"
	BackendLlamaCPP   Backend = "llamacpp"
	BackendTensorFold Backend = "tensorfold"
)

// Persisted monitor error codes. Response bodies and credentials are never
// retained; only a stable code and a short message are stored.
const (
	ErrTimeout          = "timeout"
	ErrUnreachable      = "unreachable"
	ErrUnauthorized     = "unauthorized"
	ErrUnsupported      = "unsupported"
	ErrInvalidResponse  = "invalid_response"
	ErrResponseTooLarge = "response_too_large"
)

const (
	detectionTTL       = 60 * time.Second
	maxBodyBytes       = 1 << 20
	rateStaleWindow    = 15 * time.Second
	errEndpointMissing = "endpoint_missing"
)

// Prober owns per-deployment detection and counter state. One Prober serves
// the poller's lifetime; Collect calls Probe concurrently per deployment.
type Prober struct {
	client *http.Client
	states map[string]*depState
	// now is the clock used for rate/sticky windows; overridable in tests.
	now func() time.Time
}

// depState is one deployment's detection and counter baselines. Counter
// cumulative token values are float64; Prometheus parses them as such and the
// values are far below float64's 2^53 exact-integer limit.
type depState struct {
	backend   Backend
	backendAt time.Time
	failCount int

	last    time.Time
	gen     float64
	prompt  float64
	iter    float64
	ttftSum float64

	llamaSlots     map[int]llamaSlotCounters
	llamaCompleted float64
	llamaGenSet    bool
	llamaPromptSet bool

	sglangSticky    float64
	sglangStickySet bool
	sglangStickyAt  time.Time

	tensorfold      tensorFoldCounters
	tensorfoldLanes bool
}

// NewProber builds a Prober around an injected HTTP client (tests supply a
// fixture transport).
func NewProber(client *http.Client) *Prober {
	return &Prober{client: client, states: map[string]*depState{}, now: time.Now}
}

// Probe samples one deployment's serving telemetry. Monitoring never changes
// the deployment's observed state; errors become monitor status only.
func (p *Prober) Probe(ctx context.Context, dep deploy.MonitorTarget) telemetry.ServingPayload {
	st := p.states[dep.ID]
	if st == nil {
		st = &depState{}
		p.states[dep.ID] = st
	}
	base := fmt.Sprintf("http://%s:%d", dep.Endpoint.Host, dep.Endpoint.Port)
	now := p.now()

	if st.backend == BackendUnknown || now.Sub(st.backendAt) >= detectionTTL {
		if err := p.detect(ctx, st, base, now); err != nil {
			return p.fail(st, err)
		}
	}

	switch st.backend {
	case BackendVLLM:
		return p.probeVLLM(ctx, st, base, now)
	case BackendSGLang:
		return p.probeSGLang(ctx, st, base, now)
	case BackendLlamaCPP:
		return p.probeLlamaCPP(ctx, st, base, now)
	case BackendTensorFold:
		if st.tensorfoldLanes {
			return p.probeTensorFoldLanes(ctx, st, base, now)
		}
		return p.probeTensorFold(ctx, st, base, now)
	default:
		return p.fail(st, fmt.Errorf("%s", ErrUnsupported))
	}
}

// fail records a monitor-only failure; after three consecutive failures the
// detection and rate baselines are cleared so the next poll re-detects.
func (p *Prober) fail(st *depState, err error) telemetry.ServingPayload {
	st.failCount++
	if st.failCount >= 3 {
		st.backend = BackendUnknown
		st.gen, st.prompt, st.iter, st.ttftSum = 0, 0, 0, 0
		st.sglangSticky, st.sglangStickySet = 0, false
		st.last = time.Time{}
		st.tensorfold = tensorFoldCounters{}
		st.llamaSlots = nil
		st.llamaGenSet, st.llamaPromptSet = false, false
	}
	code := monitorCode(err)
	msg := err.Error()
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return telemetry.ServingPayload{
		Backend:   string(BackendUnknown),
		ErrorCode: &code,
		Error:     &msg,
	}
}

func monitorCode(err error) string {
	s := err.Error()
	switch {
	case s == ErrUnsupported:
		return ErrUnsupported
	case s == ErrInvalidResponse:
		return ErrInvalidResponse
	case s == ErrUnauthorized:
		return ErrUnauthorized
	case strings.Contains(s, "timeout"):
		return ErrTimeout
	case strings.Contains(s, "oversized"):
		return ErrResponseTooLarge
	default:
		return ErrUnreachable
	}
}

// getBody fetches a GET body with the client's limits. Response bodies are
// read only for parsing and discarded immediately.
func (p *Prober) getBody(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("timeout")
		}
		return "", fmt.Errorf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%s", ErrUnauthorized)
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return "", fmt.Errorf("%s", errEndpointMissing)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}
	if resp.ContentLength > maxBodyBytes {
		return "", fmt.Errorf("oversized response")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read: %v", err)
	}
	if len(body) > maxBodyBytes {
		return "", fmt.Errorf("oversized response")
	}
	return string(body), nil
}

// --------------------------------------------------------------------------
// detection
// --------------------------------------------------------------------------

func (p *Prober) detect(ctx context.Context, st *depState, base string, now time.Time) error {
	// /metrics is the primary classifier; a real failure (timeout, unreachable,
	// unauthorized, oversized) is surfaced as monitor status rather than
	// treated as "no matching backend".
	st.tensorfoldLanes = false
	lanes := false
	if body, err := p.getBody(ctx, base+"/metrics"); err != nil {
		if err.Error() != errEndpointMissing {
			return err
		}
	} else if strings.Contains(body, "vllm:") {
		st.backend = BackendVLLM
		st.backendAt = now
		return nil
	} else if strings.Contains(body, "sglang:") || strings.Contains(body, "sglang_generation_tokens_total") {
		st.backend = BackendSGLang
		st.backendAt = now
		return nil
	} else if _, ok := promSum(body, "llamacpp:tokens_predicted_total"); ok {
		st.backend = BackendLlamaCPP
		st.backendAt = now
		return nil
	} else {
		_, lanes = promSum(body, "tensorfold:num_requests_running")
	}
	// TensorFold's CUDA health document identifies its backend explicitly.
	// Check it before the legacy server-info classifier.
	body, healthErr := p.getBody(ctx, base+"/health")
	if healthErr == nil {
		var identity struct {
			Backend string `json:"backend"`
		}
		if json.Unmarshal([]byte(body), &identity) == nil && identity.Backend == string(BackendTensorFold) {
			if _, err := parseTensorFoldHealth(body); err != nil {
				return err
			}
			st.backend = BackendTensorFold
			st.backendAt = now
			return nil
		}
	}
	// TensorFold's lane server (tf-dsv41-lanes) omits the health identity and
	// publishes vLLM-style names under the tensorfold: prefix instead.
	if lanes {
		st.backend = BackendTensorFold
		st.tensorfoldLanes = true
		st.backendAt = now
		return nil
	}
	if body, err := p.getBody(ctx, base+"/get_server_info"); err == nil && isSGLangInfo(body) {
		st.backend = BackendSGLang
		st.backendAt = now
		return nil
	}
	if body, err := p.getBody(ctx, base+"/slots"); err == nil && isLlamaSlots(body) {
		st.backend = BackendLlamaCPP
		st.backendAt = now
		return nil
	}
	// A health failure must not prevent classification of legacy engines.
	if healthErr != nil && healthErr.Error() != errEndpointMissing {
		return healthErr
	}
	st.backend = BackendUnknown
	return fmt.Errorf("%s", ErrUnsupported)
}

func isSGLangInfo(body string) bool {
	var info map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &info) != nil || info["error"] != nil {
		return false
	}
	for _, key := range []string{"internal_states", "last_gen_throughput", "total_input_tokens", "total_output_tokens", "max_running_requests"} {
		if info[key] != nil {
			return true
		}
	}
	return false
}

func isLlamaSlots(body string) bool {
	var slots []map[string]any
	if json.Unmarshal([]byte(body), &slots) != nil || slots == nil {
		return false
	}
	for _, slot := range slots {
		if _, ok := slot["id"]; !ok {
			return false
		}
		if _, ok := slot["is_processing"]; !ok && strField(slot, "state") == "" {
			return false
		}
	}
	return true
}

// --------------------------------------------------------------------------
// vLLM
// --------------------------------------------------------------------------

func (p *Prober) probeVLLM(ctx context.Context, st *depState, base string, now time.Time) telemetry.ServingPayload {
	body, err := p.getBody(ctx, base+"/metrics")
	if err != nil {
		return p.fail(st, err)
	}
	out := telemetry.ServingPayload{Available: true, Backend: string(BackendVLLM)}
	gen, _ := promSum(body, "vllm:generation_tokens_total")
	prompt, _ := promSum(body, "vllm:prompt_tokens_total")
	iter, _ := promSum(body, "vllm:iteration_tokens_total_sum")
	ttftSum, _ := promSum(body, "vllm:time_to_first_token_seconds_sum")
	running, _ := promSum(body, "vllm:num_requests_running")
	waiting, waitingSet := promSum(body, "vllm:num_requests_waiting")
	kv, _ := promSum(body, "vllm:kv_cache_usage_perc")
	preempts, _ := promSum(body, "vllm:num_preemptions_total")
	prefixHits, _ := promSum(body, "vllm:prefix_cache_hits_total")
	prefixQueries, _ := promSum(body, "vllm:prefix_cache_queries_total")
	accepted, _ := promSum(body, "vllm:spec_decode_num_accepted_tokens_total")
	drafted, _ := promSum(body, "vllm:spec_decode_num_draft_tokens_total")

	genRate, prefillRate := 0.0, 0.0
	if dt := now.Sub(st.last).Seconds(); dt > 0 && dt < rateStaleWindow.Seconds() {
		if st.gen > 0 && gen >= st.gen {
			genRate = (gen - st.gen) / dt
		}
		iterDelta := 0.0
		if st.iter > 0 && iter >= st.iter {
			iterDelta = iter - st.iter
		}
		iterSurplus := iterDelta - genRate*dt
		if iterSurplus < 0 {
			iterSurplus = 0
		}
		specNoise := iterSurplus > 0 && genRate > 0 && iterSurplus < genRate*dt*0.5
		switch {
		case iterSurplus > 0 && !specNoise:
			prefillRate = iterSurplus / dt
		case st.prompt > 0 && prompt >= st.prompt:
			if prompt-st.prompt > 0 && ttftSum-st.ttftSum > 0 {
				prefillRate = (prompt - st.prompt) / (ttftSum - st.ttftSum)
			} else {
				prefillRate = (prompt - st.prompt) / dt
			}
		}
	}
	st.gen, st.prompt, st.iter, st.ttftSum = gen, prompt, iter, ttftSum
	st.last = now

	out.GenerationTPS = &genRate
	out.PrefillTPS = &prefillRate
	out.RequestsRunning = int32(running)
	if waitingSet && waiting >= 0 {
		value := int32(waiting)
		out.RequestsWaiting = &value
	}
	out.SlotsActive = int32(running)
	if kv > 0 {
		r := kv / 100
		out.KVCacheUsageRatio = &r
	}
	out.PreemptionsTotal = int64(preempts)
	if prefixQueries > 0 {
		r := prefixHits / prefixQueries
		out.PrefixCacheHitRatio = &r
	}
	out.TTFTP95Seconds = p95(body, "vllm:time_to_first_token_seconds")
	out.E2EP95Seconds = p95(body, "vllm:e2e_request_latency_seconds")
	out.ITLP95Seconds = p95(body, "vllm:inter_token_latency_seconds")
	if drafted > 0 {
		r := accepted / drafted
		out.SpecAcceptanceRatio = &r
	}
	return out
}

// --------------------------------------------------------------------------
// SGLang
// --------------------------------------------------------------------------

func (p *Prober) probeSGLang(ctx context.Context, st *depState, base string, now time.Time) telemetry.ServingPayload {
	out := telemetry.ServingPayload{Available: true, Backend: string(BackendSGLang)}

	// Prefer cumulative counters from /metrics.
	if body, err := p.getBody(ctx, base+"/metrics"); err == nil {
		if gen, ok := promSumAlt(body, "sglang:generation_tokens_total", "sglang_generation_tokens_total"); ok {
			prompt, _ := promSumAlt(body, "sglang:prompt_tokens_total", "sglang_prompt_tokens_total")
			running, _ := promSumAlt(body, "sglang:num_running_reqs", "sglang_num_running_reqs")
			cached, _ := promSumAlt(body, "sglang:cached_tokens_total", "sglang_cached_tokens_total")

			genRate, prefillRate := 0.0, 0.0
			if dt := now.Sub(st.last).Seconds(); dt > 0 && dt < rateStaleWindow.Seconds() {
				if st.gen > 0 && gen >= st.gen {
					genRate = (gen - st.gen) / dt
				}
				if st.prompt > 0 && prompt >= st.prompt {
					prefillRate = (prompt - st.prompt) / dt
				}
			}
			st.gen, st.prompt = gen, prompt
			st.last = now
			out.GenerationTPS = &genRate
			out.PrefillTPS = &prefillRate
			out.RequestsRunning = int32(running)
			out.SlotsActive = int32(running)
			if prompt > 0 {
				r := cached / prompt
				out.PrefixCacheHitRatio = &r
			}
			if total, ok := promSumAlt(body, "sglang:max_total_num_tokens", "sglang_max_total_num_tokens"); ok && total > 0 {
				if used, ok := promSumAlt(body, "sglang:num_used_tokens", "sglang_num_used_tokens"); ok && used >= 0 && used <= total {
					r := used / total
					out.KVCacheUsageRatio = &r
				}
			}
			return out
		}
	}

	// Fall back to /get_server_info totals.
	if body, err := p.getBody(ctx, base+"/get_server_info"); err == nil {
		var info struct {
			TotalInputTokens  float64 `json:"total_input_tokens"`
			TotalOutputTokens float64 `json:"total_output_tokens"`
			LastGenThroughput float64 `json:"last_gen_throughput"`
			InternalStates    []struct {
				LastGenThroughput float64 `json:"last_gen_throughput"`
			} `json:"internal_states"`
			MaxRunning float64 `json:"max_running_requests"`
			Context    float64 `json:"context_length"`
		}
		_ = json.Unmarshal([]byte(body), &info)
		if info.TotalInputTokens != 0 || info.TotalOutputTokens != 0 {
			genRate, prefillRate := 0.0, 0.0
			if dt := now.Sub(st.last).Seconds(); dt > 0 && dt < rateStaleWindow.Seconds() {
				if st.gen > 0 && info.TotalOutputTokens >= st.gen {
					genRate = (info.TotalOutputTokens - st.gen) / dt
				}
				if st.prompt > 0 && info.TotalInputTokens >= st.prompt {
					prefillRate = (info.TotalInputTokens - st.prompt) / dt
				}
			}
			st.gen, st.prompt = info.TotalOutputTokens, info.TotalInputTokens
			st.last = now
			out.GenerationTPS = &genRate
			out.PrefillTPS = &prefillRate
			out.SlotsTotal = int32(info.MaxRunning)
			out.ContextLength = int32(info.Context)
			return out
		}
		raw := info.LastGenThroughput
		for _, s := range info.InternalStates {
			if s.LastGenThroughput > raw {
				raw = s.LastGenThroughput
			}
		}
		r := p.sglangSticky(st, raw, now)
		out.GenerationTPS = &r
		out.SlotsTotal = int32(info.MaxRunning)
		out.ContextLength = int32(info.Context)
		return out
	}

	return p.fail(st, fmt.Errorf("unreachable server info"))
}

// sglangSticky implements the SGLang sticky-throughput rule: 0 until the
// gauge changes between polls, then live for a short window after each change.
func (p *Prober) sglangSticky(st *depState, raw float64, now time.Time) float64 {
	if raw < 0 {
		return 0
	}
	if !st.sglangStickySet {
		// First sample after reset seeds only and stays 0 until the gauge
		// changes between polls (avoids showing a stale idle leftover).
		st.sglangSticky = raw
		st.sglangStickySet = true
		return 0
	}
	if raw != st.sglangSticky {
		st.sglangSticky = raw
		st.sglangStickyAt = now
		return raw
	}
	if now.Sub(st.sglangStickyAt) <= 6*time.Second {
		return raw
	}
	return 0
}

// --------------------------------------------------------------------------
// llama.cpp
// --------------------------------------------------------------------------

type llamaSlotCounters struct {
	task       float64
	processing bool
	generation float64
	prompt     float64
	genSet     bool
	promptSet  bool
}

func llamaCounters(slot map[string]any) llamaSlotCounters {
	_, genSet := slot["n_decoded"]
	if next, ok := slot["next_token"].([]any); !genSet && ok && len(next) > 0 {
		if token, ok := next[0].(map[string]any); ok {
			_, genSet = token["n_decoded"]
		}
	}
	_, promptSet := slot["n_prompt_tokens_processed"]
	return llamaSlotCounters{
		task:       numField(slot, "id_task"),
		processing: isProcessing(slot),
		generation: slotDecoded(slot),
		prompt:     numField(slot, "n_prompt_tokens_processed"),
		genSet:     genSet,
		promptSet:  promptSet,
	}
}

func (p *Prober) probeLlamaCPP(ctx context.Context, st *depState, base string, now time.Time) telemetry.ServingPayload {
	body, err := p.getBody(ctx, base+"/slots")
	if err != nil {
		return p.fail(st, err)
	}
	var slots []map[string]any
	if err := json.Unmarshal([]byte(body), &slots); err != nil || slots == nil {
		return p.fail(st, fmt.Errorf("%s", ErrInvalidResponse))
	}
	metrics, _ := p.getBody(ctx, base+"/metrics")
	completed, completedSet := promSum(metrics, "llamacpp:tokens_predicted_total")
	prompt, promptSet := promSum(metrics, "llamacpp:prompt_tokens_total")

	// These endpoints are not an atomic snapshot. Bracket completion counters
	// with slot reads: a release/replacement during collection would otherwise
	// count the same generation in both the completed and active totals.
	coherent := true
	if completedSet {
		body, err = p.getBody(ctx, base+"/slots")
		var after []map[string]any
		if err != nil || json.Unmarshal([]byte(body), &after) != nil || after == nil {
			coherent = false
		} else {
			coherent = len(slots) == len(after)
			if coherent {
				for i, slot := range slots {
					next := after[i]
					if numField(slot, "id") != numField(next, "id") ||
						numField(slot, "id_task") != numField(next, "id_task") ||
						isProcessing(slot) != isProcessing(next) {
						coherent = false
						break
					}
				}
			}
			slots = after
		}
	}

	out := telemetry.ServingPayload{Available: true, Backend: string(BackendLlamaCPP)}
	out.SlotsTotal = int32(len(slots))
	dt := now.Sub(st.last)
	fresh := !st.last.IsZero() && dt > 0 && dt < rateStaleWindow
	genDelta, promptDelta, activeGen := 0.0, 0.0, 0.0
	genValid, promptValid := len(st.llamaSlots) == len(slots), len(st.llamaSlots) == len(slots)
	activeGenSet := true
	regressed := false
	if st.llamaSlots == nil {
		st.llamaSlots = make(map[int]llamaSlotCounters, len(slots))
	}
	for _, slot := range slots {
		id := int(numField(slot, "id"))
		current := llamaCounters(slot)
		previous, exists := st.llamaSlots[id]
		if current.processing {
			out.SlotsActive++
			activeGen += current.generation
			activeGenSet = activeGenSet && current.genSet && current.generation >= 0
		}
		if ctx := int32(numField(slot, "n_ctx")); ctx > out.ContextLength {
			out.ContextLength = ctx
		}
		sameTask := exists && current.task == previous.task && current.processing == previous.processing
		if !sameTask {
			genValid, promptValid = false, false
		} else if current.processing {
			if !current.genSet || !previous.genSet || current.generation < previous.generation {
				genValid = false
				regressed = true
			} else {
				genDelta += current.generation - previous.generation
			}
			if !current.promptSet || !previous.promptSet || current.prompt < previous.prompt {
				promptValid = false
			} else {
				promptDelta += current.prompt - previous.prompt
			}
		}
		st.llamaSlots[id] = current
	}
	// Forget removed slots so reappearing IDs cannot inherit stale tasks.
	for id := range st.llamaSlots {
		found := false
		for _, slot := range slots {
			if int(numField(slot, "id")) == id {
				found = true
				break
			}
		}
		if !found {
			delete(st.llamaSlots, id)
		}
	}
	out.RequestsRunning = out.SlotsActive
	if running, ok := promSum(metrics, "llamacpp:requests_processing"); ok && running >= 0 {
		coherent = coherent && running == float64(out.SlotsActive)
		out.RequestsRunning = int32(running)
	}
	if waiting, ok := promSum(metrics, "llamacpp:requests_deferred"); ok && waiting >= 0 {
		value := int32(waiting)
		out.RequestsWaiting = &value
	}

	// llama.cpp commits generated tokens on slot release. Adding only active
	// tokens gives a continuous total, including the final unsampled tail on
	// completion, without charging the whole request a second time.
	totalGen := completed + activeGen
	if fresh {
		if completedSet {
			if coherent && activeGenSet && !regressed && st.llamaGenSet &&
				completed >= st.llamaCompleted && totalGen >= st.gen {
				rate := (totalGen - st.gen) / dt.Seconds()
				out.GenerationTPS = &rate
			}
		} else if genValid {
			rate := genDelta / dt.Seconds()
			out.GenerationTPS = &rate
		}
		// Prompt counters are updated per decoded prefill batch, not at
		// request completion. Do not add active prompt tokens to them.
		if promptSet {
			if st.llamaPromptSet && prompt >= st.prompt {
				rate := (prompt - st.prompt) / dt.Seconds()
				out.PrefillTPS = &rate
			}
		} else if promptValid {
			rate := promptDelta / dt.Seconds()
			out.PrefillTPS = &rate
		}
	}
	st.gen, st.prompt, st.llamaCompleted = totalGen, prompt, completed
	st.llamaGenSet = completedSet && coherent && activeGenSet
	st.llamaPromptSet = promptSet && prompt >= 0
	st.last = now
	st.failCount = 0

	if cached, ok := promSum(metrics, "llamacpp:prompt_tokens_cached_total"); ok &&
		promptSet && cached >= 0 && prompt >= 0 && cached+prompt > 0 {
		ratio := cached / (cached + prompt)
		out.PrefixCacheHitRatio = &ratio
	}
	drafted, draftedSet := promSum(metrics, "llamacpp:spec_decode_num_draft_tokens_total")
	accepted, acceptedSet := promSum(metrics, "llamacpp:spec_decode_num_accepted_tokens_total")
	if draftedSet && acceptedSet && drafted > 0 && accepted >= 0 && accepted <= drafted {
		ratio := accepted / drafted
		out.SpecAcceptanceRatio = &ratio
	}
	// n_tokens_max is a historical high-water mark, not live KV occupancy.
	// Neither these endpoints nor allocated GPU memory provide a KV fraction
	// or latency quantiles; leave those optional metrics unavailable.
	if body, err := p.getBody(ctx, base+"/props"); err == nil {
		var props map[string]any
		if json.Unmarshal([]byte(body), &props) == nil {
			if m := strField(props, "model_alias"); m != "" {
				out.ModelID = m
			} else if m := strField(props, "model_path"); m != "" {
				out.ModelID = m
			}
			if total := int32(numField(props, "total_slots")); total > 0 {
				out.SlotsTotal = total
			}
			if settings, ok := props["default_generation_settings"].(map[string]any); ok {
				if ctx := int32(numField(settings, "n_ctx")); ctx > 0 {
					out.ContextLength = ctx
				}
			}
			if out.ContextLength == 0 {
				out.ContextLength = int32(numField(props, "total_context_length"))
			}
		}
	}
	return out
}

func slotDecoded(s map[string]any) float64 {
	if _, ok := s["n_decoded"]; ok {
		return numField(s, "n_decoded")
	}
	if a, ok := s["next_token"].([]any); ok && len(a) > 0 {
		if m, ok := a[0].(map[string]any); ok {
			if v := numField(m, "n_decoded"); v != 0 {
				return v
			}
		}
	}
	return 0
}

func isProcessing(s map[string]any) bool {
	if b, ok := s["is_processing"].(bool); ok {
		return b
	}
	if state := strField(s, "state"); state != "" && state != "idle" {
		return true
	}
	return false
}

// numField extracts a numeric field from a JSON object, tolerating numbers
// and strings.
func numField(m map[string]any, key string) float64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	case []any:
		if len(n) > 0 {
			if f, ok := n[0].(float64); ok {
				return f
			}
		}
	}
	return 0
}

func strField(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// --------------------------------------------------------------------------
// Prometheus text parsing
// --------------------------------------------------------------------------

var seriesRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}]*\})?\s+([-+0-9.eE]+)\s*$`)

// promSum sums every series matching name, returning whether any matched.
func promSum(body, name string) (float64, bool) {
	return promSumAlt(body, name, "")
}

func promSumAlt(body, a, b string) (float64, bool) {
	var sum float64
	found := false
	for _, line := range strings.Split(body, "\n") {
		m := seriesRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != a && (b == "" || m[1] != b) {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		sum += v
		found = true
	}
	return sum, found
}

// p95 computes the 95th percentile from a Prometheus histogram's cumulative
// buckets; returns nil when no finite bucket covers the target.
func p95(body, prefix string) *float64 {
	bucketRe := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(prefix) + `_bucket\{[^}]*\ble="([^"]+)"[^}]*\}\s+([-+0-9.eE]+)`)
	byUpper := map[float64]float64{}
	var inf, total float64
	for _, m := range bucketRe.FindAllStringSubmatch(body, -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		if m[1] == "+Inf" {
			inf += v
			continue
		}
		upper, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		byUpper[upper] += v
	}
	if c, ok := promSum(body, prefix+"_count"); ok {
		total = c
	} else {
		total = inf
	}
	if total <= 0 {
		return nil
	}
	if inf > 0 && ((inf-total) > 1e-6 || (total-inf) > 1e-6) {
		return nil
	}
	uppers := make([]float64, 0, len(byUpper))
	for u := range byUpper {
		uppers = append(uppers, u)
	}
	sort.Float64s(uppers)
	target := total * 0.95
	var prevUpper, prevCount float64
	for _, u := range uppers {
		count := byUpper[u]
		if count >= target {
			if count == prevCount {
				return &u
			}
			q := prevUpper + (u-prevUpper)*(target-prevCount)/(count-prevCount)
			return &q
		}
		prevUpper, prevCount = u, count
	}
	return nil
}
