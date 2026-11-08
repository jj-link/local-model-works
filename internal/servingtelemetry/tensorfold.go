package servingtelemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/telemetry"
)

type tensorFoldCounters struct {
	last       time.Time
	generation float64
	prompt     float64
	genSet     bool
	promptSet  bool
}

type tensorFoldHealth struct {
	OK               bool     `json:"ok"`
	Backend          string   `json:"backend"`
	RequestsRunning  *int32   `json:"requests_running"`
	CompletionTokens *float64 `json:"completion_tokens_total"`
	PromptTokens     *float64 `json:"prompt_tokens_total"`
	Drafted          *float64 `json:"drafted_total"`
	Accepted         *float64 `json:"accepted_total"`
	PoolTokens       *int64   `json:"pool_tokens"`
	PoolFreeTokens   *int64   `json:"pool_free_tokens"`
	ContextLength    int32    `json:"context_length"`
	Streams          *struct {
		Decoding   int32  `json:"decoding"`
		Prefilling int32  `json:"prefilling"`
		Filling    *int32 `json:"filling"`
		Paused     int32  `json:"paused"`
		Max        int32  `json:"max"`
	} `json:"streams"`
}

func parseTensorFoldHealth(body string) (tensorFoldHealth, error) {
	var health tensorFoldHealth
	if err := json.Unmarshal([]byte(body), &health); err != nil {
		return health, fmt.Errorf("%s", ErrInvalidResponse)
	}
	invalid := !health.OK || health.Backend != string(BackendTensorFold) ||
		health.RequestsRunning == nil || *health.RequestsRunning < 0 || health.ContextLength < 0
	for _, counter := range []*float64{health.CompletionTokens, health.PromptTokens, health.Drafted, health.Accepted} {
		invalid = invalid || (counter != nil && *counter < 0)
	}
	invalid = invalid || (health.PoolTokens != nil && *health.PoolTokens < 0) ||
		(health.PoolFreeTokens != nil && *health.PoolFreeTokens < 0)
	if streams := health.Streams; streams != nil {
		invalid = invalid || streams.Decoding < 0 || streams.Prefilling < 0 || streams.Paused < 0 || streams.Max < 0 ||
			(streams.Filling != nil && *streams.Filling < 0)
		filling := streams.Prefilling
		if streams.Filling != nil {
			filling = *streams.Filling
		}
		active := int64(streams.Decoding) + int64(filling) + int64(streams.Paused)
		invalid = invalid || active > int64(streams.Max)
	}
	if invalid {
		return health, fmt.Errorf("%s", ErrInvalidResponse)
	}
	return health, nil
}

func (p *Prober) probeTensorFold(ctx context.Context, st *depState, base string, now time.Time) telemetry.ServingPayload {
	body, err := p.getBody(ctx, base+"/health")
	if err != nil {
		return p.fail(st, err)
	}
	health, err := parseTensorFoldHealth(body)
	if err != nil {
		return p.fail(st, err)
	}
	st.failCount = 0
	out := telemetry.ServingPayload{
		Available:       true,
		Backend:         string(BackendTensorFold),
		RequestsRunning: *health.RequestsRunning,
		ContextLength:   health.ContextLength,
	}
	if streams := health.Streams; streams != nil {
		// GLM's filling and prefilling describe the same lanes. Paused
		// decoding lanes still occupy slots, but are excluded from decoding.
		filling := streams.Prefilling
		if streams.Filling != nil {
			filling = *streams.Filling
		}
		out.SlotsActive = streams.Decoding + filling + streams.Paused
		out.SlotsTotal = streams.Max
	}
	if health.PoolTokens != nil && health.PoolFreeTokens != nil &&
		*health.PoolTokens > 0 && *health.PoolFreeTokens <= *health.PoolTokens {
		ratio := float64(*health.PoolTokens-*health.PoolFreeTokens) / float64(*health.PoolTokens)
		out.KVCacheUsageRatio = &ratio
	}
	if metrics, err := p.getBody(ctx, base+"/metrics"); err == nil {
		out.TTFTP95Seconds = p95(metrics, "tensorfold:time_to_first_token_seconds")
		out.E2EP95Seconds = p95(metrics, "tensorfold:request_latency_seconds")
		if out.KVCacheUsageRatio == nil {
			out.KVCacheMaxStreamUsageRatio = tensorFoldMaxStreamKV(metrics)
			// Without a pool, each lane owns a cache of one context window, so
			// all lanes hold max windows and their ratios sum to the used share.
			if out.KVCacheMaxStreamUsageRatio != nil && out.SlotsTotal > 0 {
				if used, ok := promSum(metrics, "tensorfold:kv_cache_usage_ratio"); ok && used <= float64(out.SlotsTotal) {
					ratio := used / float64(out.SlotsTotal)
					out.KVCacheUsageRatio = &ratio
				}
			}
		}
	}
	if health.Drafted != nil && health.Accepted != nil &&
		*health.Drafted > 0 && *health.Accepted <= *health.Drafted {
		ratio := *health.Accepted / *health.Drafted
		out.SpecAcceptanceRatio = &ratio
	}

	// The pinned CUDA /health snapshot includes live reply tokens in the
	// completion counter. Prompt tokens are folded only when requests finish:
	// their rate is interval completed-request accounting, not prefill speed.
	// Draft acceptance likewise describes cumulative completed requests.
	counters := &st.tensorfold
	dt := now.Sub(counters.last)
	if !counters.last.IsZero() && dt > 0 && dt < rateStaleWindow {
		if health.CompletionTokens != nil && counters.genSet && *health.CompletionTokens >= counters.generation {
			rate := (*health.CompletionTokens - counters.generation) / dt.Seconds()
			out.GenerationTPS = &rate
		}
		if health.PromptTokens != nil && counters.promptSet && *health.PromptTokens >= counters.prompt {
			rate := (*health.PromptTokens - counters.prompt) / dt.Seconds()
			out.PrefillTPS = &rate
		}
	}
	// Presence is tracked independently of the value, so a valid zero baseline
	// works and an omitted counter must be observed again before yielding rates.
	counters.genSet = health.CompletionTokens != nil
	if counters.genSet {
		counters.generation = *health.CompletionTokens
	}
	counters.promptSet = health.PromptTokens != nil
	if counters.promptSet {
		counters.prompt = *health.PromptTokens
	}
	counters.last = now
	return out
}

// TensorFold exports one logical context/window ratio per active stream.
// Keep its maximum separate from shared-pool occupancy: adding the ratios
// can exceed one, and idle retained prefixes are absent from these gauges.
func tensorFoldMaxStreamKV(body string) *float64 {
	return maxRatioGauge(body, "tensorfold:kv_cache_usage_ratio")
}

// maxRatioGauge is the largest series of a 0..1 gauge; any out-of-range or
// unparseable series makes the whole gauge unavailable.
func maxRatioGauge(body, name string) *float64 {
	var highest float64
	found := false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, name) {
			continue
		}
		suffix := line[len(name):]
		if suffix != "" && suffix[0] != '{' && suffix[0] != ' ' && suffix[0] != '\t' {
			continue
		}
		match := seriesRe.FindStringSubmatch(line)
		if match == nil {
			return nil
		}
		value, err := strconv.ParseFloat(match[2], 64)
		if err != nil || value < 0 || value > 1 {
			return nil
		}
		if !found || value > highest {
			highest = value
		}
		found = true
	}
	if !found {
		return nil
	}
	return &highest
}

// tensorFoldLanesHealth is the lane server's /health. It carries no backend
// identity; live holds the server's own current throughput.
type tensorFoldLanesHealth struct {
	MaxBatchSize int32 `json:"max_batch_size"`
	Live         *struct {
		Decode  *float64 `json:"decode_tokens_per_second"`
		Prefill *float64 `json:"prefill_tokens_per_second"`
	} `json:"live"`
}

// probeTensorFoldLanes reads tf-dsv41-lanes, whose /metrics mirrors vLLM's
// names under the tensorfold: prefix. Throughput comes from the server's live
// rates; /health is optional, so its failure only drops those fields.
func (p *Prober) probeTensorFoldLanes(ctx context.Context, st *depState, base string, now time.Time, poolShare float64) telemetry.ServingPayload {
	body, err := p.getBody(ctx, base+"/metrics")
	if err != nil {
		return p.fail(st, err)
	}
	running, ok := promSum(body, "tensorfold:num_requests_running")
	if !ok || running < 0 {
		return p.fail(st, fmt.Errorf("%s", ErrInvalidResponse))
	}
	st.failCount = 0
	out := telemetry.ServingPayload{
		Available:       true,
		Backend:         string(BackendTensorFold),
		RequestsRunning: int32(running),
		SlotsActive:     int32(running),
	}
	if waiting, ok := promSum(body, "tensorfold:num_requests_waiting"); ok && waiting >= 0 {
		value := int32(waiting)
		out.RequestsWaiting = &value
	}
	out.KVCacheMaxStreamUsageRatio = maxRatioGauge(body, "tensorfold:kv_cache_usage_perc")
	if hits, ok := promSum(body, "tensorfold:prefix_cache_hits_total"); ok {
		if queries, _ := promSum(body, "tensorfold:prefix_cache_queries_total"); queries > 0 && hits <= queries {
			r := hits / queries
			out.PrefixCacheHitRatio = &r
		}
	}
	if accepted, ok := promSum(body, "tensorfold:spec_decode_num_accepted_tokens_total"); ok {
		if drafted, _ := promSum(body, "tensorfold:spec_decode_num_draft_tokens_total"); drafted > 0 && accepted <= drafted {
			r := accepted / drafted
			out.SpecAcceptanceRatio = &r
		}
	}
	if preempts, ok := promSum(body, "tensorfold:preemptions_total"); ok && preempts >= 0 {
		out.PreemptionsTotal = int64(preempts)
	}
	out.TTFTP95Seconds = p95(body, "tensorfold:time_to_first_token_seconds")
	out.E2EP95Seconds = p95(body, "tensorfold:request_latency_seconds")
	out.ITLP95Seconds = p95(body, "tensorfold:request_time_per_output_token_seconds")

	if raw, err := p.getBody(ctx, base+"/health"); err == nil {
		var health tensorFoldLanesHealth
		if json.Unmarshal([]byte(raw), &health) == nil {
			if health.MaxBatchSize > 0 && int32(running) <= health.MaxBatchSize {
				out.SlotsTotal = health.MaxBatchSize
				// Each lane's ratio is over the --context window; all lanes share
				// one position pool of --pool (or --context), so the scaled sum is
				// the pool's live share; kept prompts are not in it.
				if out.KVCacheMaxStreamUsageRatio != nil {
					if used, ok := promSum(body, "tensorfold:kv_cache_usage_perc"); ok {
						if used *= poolShare; used <= 1 {
							out.KVCacheUsageRatio = &used
						}
					}
				}
			}
			if live := health.Live; live != nil {
				if live.Decode != nil && *live.Decode >= 0 {
					out.GenerationTPS = live.Decode
				}
				if live.Prefill != nil && *live.Prefill >= 0 {
					out.PrefillTPS = live.Prefill
				}
			}
		}
	}
	return out
}

// lanePoolShare is context/pool for a lane server whose --pool exceeds its
// --context, and 1 when the pool is the context window.
func lanePoolShare(params map[string]any) float64 {
	window, _ := params["context"].(float64)
	pool, _ := params["pool"].(float64)
	if window > 0 && pool > window {
		return window / pool
	}
	return 1
}
