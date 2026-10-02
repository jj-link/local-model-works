package servingtelemetry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	if out.PrefixCacheHitRatio != nil || out.TTFTP95Seconds != nil || out.E2EP95Seconds != nil || out.ITLP95Seconds != nil || out.RequestsWaiting != 0 || out.PreemptionsTotal != 0 {
		t.Fatalf("unsupported metrics fabricated: %+v", out)
	}
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
