package servingtelemetry

import (
	"context"
	"encoding/json"
	"fmt"
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
