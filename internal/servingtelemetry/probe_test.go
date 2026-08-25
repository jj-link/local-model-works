package servingtelemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/deploy"
)

func dep(id string, port int) deploy.MonitorTarget {
	return deploy.MonitorTarget{ID: id, Endpoint: deploy.Endpoint{Host: "127.0.0.1", Port: int32(port)}}
}

// portOf extracts the listening port from an httptest URL.
func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func portOf(url string) int {
	i := strings.LastIndex(url, ":")
	n, _ := strconv.Atoi(url[i+1:])
	return n
}

func TestVllmTwoSampleRates(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	var gen, prompt atomic.Int64
	gen.Store(100)
	prompt.Store(50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "vllm:generation_tokens_total{engine=\"0\"} %d\nvllm:prompt_tokens_total{engine=\"0\"} %d\n", gen.Load(), prompt.Load())
	}))
	defer srv.Close()

	p := NewProber(srv.Client())
	p.now = func() time.Time { return base }
	ctx := context.Background()
	d := dep("d1", portOf(srv.URL))

	// First sample seeds counters (no rate).
	first := p.Probe(ctx, d)
	if first.GenerationTPS == nil || *first.GenerationTPS != 0 {
		t.Fatalf("first gen=%v want 0", first.GenerationTPS)
	}

	// Second sample five seconds later with advanced counters.
	gen.Store(150)
	prompt.Store(70)
	p.now = func() time.Time { return base.Add(5 * time.Second) }
	second := p.Probe(ctx, d)
	if second.GenerationTPS == nil || *second.GenerationTPS != 10 {
		t.Fatalf("gen tps=%v want 10", second.GenerationTPS)
	}
	if second.PrefillTPS == nil || *second.PrefillTPS != 4 {
		t.Fatalf("prefill tps=%v want 4", second.PrefillTPS)
	}

	// Unchanged third counters yield zero rates.
	p.now = func() time.Time { return base.Add(10 * time.Second) }
	third := p.Probe(ctx, d)
	if third.GenerationTPS == nil || *third.GenerationTPS != 0 {
		t.Fatalf("third gen=%v want 0", third.GenerationTPS)
	}
}

func TestVllmQueueCacheLatency(t *testing.T) {
	body := `vllm:generation_tokens_total 100
vllm:num_requests_running 2
vllm:num_requests_waiting 1
vllm:kv_cache_usage_perc 42
vllm:num_preemptions_total 3
vllm:prefix_cache_hits_total 10
vllm:prefix_cache_queries_total 20
vllm:spec_decode_num_accepted_tokens_total 80
vllm:spec_decode_num_draft_tokens_total 100
vllm:time_to_first_token_seconds_bucket{le="0.1"} 500
vllm:time_to_first_token_seconds_bucket{le="1.0"} 1000
vllm:time_to_first_token_seconds_bucket{le="+Inf"} 1000
vllm:time_to_first_token_seconds_count 1000
`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	p.now = func() time.Time { return time.Unix(1_800_000_000, 0) }
	out := p.Probe(context.Background(), dep("d1", portOf(srv.URL)))
	if out.RequestsRunning != 2 || out.RequestsWaiting != 1 || out.SlotsActive != 2 {
		t.Fatalf("load wrong: %+v", out)
	}
	if out.KVCacheUsageRatio == nil || *out.KVCacheUsageRatio != 0.42 {
		t.Fatalf("kv ratio=%v", out.KVCacheUsageRatio)
	}
	if out.PrefixCacheHitRatio == nil || *out.PrefixCacheHitRatio != 0.5 {
		t.Fatalf("prefix ratio=%v", out.PrefixCacheHitRatio)
	}
	if out.SpecAcceptanceRatio == nil || *out.SpecAcceptanceRatio != 0.8 {
		t.Fatalf("spec=%v", out.SpecAcceptanceRatio)
	}
	if out.TTFTP95Seconds == nil || abs(*out.TTFTP95Seconds-0.91) > 0.001 {
		t.Fatalf("ttft p95=%v want ~0.91", *out.TTFTP95Seconds)
	}
	if out.PreemptionsTotal != 3 {
		t.Fatalf("preempts=%d", out.PreemptionsTotal)
	}
}

func TestSglangStickyExpiresToZero(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	current := 29.746
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"last_gen_throughput":%v,"internal_states":[{"last_gen_throughput":%v}]}`, current, current)
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	p.now = func() time.Time { return base }
	ctx := context.Background()
	d := dep("d1", portOf(srv.URL))

	// First probe seeds sticky → 0 (stale leftover not shown).
	if out := p.Probe(ctx, d); out.GenerationTPS == nil || *out.GenerationTPS != 0 {
		t.Fatalf("seed gen=%v want 0", out.GenerationTPS)
	}
	// Unchanged gauge stays 0.
	p.now = func() time.Time { return base.Add(2 * time.Second) }
	if out := p.Probe(ctx, d); *out.GenerationTPS != 0 {
		t.Fatalf("idle gen=%v want 0", *out.GenerationTPS)
	}
	// Gauge changes → live for the 6s window.
	current = 41.2
	p.now = func() time.Time { return base.Add(4 * time.Second) }
	if out := p.Probe(ctx, d); *out.GenerationTPS != 41.2 {
		t.Fatalf("changed gen=%v want 41.2", *out.GenerationTPS)
	}
	// Still within window → live.
	p.now = func() time.Time { return base.Add(8 * time.Second) }
	if out := p.Probe(ctx, d); *out.GenerationTPS != 41.2 {
		t.Fatalf("within-window gen=%v", *out.GenerationTPS)
	}
	// After the 6s window passes → expires to 0.
	p.now = func() time.Time { return base.Add(20 * time.Second) }
	if out := p.Probe(ctx, d); *out.GenerationTPS != 0 {
		t.Fatalf("expired gen=%v want 0", *out.GenerationTPS)
	}
}

func TestSglangMetricsCounters(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	var gen, prompt atomic.Int64
	gen.Store(100)
	prompt.Store(50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			fmt.Fprintf(w, "sglang:generation_tokens_total %d\nsglang:prompt_tokens_total %d\nsglang:num_running_reqs 3\n", gen.Load(), prompt.Load())
		} else {
			fmt.Fprint(w, `{}`)
		}
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	p.now = func() time.Time { return base }
	d := dep("d1", portOf(srv.URL))
	p.Probe(context.Background(), d)
	gen.Store(150)
	prompt.Store(70)
	p.now = func() time.Time { return base.Add(5 * time.Second) }
	out := p.Probe(context.Background(), d)
	if *out.GenerationTPS != 10 || *out.PrefillTPS != 4 {
		t.Fatalf("sglang gen/prefill=%v/%v want 10/4", *out.GenerationTPS, *out.PrefillTPS)
	}
	if out.RequestsRunning != 3 {
		t.Fatalf("running=%d want 3", out.RequestsRunning)
	}
}

func TestLlamaCPPSlotCounters(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	slotsA := `[{"id":0,"n_decoded":100,"n_prompt_tokens_processed":50,"is_processing":true},
	            {"id":1,"n_decoded":40,"n_prompt_tokens_processed":20,"is_processing":false}]`
	slotsB := `[{"id":0,"n_decoded":120,"n_prompt_tokens_processed":70,"is_processing":true},
	            {"id":1,"n_decoded":40,"n_prompt_tokens_processed":20,"is_processing":false}]`
	current := &slotsA
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slots":
			fmt.Fprint(w, *current)
		case "/props":
			fmt.Fprint(w, `{"model_alias":"qwen-7b","total_context_length":32768}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	p.now = func() time.Time { return base }
	d := dep("d1", portOf(srv.URL))
	p.Probe(context.Background(), d)
	current = &slotsB
	p.now = func() time.Time { return base.Add(5 * time.Second) }
	out := p.Probe(context.Background(), d)
	// generation delta 20 over 5s = 4; prefill delta 20 over 5s = 4.
	if *out.GenerationTPS != 4 {
		t.Fatalf("llama gen=%v want 4", *out.GenerationTPS)
	}
	if *out.PrefillTPS != 4 {
		t.Fatalf("llama prefill=%v want 4", *out.PrefillTPS)
	}
	if out.SlotsActive != 1 || out.SlotsTotal != 2 {
		t.Fatalf("slots=%d/%d", out.SlotsActive, out.SlotsTotal)
	}
	if out.ModelID != "qwen-7b" || out.ContextLength != 32768 {
		t.Fatalf("model=%q ctx=%d", out.ModelID, out.ContextLength)
	}
}

func TestUnauthorizedPersistsMonitorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	out := p.Probe(context.Background(), dep("d1", portOf(srv.URL)))
	if out.Available {
		t.Fatal("expected unavailable")
	}
	if out.ErrorCode == nil || *out.ErrorCode != ErrUnauthorized {
		t.Fatalf("error code=%v want unauthorized", out.ErrorCode)
	}
}

func TestOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(make([]byte, maxBodyBytes+100))
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	out := p.Probe(context.Background(), dep("d1", portOf(srv.URL)))
	if out.ErrorCode == nil || *out.ErrorCode != ErrResponseTooLarge {
		t.Fatalf("error code=%v want response_too_large", out.ErrorCode)
	}
}

func TestThreeFailuresRedetect(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	var mode atomic.Int32 // 0=fail, 1=serve vllm
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if mode.Load() == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, "vllm:generation_tokens_total 100\n")
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	p.now = func() time.Time { return base }
	ctx := context.Background()
	d := dep("d1", portOf(srv.URL))
	// Three failures clear detection and counters.
	for range 3 {
		p.Probe(ctx, d)
	}
	// Now the endpoint serves metric-compatible output; detection re-runs.
	mode.Store(1)
	out := p.Probe(ctx, d)
	if out.Backend != "vllm" {
		t.Fatalf("backend=%q want vllm after re-detect", out.Backend)
	}
}
