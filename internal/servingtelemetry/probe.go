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
	BackendUnknown  Backend = "unknown"
	BackendVLLM     Backend = "vllm"
	BackendSGLang   Backend = "sglang"
	BackendLlamaCPP Backend = "llamacpp"
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
	detectionTTL    = 60 * time.Second
	maxBodyBytes    = 1 << 20
	rateStaleWindow = 15 * time.Second
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

	llamaGen    map[int]float64
	llamaPrompt map[int]float64

	sglangSticky    float64
	sglangStickySet bool
	sglangStickyAt  time.Time
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
		st = &depState{llamaGen: map[int]float64{}, llamaPrompt: map[int]float64{}}
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
	if body, err := p.getBody(ctx, base+"/metrics"); err != nil {
		return err
	} else if strings.Contains(body, "vllm:") {
		st.backend = BackendVLLM
		st.backendAt = now
		return nil
	} else if strings.Contains(body, "sglang:") || strings.Contains(body, "sglang_generation_tokens_total") {
		st.backend = BackendSGLang
		st.backendAt = now
		return nil
	}
	if body, err := p.getBody(ctx, base+"/get_server_info"); err == nil && isJSON(body) {
		st.backend = BackendSGLang
		st.backendAt = now
		return nil
	}
	if body, err := p.getBody(ctx, base+"/slots"); err == nil && strings.HasPrefix(strings.TrimSpace(body), "[") {
		st.backend = BackendLlamaCPP
		st.backendAt = now
		return nil
	}
	st.backend = BackendUnknown
	return fmt.Errorf("%s", ErrUnsupported)
}

func isJSON(s string) bool {
	return json.Valid([]byte(s))
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
	waiting, _ := promSum(body, "vllm:num_requests_waiting")
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
	out.RequestsWaiting = int32(waiting)
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

func (p *Prober) probeLlamaCPP(ctx context.Context, st *depState, base string, now time.Time) telemetry.ServingPayload {
	body, err := p.getBody(ctx, base+"/slots")
	if err != nil {
		return p.fail(st, err)
	}
	var slots []map[string]any
	if err := json.Unmarshal([]byte(body), &slots); err != nil {
		return p.fail(st, fmt.Errorf("%s", ErrInvalidResponse))
	}
	out := telemetry.ServingPayload{Available: true, Backend: string(BackendLlamaCPP)}
	out.SlotsTotal = int32(len(slots))

	var totalGen, totalPrompt float64
	var live int
	for _, raw := range slots {
		totalGen += slotDecoded(raw)
		totalPrompt += slotPrompts(raw)
		if isProcessing(raw) {
			live++
		}
	}
	out.SlotsActive = int32(live)

	genRate, prefillRate := 0.0, 0.0
	if dt := now.Sub(st.last).Seconds(); dt > 0 && dt < rateStaleWindow.Seconds() {
		if st.gen > 0 && totalGen >= st.gen {
			genRate = (totalGen - st.gen) / dt
		}
		if st.prompt > 0 && totalPrompt >= st.prompt {
			prefillRate = (totalPrompt - st.prompt) / dt
		}
	}
	st.gen, st.prompt = totalGen, totalPrompt
	st.last = now

	out.GenerationTPS = &genRate
	out.PrefillTPS = &prefillRate
	if body, err := p.getBody(ctx, base+"/props"); err == nil {
		var props map[string]any
		if json.Unmarshal([]byte(body), &props) == nil {
			if m := strField(props, "model_alias"); m != "" {
				out.ModelID = m
			} else if m := strField(props, "model_path"); m != "" {
				out.ModelID = m
			}
			out.ContextLength = int32(numField(props, "total_context_length"))
		}
	}
	return out
}

func slotDecoded(s map[string]any) float64 {
	if v := numField(s, "n_decoded"); v != 0 {
		return v
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

func slotPrompts(s map[string]any) float64 {
	if v := numField(s, "n_prompt_tokens_processed"); v > 0 {
		return v
	}
	return numField(s, "n_prompt_tokens")
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
