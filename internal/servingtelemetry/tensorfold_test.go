package servingtelemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tensorFoldFixture = `{"ok":true,"backend":"tensorfold","busy":false,"requests_running":2,"requests_total":7,"prompt_tokens_total":87859,"completion_tokens_total":36499,"prefill_seconds_total":48.1576,"decode_seconds_total":89.3912,"cached_tokens_total":39296,"drafted_total":2102,"accepted_total":1533,"streams":{"decoding":2,"prefilling":0,"max":4,"filling":0,"paused":0},"pool_tokens":2476032,"pool_free_tokens":2381824,"context_length":1048576}`

func tensorFoldServer(t *testing.T, body *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, body.Load().(string))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func tensorFoldCounterBody(generation, prompt int) string {
	return fmt.Sprintf(`{"ok":true,"backend":"tensorfold","requests_running":0,"completion_tokens_total":%d,"prompt_tokens_total":%d}`, generation, prompt)
}

func assertTensorFoldRate(t *testing.T, name string, got, want *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("%s=%v want unavailable", name, *got)
		}
		return
	}
	if got == nil || abs(*got-*want) > 1e-9 {
		t.Fatalf("%s=%v want %v", name, got, *want)
	}
}

func TestTensorFoldHealthMetrics(t *testing.T) {
	var body atomic.Value
	body.Store(tensorFoldFixture)
	srv := tensorFoldServer(t, &body)
	p := NewProber(srv.Client())
	out := p.Probe(context.Background(), dep("tf", portOf(srv.URL)))
	if !out.Available || out.Backend != "tensorfold" || out.ErrorCode != nil {
		t.Fatalf("health unavailable: %+v", out)
	}
	if out.RequestsRunning != 2 || out.SlotsActive != 2 || out.SlotsTotal != 4 || out.ContextLength != 1048576 {
		t.Fatalf("load/context incorrect: %+v", out)
	}
	kv := float64(2476032-2381824) / 2476032
	acceptance := float64(1533) / 2102
	assertTensorFoldRate(t, "KV fraction", out.KVCacheUsageRatio, &kv)
	assertTensorFoldRate(t, "draft acceptance", out.SpecAcceptanceRatio, &acceptance)
	assertTensorFoldRate(t, "first generation", out.GenerationTPS, nil)
	assertTensorFoldRate(t, "first prefill", out.PrefillTPS, nil)
	if out.PrefixCacheHitRatio != nil || out.TTFTP95Seconds != nil || out.E2EP95Seconds != nil || out.ITLP95Seconds != nil || out.RequestsWaiting != nil || out.PreemptionsTotal != 0 {
		t.Fatalf("unsupported metrics fabricated: %+v", out)
	}
}

func TestTensorFoldNativeStreamKV(t *testing.T) {
	const health = `{"ok":true,"backend":"tensorfold","requests_running":3,"streams":{"decoding":3,"prefilling":0,"max":5},"context_length":262144}`
	for _, tc := range []struct {
		name, metrics string
		want, total   *float64
	}{
		// Each of the 5 lanes owns one window: 2.15 windows used of 5.
		{"busiest stream and lane total", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} 0.75\ntensorfold:kv_cache_usage_ratio{pool=\"1\"} 0.8\ntensorfold:kv_cache_usage_ratio{pool=\"2\"} 0.6\n", new(0.8), new(2.15 / 5)},
		{"idle gauge preserves zero", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} 0\n", new(0.0), new(0.0)},
		{"unsupported is unavailable", "tensorfold:requests_running 3\n", nil, nil},
		{"invalid member rejects partial peak", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} 0.75\ntensorfold:kv_cache_usage_ratio{pool=\"1\"} 1.1\n", nil, nil},
		{"negative is unavailable", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} -0.1\n", nil, nil},
		{"nan is unavailable", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} NaN\n", nil, nil},
		{"overflow is unavailable", "tensorfold:kv_cache_usage_ratio{pool=\"0\"} 1e999\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					fmt.Fprint(w, health)
				case "/metrics":
					fmt.Fprint(w, tc.metrics)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
			if !out.Available {
				t.Fatalf("unavailable: %+v", out)
			}
			assertTensorFoldRate(t, "busiest stream KV", out.KVCacheMaxStreamUsageRatio, tc.want)
			assertTensorFoldRate(t, "lane KV total", out.KVCacheUsageRatio, tc.total)
		})
	}
}

func TestTensorFoldKVPoolPrecedenceAndMetricsFailure(t *testing.T) {
	var health, metrics atomic.Value
	health.Store(tensorFoldFixture)
	metrics.Store("tensorfold:kv_cache_usage_ratio{pool=\"0\"} 0.8\n")
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, health.Load().(string))
		case "/metrics":
			w.WriteHeader(int(status.Load()))
			fmt.Fprint(w, metrics.Load().(string))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := NewProber(srv.Client())
	d := dep("tf", portOf(srv.URL))
	out := p.Probe(context.Background(), d)
	pool := float64(2476032-2381824) / 2476032
	assertTensorFoldRate(t, "shared pool", out.KVCacheUsageRatio, &pool)
	assertTensorFoldRate(t, "pool takes precedence over streams", out.KVCacheMaxStreamUsageRatio, nil)
	health.Store(tensorFoldCounterBody(10, 20))
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "native streams", out.KVCacheMaxStreamUsageRatio, new(0.8))
	status.Store(http.StatusServiceUnavailable)
	out = p.Probe(context.Background(), d)
	if !out.Available {
		t.Fatalf("optional metrics failure discarded healthy counters: %+v", out)
	}
	assertTensorFoldRate(t, "unavailable gauge must not retain old value", out.KVCacheMaxStreamUsageRatio, nil)
}

func TestTensorFoldIntervalRates(t *testing.T) {
	var body atomic.Value
	body.Store(tensorFoldCounterBody(0, 0))
	srv := tensorFoldServer(t, &body)
	p := NewProber(srv.Client())
	base := time.Unix(1_800_000_000, 0)
	var now time.Time
	p.now = func() time.Time { return now }
	zero, two, four, ten := 0.0, 2.0, 4.0, 10.0
	steps := []struct {
		name       string
		after      time.Duration
		generation int
		prompt     int
		genRate    *float64
		promptRate *float64
	}{
		{"first zero baseline", 0, 0, 0, nil, nil},
		{"advance from zero", 5 * time.Second, 50, 20, &ten, &four},
		{"unchanged counters", 10 * time.Second, 50, 20, &zero, &zero},
		{"both counters reset", 15 * time.Second, 0, 0, nil, nil},
		{"reset baseline recovers", 20 * time.Second, 10, 20, &two, &four},
		{"generation reset independent", 25 * time.Second, 0, 40, nil, &four},
		{"stale boundary", 40 * time.Second, 200, 500, nil, nil},
		{"stale baseline recovers", 45 * time.Second, 210, 520, &two, &four},
		{"long stale interval", 70 * time.Second, 1000, 1000, nil, nil},
		{"same clock unavailable", 70 * time.Second, 1000, 1000, nil, nil},
		{"backward clock unavailable", 65 * time.Second, 1000, 1000, nil, nil},
		{"clock baseline recovers", 70 * time.Second, 1010, 1020, &two, &four},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			body.Store(tensorFoldCounterBody(step.generation, step.prompt))
			now = base.Add(step.after)
			out := p.Probe(context.Background(), dep("tf", portOf(srv.URL)))
			if !out.Available || out.Backend != "tensorfold" {
				t.Fatalf("unavailable: %+v", out)
			}
			assertTensorFoldRate(t, "generation", out.GenerationTPS, step.genRate)
			assertTensorFoldRate(t, "prefill accounting", out.PrefillTPS, step.promptRate)
		})
	}
}

func TestTensorFoldMissingCountersReseed(t *testing.T) {
	var body atomic.Value
	body.Store(tensorFoldCounterBody(0, 0))
	srv := tensorFoldServer(t, &body)
	p := NewProber(srv.Client())
	base := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return base }
	d := dep("tf", portOf(srv.URL))
	p.Probe(context.Background(), d)
	body.Store(`{"ok":true,"backend":"tensorfold","requests_running":0,"completion_tokens_total":10}`)
	p.now = func() time.Time { return base.Add(5 * time.Second) }
	out := p.Probe(context.Background(), d)
	two := 2.0
	assertTensorFoldRate(t, "present generation", out.GenerationTPS, &two)
	assertTensorFoldRate(t, "missing prompt", out.PrefillTPS, nil)
	body.Store(tensorFoldCounterBody(20, 100))
	p.now = func() time.Time { return base.Add(10 * time.Second) }
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "generation", out.GenerationTPS, &two)
	assertTensorFoldRate(t, "returned prompt needs baseline", out.PrefillTPS, nil)
	body.Store(tensorFoldCounterBody(30, 110))
	p.now = func() time.Time { return base.Add(15 * time.Second) }
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "prompt recovered", out.PrefillTPS, &two)
}

func TestTensorFoldPoolAndStreamBoundaries(t *testing.T) {
	zero := 0.0
	cases := []struct {
		name       string
		fields     string
		active     int32
		kv         *float64
		acceptance *float64
	}{
		{"prefilling fallback", `"streams":{"decoding":1,"prefilling":2,"paused":1,"max":4}`, 4, nil, nil},
		{"filling alias not double counted", `"streams":{"decoding":1,"prefilling":1,"filling":1,"paused":1,"max":4}`, 3, nil, nil},
		{"zero filling overrides alias", `"streams":{"decoding":1,"prefilling":2,"filling":0,"max":4}`, 1, nil, nil},
		{"empty pool and zero acceptance", `"pool_tokens":100,"pool_free_tokens":100,"drafted_total":10,"accepted_total":0`, 0, &zero, &zero},
		{"zero denominators", `"pool_tokens":0,"pool_free_tokens":0,"drafted_total":0,"accepted_total":0`, 0, nil, nil},
		{"inconsistent ratios unavailable", `"pool_tokens":100,"pool_free_tokens":101,"drafted_total":10,"accepted_total":11`, 0, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(`{"ok":true,"backend":"tensorfold","requests_running":0,` + tc.fields + `}`)
			srv := tensorFoldServer(t, &body)
			out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
			if !out.Available || out.SlotsActive != tc.active {
				t.Fatalf("load incorrect: %+v", out)
			}
			assertTensorFoldRate(t, "KV", out.KVCacheUsageRatio, tc.kv)
			assertTensorFoldRate(t, "acceptance", out.SpecAcceptanceRatio, tc.acceptance)
		})
	}
}

func TestTensorFoldInvalidAndUnsupportedHealth(t *testing.T) {
	cases := []struct{ name, body, code string }{
		{"other backend", `{"ok":true,"backend":"other","requests_running":0}`, ErrUnsupported},
		{"generic health", `{"ok":true}`, ErrUnsupported},
		{"malformed unclassified JSON", `{"backend":`, ErrUnsupported},
		{"not healthy", `{"ok":false,"backend":"tensorfold","requests_running":0}`, ErrInvalidResponse},
		{"missing running", `{"ok":true,"backend":"tensorfold"}`, ErrInvalidResponse},
		{"negative running", `{"ok":true,"backend":"tensorfold","requests_running":-1}`, ErrInvalidResponse},
		{"invalid counter type", `{"ok":true,"backend":"tensorfold","requests_running":0,"completion_tokens_total":"12"}`, ErrInvalidResponse},
		{"negative counter", `{"ok":true,"backend":"tensorfold","requests_running":0,"prompt_tokens_total":-1}`, ErrInvalidResponse},
		{"counter overflow", `{"ok":true,"backend":"tensorfold","requests_running":0,"completion_tokens_total":1e999}`, ErrInvalidResponse},
		{"slot overflow", `{"ok":true,"backend":"tensorfold","requests_running":0,"streams":{"decoding":2147483647,"paused":1,"max":4}}`, ErrInvalidResponse},
		{"oversized health", strings.Repeat("x", maxBodyBytes+1), ErrResponseTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body atomic.Value
			body.Store(tc.body)
			srv := tensorFoldServer(t, &body)
			out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
			if out.Available || out.ErrorCode == nil || *out.ErrorCode != tc.code {
				t.Fatalf("status=%+v want %s", out, tc.code)
			}
		})
	}
}

func TestTensorFoldDetectionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, metrics, info, slots string
		healthStatus               int
		want                       string
	}{
		{"TensorFold before generic JSON error", "", `{"detail":"Not Found"}`, "", http.StatusOK, "tensorfold"},
		{"vLLM metrics retain precedence", "vllm:generation_tokens_total 10", "", "", http.StatusOK, "vllm"},
		{"SGLang metrics retain precedence", "sglang:generation_tokens_total 10", "", "", http.StatusOK, "sglang"},
		{"legacy SGLang tolerates missing health", "", `{"total_output_tokens":10}`, "", http.StatusNotFound, "sglang"},
		{"legacy llama tolerates unauthorized health", "", "", `[{"id":0,"n_decoded":10,"is_processing":true}]`, http.StatusUnauthorized, "llamacpp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/metrics":
					fmt.Fprint(w, tc.metrics)
				case "/health":
					w.WriteHeader(tc.healthStatus)
					if tc.healthStatus == http.StatusOK {
						fmt.Fprint(w, tensorFoldFixture)
					}
				case "/get_server_info":
					fmt.Fprint(w, tc.info)
				case "/slots":
					fmt.Fprint(w, tc.slots)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
			if !out.Available || out.Backend != tc.want {
				t.Fatalf("classification=%+v want %s", out, tc.want)
			}
		})
	}
}

func TestTensorFoldFailureReseedsRates(t *testing.T) {
	var body atomic.Value
	body.Store(tensorFoldCounterBody(0, 0))
	srv := tensorFoldServer(t, &body)
	p := NewProber(srv.Client())
	base := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return base }
	d := dep("tf", portOf(srv.URL))
	p.Probe(context.Background(), d)
	body.Store(`{"ok":false,"backend":"tensorfold","requests_running":0}`)
	for i := 1; i <= 3; i++ {
		p.now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		out := p.Probe(context.Background(), d)
		if out.Available || out.ErrorCode == nil || *out.ErrorCode != ErrInvalidResponse {
			t.Fatalf("invalid health status: %+v", out)
		}
	}
	body.Store(tensorFoldCounterBody(50, 20))
	p.now = func() time.Time { return base.Add(5 * time.Second) }
	out := p.Probe(context.Background(), d)
	if !out.Available {
		t.Fatalf("recovery unavailable: %+v", out)
	}
	assertTensorFoldRate(t, "generation after failures", out.GenerationTPS, nil)
	assertTensorFoldRate(t, "prompt after failures", out.PrefillTPS, nil)
	body.Store(tensorFoldCounterBody(100, 40))
	p.now = func() time.Time { return base.Add(10 * time.Second) }
	out = p.Probe(context.Background(), d)
	ten, four := 10.0, 4.0
	assertTensorFoldRate(t, "generation recovery", out.GenerationTPS, &ten)
	assertTensorFoldRate(t, "prompt recovery", out.PrefillTPS, &four)
	// Successful observations break the consecutive-failure streak.
	body.Store(`{"ok":false,"backend":"tensorfold","requests_running":0}`)
	p.Probe(context.Background(), d)
	body.Store(tensorFoldCounterBody(150, 60))
	p.now = func() time.Time { return base.Add(15 * time.Second) }
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "single failure retains fresh baseline", out.GenerationTPS, &ten)
}

// Captured from tf-dsv41-lanes serving DeepSeek-V4.1-Flash-1017.
func tensorFoldLanesServer(t *testing.T, health string) *httptest.Server {
	t.Helper()
	metrics, err := os.ReadFile("testdata/tensorfold-lanes-metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			w.Write(metrics)
		case "/health":
			fmt.Fprint(w, health)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTensorFoldLanesMetrics(t *testing.T) {
	health, err := os.ReadFile("testdata/tensorfold-lanes-health.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := tensorFoldLanesServer(t, string(health))
	out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
	if !out.Available || out.Backend != "tensorfold" || out.ErrorCode != nil {
		t.Fatalf("lane server unavailable: %+v", out)
	}
	if out.RequestsRunning != 3 || out.SlotsActive != 3 || out.SlotsTotal != 4 || out.RequestsWaiting == nil || *out.RequestsWaiting != 0 {
		t.Fatalf("load incorrect: %+v", out)
	}
	decode, prefill := 72.0, 1468.3
	prefix, acceptance, kv := 702464.0/848190, 17052.0/25582, 0.050498
	assertTensorFoldRate(t, "generation", out.GenerationTPS, &decode)
	assertTensorFoldRate(t, "prefill", out.PrefillTPS, &prefill)
	assertTensorFoldRate(t, "prefix cache", out.PrefixCacheHitRatio, &prefix)
	assertTensorFoldRate(t, "draft acceptance", out.SpecAcceptanceRatio, &acceptance)
	assertTensorFoldRate(t, "busiest stream KV", out.KVCacheMaxStreamUsageRatio, &kv)
	// The lanes share one pool of the context window's positions.
	total := 0.039606 + 0.010029 + 0.050498
	assertTensorFoldRate(t, "all-lane KV", out.KVCacheUsageRatio, &total)
	if out.TTFTP95Seconds == nil || out.E2EP95Seconds == nil || out.ITLP95Seconds == nil {
		t.Fatalf("latency/KV fields incorrect: %+v", out)
	}
}

// With a pool four context windows long, each lane's window ratio is a
// quarter as much of the pool.
func TestTensorFoldLanesPoolLargerThanContext(t *testing.T) {
	health, err := os.ReadFile("testdata/tensorfold-lanes-health.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := tensorFoldLanesServer(t, string(health))
	target := dep("tf", portOf(srv.URL))
	target.Parameters = map[string]any{"context": float64(262144), "pool": float64(1048576)}
	out := NewProber(srv.Client()).Probe(context.Background(), target)
	total := (0.039606 + 0.010029 + 0.050498) / 4
	assertTensorFoldRate(t, "pool KV", out.KVCacheUsageRatio, &total)
}

func TestTensorFoldLanesHealthOptional(t *testing.T) {
	srv := tensorFoldLanesServer(t, `{"detail":"Not Found"}`)
	out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
	if !out.Available || out.RequestsRunning != 3 || out.SlotsTotal != 0 || out.GenerationTPS != nil || out.PrefillTPS != nil || out.KVCacheUsageRatio != nil {
		t.Fatalf("health-less lane server: %+v", out)
	}
}

func TestTensorFoldHealthIdentityBeatsLaneMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			fmt.Fprint(w, "tensorfold:num_requests_running 1\n")
		case "/health":
			fmt.Fprint(w, tensorFoldFixture)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
	if !out.Available || out.RequestsRunning != 2 || out.SlotsTotal != 4 {
		t.Fatalf("health-identified server read as lanes: %+v", out)
	}
}

func TestTensorFoldLatencyPercentiles(t *testing.T) {
	const health = `{"ok":true,"backend":"tensorfold","requests_running":0,"context_length":262144}`
	const metrics = "tensorfold:time_to_first_token_seconds_bucket{le=\"1\"} 90\ntensorfold:time_to_first_token_seconds_bucket{le=\"5\"} 100\ntensorfold:time_to_first_token_seconds_bucket{le=\"+Inf\"} 100\n" +
		"tensorfold:request_latency_seconds_bucket{le=\"10\"} 50\ntensorfold:request_latency_seconds_bucket{le=\"60\"} 100\ntensorfold:request_latency_seconds_bucket{le=\"+Inf\"} 100\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, health)
		case "/metrics":
			fmt.Fprint(w, metrics)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out := NewProber(srv.Client()).Probe(context.Background(), dep("tf", portOf(srv.URL)))
	if out.TTFTP95Seconds == nil || out.E2EP95Seconds == nil || out.ITLP95Seconds != nil {
		t.Fatalf("latency percentiles: %+v", out)
	}
}
