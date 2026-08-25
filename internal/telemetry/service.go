// Package telemetry persists node and serving samples at operational (5s) and
// historical (1m) resolutions and enforces bounded retention.
package telemetry

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

const (
	RawRetention      = 24 * time.Hour
	MinuteRetention   = 30 * 24 * time.Hour
	retentionInterval = time.Minute

	// maxHistoryLimit bounds a single ordered history query and matches the
	// OpenAPI maximum and rangePolicy("7d") contract at one-minute resolution
	// (10080 samples/week). Limits outside 1..maxHistoryLimit fall back to 2000.
	maxHistoryLimit = 100000
)

type Service struct {
	db *sql.DB
	q  *db.Queries
}

func New(database *sql.DB, queries *db.Queries) *Service {
	return &Service{db: database, q: queries}
}

// IngestNode stores the five-second sample and recomputes its minute aggregate
// transactionally. Raw timestamps are floored to a real five-second bucket so
// resolution=5s stays bounded even when agents sample faster. Replays and
// out-of-order samples cannot skew the retained minute value.
func (s *Service) IngestNode(ctx context.Context, nodeID string, at time.Time, payload NodePayload) error {
	ts := at.Unix()
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
	if err := qtx.InsertTelemetry5s(ctx, db.InsertTelemetry5sParams{NodeID: nodeID, Ts: ts, Payload: string(data)}); err != nil {
		tx.Rollback()
		return err
	}
	minute := ts - ts%60
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM telemetry_5s WHERE node_id = ? AND ts >= ? AND ts < ? ORDER BY ts", nodeID, minute, minute+60)
	if err != nil {
		tx.Rollback()
		return err
	}
	var payloads []NodePayload
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		var p NodePayload
		if json.Unmarshal([]byte(raw), &p) == nil {
			payloads = append(payloads, p)
		}
	}
	rows.Close()
	aggregate := aggregateNodeMinute(payloads)
	aggr, err := json.Marshal(aggregate)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := qtx.InsertTelemetry1m(ctx, db.InsertTelemetry1mParams{NodeID: nodeID, Ts: minute, Payload: string(aggr)}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// aggregateNodeMinute averages interval gauges (CPU, memory, network rates,
// accelerator gauges keyed by index) and retains the latest cumulative
// counters and uptime. Current-only arrays (filesystems, interfaces, GPU
// processes, throttle reasons) are omitted.
func aggregateNodeMinute(payloads []NodePayload) NodePayload {
	var out NodePayload
	var cpuSum int64
	var cpuCount int
	var memUsedSum int64
	var rxRateSum, txRateSum uint64
	type accAgg struct {
		idx        int
		utilSum    int64
		utilCnt    int
		memUsedSum int64
		tempSum    int64
		tempCnt    int
		powerSum   int64
		powCnt     int
		limitSum   int64
		limitCnt   int
	}
	accMap := map[int]*accAgg{}

	for _, p := range payloads {
		if c := p.CPU; c != nil {
			cpuSum += int64(c.UsagePercent)
			cpuCount++
			out.CPU = &CPUPayload{Cores: c.Cores, Load1: c.Load1}
			out.CPU.UsagePercent = c.UsagePercent // provisional; overwritten below
		}
		if m := p.Memory; m != nil {
			memUsedSum += int64(m.UsedBytes)
			out.Memory = &MemoryPayload{TotalBytes: m.TotalBytes, SwapUsedBytes: m.SwapUsedBytes}
			out.Memory.UsedBytes = m.UsedBytes
		}
		if p.UptimeSeconds > 0 {
			out.UptimeSeconds = p.UptimeSeconds
		}
		if n := p.Network; n != nil {
			out.Network = &NetworkPayload{RxBytes: n.RxBytes, TxBytes: n.TxBytes}
			rxRateSum += n.RxBytesPerSecond
			txRateSum += n.TxBytesPerSecond
		}
		for _, a := range p.Accelerators {
			ag := accMap[a.Index]
			if ag == nil {
				ag = &accAgg{idx: a.Index}
				accMap[a.Index] = ag
			}
			ag.utilSum += int64(a.UtilizationPercent)
			ag.memUsedSum += int64(a.MemoryUsedBytes)
			ag.tempSum += int64(a.TemperatureC)
			ag.powerSum += int64(a.PowerMW)
			ag.limitSum += int64(a.PowerLimitMW)
			ag.utilCnt++
			if a.TemperatureC > 0 {
				ag.tempCnt++
			}
			if a.PowerMW > 0 {
				ag.powCnt++
			}
			if a.PowerLimitMW > 0 {
				ag.limitCnt++
			}
			// retain latest capacity
			outAcc := ensureAcc(&out, a.Index)
			outAcc.MemoryTotalBytes = a.MemoryTotalBytes
		}
	}

	if cpuCount > 0 {
		out.CPU.UsagePercent = uint32(cpuSum / int64(cpuCount))
	}
	if out.Memory != nil {
		out.Memory.UsedBytes = uint64(memUsedSum / int64(len(payloads)))
	}
	if out.Network != nil {
		out.Network.RxBytesPerSecond = rxRateSum / uint64(len(payloads))
		out.Network.TxBytesPerSecond = txRateSum / uint64(len(payloads))
	}
	idxKeys := make([]int, 0, len(accMap))
	for k := range accMap {
		idxKeys = append(idxKeys, k)
	}
	sort.Ints(idxKeys)
	var accs []AcceleratorPayload
	for _, k := range idxKeys {
		ag := accMap[k]
		outA := AcceleratorPayload{
			Index:              ag.idx,
			UtilizationPercent: uint32(ag.utilSum / int64(ag.utilCnt)),
			MemoryUsedBytes:    uint64(ag.memUsedSum / int64(ag.utilCnt)),
		}
		if ag.tempCnt > 0 {
			outA.TemperatureC = uint32(ag.tempSum / int64(ag.tempCnt))
		}
		if ag.powCnt > 0 {
			outA.PowerMW = uint32(ag.powerSum / int64(ag.powCnt))
		}
		if ag.limitCnt > 0 {
			outA.PowerLimitMW = uint32(ag.limitSum / int64(ag.limitCnt))
		}
		accs = append(accs, outA)
	}
	out.Accelerators = accs
	return out
}

func ensureAcc(p *NodePayload, index int) *AcceleratorPayload {
	for i := range p.Accelerators {
		if p.Accelerators[i].Index == index {
			return &p.Accelerators[i]
		}
	}
	p.Accelerators = append(p.Accelerators, AcceleratorPayload{Index: index})
	return &p.Accelerators[len(p.Accelerators)-1]
}

// normalizeMinutePayload re-shapes a legacy minute row shaped as
// {"samples":…, "average":…, "last":…} into the nested typed payload so
// pre-upgrade retained telemetry remains readable.
func normalizeMinutePayload(raw []byte) (NodePayload, bool) {
	var candidate struct {
		Average json.RawMessage `json:"average"`
		Last    json.RawMessage `json:"last"`
	}
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return NodePayload{}, false
	}
	if len(candidate.Average) == 0 {
		return NodePayload{}, false
	}
	var p NodePayload
	if json.Unmarshal(candidate.Average, &p) != nil {
		return NodePayload{}, false
	}
	return p, true
}

// NodeHistory returns ordered samples for a node. Filesystem/process/throttle
// current-state arrays are stripped because the chart surface never consumes
// them; current state comes from LatestNodes.
func (s *Service) NodeHistory(ctx context.Context, nodeID, resolution string, from, to int64, limit int) ([]NodeSample, error) {
	table := "telemetry_5s"
	if resolution == "1m" {
		table = "telemetry_1m"
	} else if resolution != "5s" {
		return nil, fmt.Errorf("resolution must be 5s or 1m")
	}
	if limit < 1 || limit > maxHistoryLimit {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx, "SELECT ts, payload FROM "+table+" WHERE node_id = ? AND ts >= ? AND ts <= ? ORDER BY ts LIMIT ?", nodeID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]NodeSample, 0)
	for rows.Next() {
		var ts int64
		var raw string
		if err := rows.Scan(&ts, &raw); err != nil {
			return nil, err
		}
		var p NodePayload
		if resolution == "1m" {
			// Pre-upgrade rows may be legacy-shaped; normalize before direct read.
			if legacy, ok := normalizeMinutePayload([]byte(raw)); ok {
				p = legacy
			} else if json.Unmarshal([]byte(raw), &p) != nil {
				continue
			}
		} else {
			if json.Unmarshal([]byte(raw), &p) != nil {
				continue
			}
		}
		p.stripCurrentOnly()
		out = append(out, NodeSample{NodeID: nodeID, TS: ts, Payload: p})
	}
	return out, rows.Err()
}

// LatestNodes returns the newest full raw sample per node in one query.
func (s *Service) LatestNodes(ctx context.Context) (map[string]NodeSample, error) {
	rows, err := s.q.LatestTelemetryAll(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]NodeSample, len(rows))
	for _, r := range rows {
		var p NodePayload
		if json.Unmarshal([]byte(r.Payload), &p) != nil {
			continue
		}
		out[r.NodeID] = NodeSample{NodeID: r.NodeID, TS: r.Ts, Payload: p}
	}
	return out, nil
}

func (s *Service) Prune(ctx context.Context, now time.Time) error {
	cutRaw := now.Add(-RawRetention).Unix()
	cutMin := now.Add(-MinuteRetention).Unix()
	if err := s.q.DeleteTelemetry5sOlder(ctx, cutRaw); err != nil {
		return err
	}
	if err := s.q.DeleteTelemetry1mOlder(ctx, cutMin); err != nil {
		return err
	}
	if err := s.q.DeleteServingTelemetry5sOlder(ctx, cutRaw); err != nil {
		return err
	}
	return s.q.DeleteServingTelemetry1mOlder(ctx, cutMin)
}

func (s *Service) RunRetention(ctx context.Context) {
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	_ = s.Prune(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			_ = s.Prune(ctx, now)
		}
	}
}

// Prometheus renders current node and serving telemetry with bounded labels:
// aggregate node gauges carry only node_id; serving gauges only deployment_id.
// Unbounded label sets (process names, mount paths, model IDs) are never
// emitted.
func (s *Service) Prometheus(ctx context.Context) (string, error) {
	var lines []string
	latest, err := s.LatestNodes(ctx)
	if err != nil {
		return "", err
	}
	nodeIDs := make([]string, 0, len(latest))
	for id := range latest {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		p := latest[id].Payload
		lines = append(lines, fmt.Sprintf("lmw_node_telemetry_timestamp_seconds{node_id=%q} %d", id, latest[id].TS))
		if p.CPU != nil {
			lines = append(lines, fmt.Sprintf("lmw_node_cpu_usage_percent{node_id=%q} %d", id, p.CPU.UsagePercent))
		}
		if p.Memory != nil {
			lines = append(lines, fmt.Sprintf("lmw_node_memory_used_bytes{node_id=%q} %d", id, p.Memory.UsedBytes))
			lines = append(lines, fmt.Sprintf("lmw_node_memory_total_bytes{node_id=%q} %d", id, p.Memory.TotalBytes))
		}
		if p.Network != nil {
			lines = append(lines, fmt.Sprintf("lmw_node_network_rx_bytes_per_second{node_id=%q} %d", id, p.Network.RxBytesPerSecond))
			lines = append(lines, fmt.Sprintf("lmw_node_network_tx_bytes_per_second{node_id=%q} %d", id, p.Network.TxBytesPerSecond))
		}
		lines = append(lines, fmt.Sprintf("lmw_node_uptime_seconds{node_id=%q} %d", id, p.UptimeSeconds))
		var maxUtil, maxTemp uint32
		var sumMemUsed, sumMemTotal, sumPower, sumLimit uint64
		for _, a := range p.Accelerators {
			if a.UtilizationPercent > maxUtil {
				maxUtil = a.UtilizationPercent
			}
			if a.TemperatureC > maxTemp {
				maxTemp = a.TemperatureC
			}
			sumMemUsed += a.MemoryUsedBytes
			sumMemTotal += a.MemoryTotalBytes
			sumPower += uint64(a.PowerMW)
			sumLimit += uint64(a.PowerLimitMW)
		}
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_max_utilization_percent{node_id=%q} %d", id, maxUtil))
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_max_temperature_c{node_id=%q} %d", id, maxTemp))
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_memory_used_bytes{node_id=%q} %d", id, sumMemUsed))
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_memory_total_bytes{node_id=%q} %d", id, sumMemTotal))
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_power_mw{node_id=%q} %d", id, sumPower))
		lines = append(lines, fmt.Sprintf("lmw_node_gpu_power_limit_mw{node_id=%q} %d", id, sumLimit))
	}
	lines = append(lines, s.servingPrometheusLines(ctx)...)
	sort.Strings(lines)
	return "# TYPE lmw_node_telemetry_timestamp_seconds gauge\n" + strings.Join(lines, "\n") + "\n", nil
}
