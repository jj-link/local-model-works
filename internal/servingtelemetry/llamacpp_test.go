package servingtelemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Sanitized native b11539 response fields. The prompt length includes generated
// tokens; it must not be substituted for processed prefill or KV occupancy.
const llamaNativeSlots = `[{"id":0,"id_task":21671,"n_ctx":262144,"is_processing":true,"n_prompt_tokens":72605,"n_prompt_tokens_processed":14201,"n_prompt_tokens_cache":55951,"next_token":[{"n_decoded":2448}]}]`
const llamaNativeProps = `{"model_alias":"Qwen3.8-Flash-Next","total_slots":1,"default_generation_settings":{"n_ctx":262144}}`
const llamaNativeMetrics = `llamacpp:prompt_tokens_total 68201
llamacpp:prompt_tokens_cached_total 543259
llamacpp:tokens_predicted_total 57049
llamacpp:n_tokens_max 87000
llamacpp:spec_decode_num_draft_tokens_total 90540
llamacpp:spec_decode_num_accepted_tokens_total 38931
llamacpp:prompt_tokens_seconds 0
llamacpp:predicted_tokens_seconds 0
llamacpp:requests_processing 1
llamacpp:requests_deferred 0
`

func llamaServer(t *testing.T, slots, metrics *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			if *metrics == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, *metrics)
		case "/slots":
			fmt.Fprint(w, *slots)
		case "/props":
			fmt.Fprint(w, llamaNativeProps)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"message":"File Not Found","type":"not_found_error","code":404}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLlamaCPPNativeResponse(t *testing.T) {
	slots, metrics := llamaNativeSlots, llamaNativeMetrics
	srv := llamaServer(t, &slots, &metrics)
	out := NewProber(srv.Client()).Probe(context.Background(), dep("native", portOf(srv.URL)))
	if !out.Available || out.Backend != "llamacpp" || out.ModelID != "Qwen3.8-Flash-Next" || out.ContextLength != 262144 {
		t.Fatalf("native identity/context: %+v", out)
	}
	if out.SlotsTotal != 1 || out.SlotsActive != 1 || out.RequestsRunning != 1 || out.RequestsWaiting == nil || *out.RequestsWaiting != 0 {
		t.Fatalf("native load: %+v", out)
	}
	acceptance, prefix := float64(38931)/90540, float64(543259)/(543259+68201)
	assertTensorFoldRate(t, "draft acceptance", out.SpecAcceptanceRatio, &acceptance)
	assertTensorFoldRate(t, "prefix reuse", out.PrefixCacheHitRatio, &prefix)
	if out.GenerationTPS != nil || out.PrefillTPS != nil || out.KVCacheUsageRatio != nil || out.TTFTP95Seconds != nil || out.E2EP95Seconds != nil || out.ITLP95Seconds != nil {
		t.Fatalf("unavailable measurements fabricated: %+v", out)
	}
}

func TestDetectionRejectsJSONErrorsAndPreservesFallbacks(t *testing.T) {
	cases := []struct {
		name       string
		infoStatus int
		info       string
		slots      string
		backend    string
	}{
		{"native JSON 404 falls through to slots", 404, `{"error":{"code":404}}`, llamaNativeSlots, "llamacpp"},
		{"successful error document is not SGLang", 200, `{"error":{"code":404}}`, "", "unknown"},
		{"arbitrary JSON is not SGLang", 200, `{"ok":true}`, "", "unknown"},
		{"HTTP error with positive-looking fields rejected", 500, `{"total_output_tokens":10}`, "", "unknown"},
		{"legacy SGLang without metrics", 200, `{"last_gen_throughput":42,"max_running_requests":4}`, "", "sglang"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/get_server_info":
					w.WriteHeader(tc.infoStatus)
					fmt.Fprint(w, tc.info)
				case "/slots":
					if tc.slots != "" {
						fmt.Fprint(w, tc.slots)
						return
					}
					http.NotFound(w, r)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			out := NewProber(srv.Client()).Probe(context.Background(), dep("fallback", portOf(srv.URL)))
			if out.Backend != tc.backend || out.Available != (tc.backend != "unknown") {
				t.Fatalf("classification=%+v want %s", out, tc.backend)
			}
		})
	}
}

func llamaSlot(task, decoded, processed int, active bool) string {
	return fmt.Sprintf(`[{"id":0,"id_task":%d,"is_processing":%t,"n_prompt_tokens":999999,"n_prompt_tokens_processed":%d,"next_token":[{"n_decoded":%d}]}]`, task, active, processed, decoded)
}

func llamaMetrics(completed, prompt, running, waiting int) string {
	return fmt.Sprintf("llamacpp:tokens_predicted_total %d\nllamacpp:prompt_tokens_total %d\nllamacpp:requests_processing %d\nllamacpp:requests_deferred %d\n", completed, prompt, running, waiting)
}

func TestLlamaCPPContinuousRatesAcrossRequestBoundaries(t *testing.T) {
	slots, metrics := "", ""
	srv := llamaServer(t, &slots, &metrics)
	p := NewProber(srv.Client())
	base := time.Unix(1_800_000_000, 0)
	var now time.Time
	p.now = func() time.Time { return now }
	zero, two, four, six, ten := 0.0, 2.0, 4.0, 6.0, 10.0
	steps := []struct {
		name                          string
		seconds, task, gen, processed int
		completed, prompt             int
		active                        bool
		genRate, promptRate           *float64
	}{
		{"zero baseline", 0, 1, 0, 0, 0, 0, true, nil, nil},
		{"live prefill from zero", 5, 1, 0, 50, 0, 50, true, &zero, &ten},
		{"live decode counters not committed", 10, 1, 50, 50, 0, 50, true, &ten, &zero},
		{"completion charges only remaining tokens", 15, 1, 0, 0, 60, 50, false, &two, &zero},
		{"idle completion not charged twice", 20, 1, 0, 0, 60, 50, false, &zero, &zero},
		{"new task includes new live tokens", 25, 2, 20, 30, 60, 80, true, &four, &six},
		{"task replacement combines old tail and new task", 30, 3, 10, 20, 100, 100, true, &six, &four},
		{"counter reset invalidates interval", 35, 1, 0, 0, 0, 0, true, nil, nil},
		{"reset recovers", 40, 1, 10, 20, 0, 20, true, &two, &four},
		{"stale boundary", 55, 1, 100, 100, 0, 100, true, nil, nil},
		{"stale baseline recovers", 60, 1, 110, 120, 0, 120, true, &two, &four},
		{"same clock invalidates interval", 60, 1, 120, 120, 0, 120, true, nil, nil},
		{"same task decoded regression", 65, 1, 5, 120, 0, 120, true, nil, &zero},
		{"regression baseline recovers", 70, 1, 15, 140, 0, 140, true, &two, &four},
		{"backward clock", 65, 1, 20, 140, 0, 140, true, nil, nil},
		{"clock recovers", 70, 1, 30, 160, 0, 160, true, &two, &four},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			slots = llamaSlot(step.task, step.gen, step.processed, step.active)
			running := 0
			if step.active {
				running = 1
			}
			metrics = llamaMetrics(step.completed, step.prompt, running, 2)
			now = base.Add(time.Duration(step.seconds) * time.Second)
			out := p.Probe(context.Background(), dep("rates", portOf(srv.URL)))
			if !out.Available || out.RequestsRunning != int32(running) || out.RequestsWaiting == nil || *out.RequestsWaiting != 2 {
				t.Fatalf("load incorrect: %+v", out)
			}
			assertTensorFoldRate(t, "generation", out.GenerationTPS, step.genRate)
			assertTensorFoldRate(t, "prefill", out.PrefillTPS, step.promptRate)
		})
	}
}

func TestLlamaCPPSlotsOnlyTaskReplacementAndMissingPrefill(t *testing.T) {
	slots, metrics := llamaSlot(1, 100, 50, true), ""
	srv := llamaServer(t, &slots, &metrics)
	p := NewProber(srv.Client())
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	d := dep("slots-only", portOf(srv.URL))
	p.Probe(context.Background(), d)
	now = now.Add(5 * time.Second)
	// Larger replacement counters are not a delta from the preceding task.
	slots = llamaSlot(2, 200, 150, true)
	out := p.Probe(context.Background(), d)
	if out.GenerationTPS != nil || out.PrefillTPS != nil {
		t.Fatalf("replacement should reseed: %+v", out)
	}
	now = now.Add(5 * time.Second)
	slots = `[{"id":0,"id_task":2,"is_processing":true,"n_prompt_tokens":999999,"next_token":[{"n_decoded":210}]}]`
	out = p.Probe(context.Background(), d)
	two := 2.0
	assertTensorFoldRate(t, "generation", out.GenerationTPS, &two)
	assertTensorFoldRate(t, "missing processed counter", out.PrefillTPS, nil)
}

func TestLlamaCPPIncoherentSnapshotReseeds(t *testing.T) {
	base := time.Unix(1_800_000_000, 0)
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			slots := llamaSlot(1, 100, 50, true)
			metrics := llamaMetrics(1000, 50, 1, 0)
			transition := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/slots":
					fmt.Fprint(w, slots)
				case "/metrics":
					if transition {
						if replacement {
							slots = llamaSlot(2, 5, 20, true)
							metrics = llamaMetrics(1120, 70, 1, 0)
						} else {
							slots = llamaSlot(1, 0, 0, false)
							metrics = llamaMetrics(1120, 50, 0, 0)
						}
						transition = false
					}
					fmt.Fprint(w, metrics)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			p := NewProber(srv.Client())
			now := base
			p.now = func() time.Time { return now }
			d := dep("racing", portOf(srv.URL))
			p.Probe(context.Background(), d)
			transition = true
			for i := 1; i <= 3; i++ {
				now = base.Add(time.Duration(i*5) * time.Second)
				out := p.Probe(context.Background(), d)
				if i < 3 {
					assertTensorFoldRate(t, "incoherent/reseed generation", out.GenerationTPS, nil)
				} else {
					zero := 0.0
					assertTensorFoldRate(t, "stable generation", out.GenerationTPS, &zero)
				}
			}
		})
	}
}

func TestLlamaCPPIndependentCounterAvailability(t *testing.T) {
	slots, metrics := llamaSlot(1, 10, 20, true), llamaMetrics(100, 200, 1, 0)
	srv := llamaServer(t, &slots, &metrics)
	p := NewProber(srv.Client())
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	d := dep("missing", portOf(srv.URL))
	p.Probe(context.Background(), d)
	two, four := 2.0, 4.0

	now = now.Add(5 * time.Second)
	slots = llamaSlot(1, 20, 40, true)
	metrics = "llamacpp:tokens_predicted_total 100\n"
	out := p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "generation with missing prompt total", out.GenerationTPS, &two)
	assertTensorFoldRate(t, "slot prefill fallback", out.PrefillTPS, &four)

	now = now.Add(5 * time.Second)
	slots = llamaSlot(1, 30, 60, true)
	metrics = llamaMetrics(100, 240, 1, 0)
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "generation unaffected by prompt reseed", out.GenerationTPS, &two)
	assertTensorFoldRate(t, "returned prompt total needs baseline", out.PrefillTPS, nil)

	now = now.Add(5 * time.Second)
	slots = llamaSlot(1, 40, 80, true)
	metrics = llamaMetrics(100, 260, 1, 0)
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "recovered prompt total", out.PrefillTPS, &four)

	now = now.Add(5 * time.Second)
	// A server counter reset can hide behind increasing live slot counters.
	slots = llamaSlot(1, 200, 100, true)
	metrics = llamaMetrics(0, 280, 1, 0)
	out = p.Probe(context.Background(), d)
	assertTensorFoldRate(t, "completed counter reset", out.GenerationTPS, nil)
	assertTensorFoldRate(t, "independent prompt total", out.PrefillTPS, &four)
}

func TestLlamaCPPMultipleSlotsDoNotHideReplacement(t *testing.T) {
	slots := `[{"id":0,"id_task":1,"is_processing":true,"n_decoded":100,"n_prompt_tokens_processed":50},{"id":1,"id_task":2,"is_processing":true,"n_decoded":200,"n_prompt_tokens_processed":100}]`
	metrics := llamaMetrics(1000, 150, 2, 0)
	srv := llamaServer(t, &slots, &metrics)
	p := NewProber(srv.Client())
	now := time.Unix(1_800_000_000, 0)
	p.now = func() time.Time { return now }
	d := dep("multiple", portOf(srv.URL))
	p.Probe(context.Background(), d)
	now = now.Add(5 * time.Second)
	slots = `[{"id":0,"id_task":3,"is_processing":true,"n_decoded":5,"n_prompt_tokens_processed":10},{"id":1,"id_task":2,"is_processing":true,"n_decoded":220,"n_prompt_tokens_processed":100}]`
	metrics = llamaMetrics(1125, 160, 2, 1)
	out := p.Probe(context.Background(), d)
	// 25 final tokens from task 1, 5 from its replacement, 20 from task 2.
	ten, two := 10.0, 2.0
	assertTensorFoldRate(t, "multi-slot generation", out.GenerationTPS, &ten)
	assertTensorFoldRate(t, "multi-slot prefill", out.PrefillTPS, &two)
	if out.SlotsActive != 2 || out.RequestsWaiting == nil || *out.RequestsWaiting != 1 {
		t.Fatalf("multi-slot load incorrect: %+v", out)
	}
}

func TestLlamaCPPRatioBoundaries(t *testing.T) {
	zero := 0.0
	for _, tc := range []struct {
		name, counters string
		spec, prefix   *float64
	}{
		{"absent", "", nil, nil},
		{"zero accepted and cached", "llamacpp:spec_decode_num_draft_tokens_total 10\nllamacpp:spec_decode_num_accepted_tokens_total 0\nllamacpp:prompt_tokens_cached_total 0\n", &zero, &zero},
		{"missing accepted numerator", "llamacpp:spec_decode_num_draft_tokens_total 10\n", nil, nil},
		{"invalid acceptance and prefix", "llamacpp:spec_decode_num_draft_tokens_total 10\nllamacpp:spec_decode_num_accepted_tokens_total 11\nllamacpp:prompt_tokens_cached_total -1\n", nil, nil},
		{"zero draft denominator", "llamacpp:spec_decode_num_draft_tokens_total 0\nllamacpp:spec_decode_num_accepted_tokens_total 0\n", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots := llamaSlot(1, 0, 0, false)
			metrics := llamaMetrics(10, 20, 0, 0) + tc.counters
			srv := llamaServer(t, &slots, &metrics)
			out := NewProber(srv.Client()).Probe(context.Background(), dep("ratios", portOf(srv.URL)))
			assertTensorFoldRate(t, "acceptance", out.SpecAcceptanceRatio, tc.spec)
			assertTensorFoldRate(t, "prefix", out.PrefixCacheHitRatio, tc.prefix)
		})
	}
}

func TestLlamaCPPDisabledMetricsAllowsSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			w.WriteHeader(http.StatusNotImplemented)
			fmt.Fprint(w, `{"error":{"code":501,"type":"not_supported_error"}}`)
		case "/slots":
			fmt.Fprint(w, llamaNativeSlots)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	out := NewProber(srv.Client()).Probe(context.Background(), dep("disabled-metrics", portOf(srv.URL)))
	if !out.Available || out.Backend != "llamacpp" || out.SlotsActive != 1 {
		t.Fatalf("disabled metrics prevented slot telemetry: %+v", out)
	}
}

func TestLlamaCPPQueuePresenceSurvivesSerialization(t *testing.T) {
	for _, tc := range []struct {
		name, metric string
		present      bool
		value        float64
	}{
		{"unreported", "", false, 0},
		{"empty", "llamacpp:requests_deferred 0\n", true, 0},
		{"queued", "llamacpp:requests_deferred 2\n", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			slots, metrics := llamaNativeSlots, "llamacpp:tokens_predicted_total 10\n"+tc.metric
			srv := llamaServer(t, &slots, &metrics)
			out := NewProber(srv.Client()).Probe(context.Background(), dep("queue", portOf(srv.URL)))
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(encoded, &payload); err != nil {
				t.Fatal(err)
			}
			value, present := payload["requests_waiting"]
			if present != tc.present || (present && value != tc.value) {
				t.Fatalf("queue presence/value = %v/%v, want %v/%v", present, value, tc.present, tc.value)
			}
		})
	}
}
