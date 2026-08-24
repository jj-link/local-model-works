package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/jj-link/local-model-works/internal/db"
)

// IngestServing stores a five-second serving sample and recomputes its minute
// aggregate transactionally, mirroring IngestNode's bucketing and replay
// safety. Plain (non-legacy) payloads are always written; there is no legacy
// serving schema to normalize.
func (s *Service) IngestServing(ctx context.Context, deploymentID string, at int64, payload ServingPayload) error {
	ts := at
	ts -= ts % 5
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	qtx := s.q.WithTx(tx)
	if err := qtx.InsertServingTelemetry5s(ctx, db.InsertServingTelemetry5sParams{DeploymentID: deploymentID, Ts: ts, Payload: string(data)}); err != nil {
		tx.Rollback()
		return err
	}
	minute := ts - ts%60
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM serving_telemetry_5s WHERE deployment_id = ? AND ts >= ? AND ts < ? ORDER BY ts", deploymentID, minute, minute+60)
	if err != nil {
		tx.Rollback()
		return err
	}
	var payloads []ServingPayload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		var p ServingPayload
		if json.Unmarshal([]byte(raw), &p) == nil {
			payloads = append(payloads, p)
		}
	}
	rows.Close()
	aggregate := aggregateServingMinute(payloads)
	aggr, err := json.Marshal(aggregate)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := qtx.InsertServingTelemetry1m(ctx, db.InsertServingTelemetry1mParams{DeploymentID: deploymentID, Ts: minute, Payload: string(aggr)}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// aggregateServingMinute averages rate/load/cache/latency gauges and retains
// the latest backend, model identity, status, and cumulative counters.
func aggregateServingMinute(payloads []ServingPayload) ServingPayload {
	var out ServingPayload
	var genSum, prefillSum float64
	var genCnt, prefillCnt int
	var runSum, waitSum int64
	var runCnt, waitCnt int
	var slotsActiveSum, slotsTotalSum int64
	var slotsCnt int
	var kvSum float64
	var kvCnt int
	var prefixSum float64
	var prefixCnt int
	var ttftSum, e2eSum, itlSum float64
	var ttftCnt, e2eCnt, itlCnt int
	var specSum float64
	var specCnt int

	for _, p := range payloads {
		if p.GenerationTPS != nil {
			genSum += *p.GenerationTPS
			genCnt++
		}
		if p.PrefillTPS != nil {
			prefillSum += *p.PrefillTPS
			prefillCnt++
		}
		if p.RequestsRunning > 0 || p.RequestsWaiting > 0 {
			runSum += int64(p.RequestsRunning)
			waitSum += int64(p.RequestsWaiting)
		}
		if p.SlotsActive > 0 || p.SlotsTotal > 0 {
			slotsActiveSum += int64(p.SlotsActive)
			slotsTotalSum += int64(p.SlotsTotal)
			slotsCnt++
		}
		if p.KVCacheUsageRatio != nil {
			kvSum += *p.KVCacheUsageRatio
			kvCnt++
		}
		if p.PrefixCacheHitRatio != nil {
			prefixSum += *p.PrefixCacheHitRatio
			prefixCnt++
		}
		if p.TTFTP95Seconds != nil {
			ttftSum += *p.TTFTP95Seconds
			ttftCnt++
		}
		if p.E2EP95Seconds != nil {
			e2eSum += *p.E2EP95Seconds
			e2eCnt++
		}
		if p.ITLP95Seconds != nil {
			itlSum += *p.ITLP95Seconds
			itlCnt++
		}
		if p.SpecAcceptanceRatio != nil {
			specSum += *p.SpecAcceptanceRatio
			specCnt++
		}
		// Latest-tracking fields.
		out.Available = p.Available
		if p.Backend != "" {
			out.Backend = p.Backend
		}
		if p.ModelID != "" {
			out.ModelID = p.ModelID
		}
		out.RequestsRunning = p.RequestsRunning
		out.RequestsWaiting = p.RequestsWaiting
		out.SlotsActive = p.SlotsActive
		out.SlotsTotal = p.SlotsTotal
		out.PreemptionsTotal = p.PreemptionsTotal
		out.ContextLength = p.ContextLength
	}

	n := int64(len(payloads))
	if genCnt > 0 {
		out.GenerationTPS = &[]float64{genSum / float64(genCnt)}[0]
	}
	if prefillCnt > 0 {
		out.PrefillTPS = &[]float64{prefillSum / float64(prefillCnt)}[0]
	}
	if n > 0 {
		if runCnt == 0 && slotsCnt == 0 {
			// all zeros: leave integer defaults
		} else if runCnt > 0 {
			out.RequestsRunning = int32(runSum / int64(runCnt))
			out.RequestsWaiting = int32(waitSum / int64(waitCnt))
		}
		if slotsCnt > 0 {
			out.SlotsActive = int32(slotsActiveSum / int64(slotsCnt))
			out.SlotsTotal = int32(slotsTotalSum / int64(slotsCnt))
		}
	}
	if kvCnt > 0 {
		out.KVCacheUsageRatio = &[]float64{kvSum / float64(kvCnt)}[0]
	}
	if prefixCnt > 0 {
		out.PrefixCacheHitRatio = &[]float64{prefixSum / float64(prefixCnt)}[0]
	}
	if ttftCnt > 0 {
		out.TTFTP95Seconds = &[]float64{ttftSum / float64(ttftCnt)}[0]
	}
	if e2eCnt > 0 {
		out.E2EP95Seconds = &[]float64{e2eSum / float64(e2eCnt)}[0]
	}
	if itlCnt > 0 {
		out.ITLP95Seconds = &[]float64{itlSum / float64(itlCnt)}[0]
	}
	if specCnt > 0 {
		out.SpecAcceptanceRatio = &[]float64{specSum / float64(specCnt)}[0]
	}
	return out
}

// ServingHistory returns ordered samples for one deployment. An unknown
// deployment returns an empty body.
func (s *Service) ServingHistory(ctx context.Context, deploymentID, resolution string, from, to int64, limit int) ([]ServingSample, error) {
	table := "serving_telemetry_5s"
	if resolution == "1m" {
		table = "serving_telemetry_1m"
	} else if resolution != "5s" {
		return nil, fmt.Errorf("resolution must be 5s or 1m")
	}
	if limit < 1 || limit > 10000 {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx, "SELECT ts, payload FROM "+table+" WHERE deployment_id = ? AND ts >= ? AND ts <= ? ORDER BY ts LIMIT ?", deploymentID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ServingSample, 0)
	for rows.Next() {
		var ts int64
		var raw string
		if err := rows.Scan(&ts, &raw); err != nil {
			return nil, err
		}
		var p ServingPayload
		if json.Unmarshal([]byte(raw), &p) != nil {
			continue
		}
		out = append(out, ServingSample{DeploymentID: deploymentID, TS: ts, Payload: p})
	}
	return out, rows.Err()
}

// LatestServing returns the newest full sample per deployment in one query.
func (s *Service) LatestServing(ctx context.Context) (map[string]ServingSample, error) {
	rows, err := s.q.LatestServingTelemetryAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ServingSample, len(rows))
	for _, r := range rows {
		var p ServingPayload
		if json.Unmarshal([]byte(r.Payload), &p) != nil {
			continue
		}
		out[r.DeploymentID] = ServingSample{DeploymentID: r.DeploymentID, TS: r.Ts, Payload: p}
	}
	return out, nil
}

// servingPrometheusLines emits numeric serving gauges labeled only by
// deployment_id.
func (s *Service) servingPrometheusLines(ctx context.Context) []string {
	latest, err := s.LatestServing(ctx)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(latest))
	for id := range latest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var lines []string
	for _, id := range ids {
		p := latest[id].Payload
		lines = append(lines, fmtServing("lmw_serving_telemetry_timestamp_seconds", id, float64(latest[id].TS)))
		gauge := func(name string, v *float64) {
			if v != nil {
				lines = append(lines, fmtServing(name, id, *v))
			}
		}
		gauge("lmw_serving_generation_tps", p.GenerationTPS)
		gauge("lmw_serving_prefill_tps", p.PrefillTPS)
		lines = append(lines, fmtServing("lmw_serving_requests_running", id, float64(p.RequestsRunning)))
		lines = append(lines, fmtServing("lmw_serving_requests_waiting", id, float64(p.RequestsWaiting)))
		lines = append(lines, fmtServing("lmw_serving_slots_active", id, float64(p.SlotsActive)))
		lines = append(lines, fmtServing("lmw_serving_slots_total", id, float64(p.SlotsTotal)))
		gauge("lmw_serving_kv_cache_usage_ratio", p.KVCacheUsageRatio)
		gauge("lmw_serving_prefix_cache_hit_ratio", p.PrefixCacheHitRatio)
		gauge("lmw_serving_ttft_p95_seconds", p.TTFTP95Seconds)
		gauge("lmw_serving_e2e_p95_seconds", p.E2EP95Seconds)
		gauge("lmw_serving_itl_p95_seconds", p.ITLP95Seconds)
		lines = append(lines, fmtServing("lmw_serving_preemptions_total", id, float64(p.PreemptionsTotal)))
		gauge("lmw_serving_spec_acceptance_ratio", p.SpecAcceptanceRatio)
		lines = append(lines, fmtServing("lmw_serving_context_length", id, float64(p.ContextLength)))
	}
	return lines
}

func fmtServing(name, id string, v float64) string {
	return fmt.Sprintf("%s{deployment_id=%q} %v", name, id, v)
}
