// Package telemetry persists node samples at operational and historical
// resolutions and enforces bounded retention.
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
)

type Sample struct {
	NodeID  string          `json:"node_id"`
	TS      int64           `json:"ts"`
	Payload json.RawMessage `json:"payload"`
}

type Service struct {
	db *sql.DB
	q  *db.Queries
}

func New(database *sql.DB, queries *db.Queries) *Service { return &Service{db: database, q: queries} }

// Ingest stores the five-second sample and recomputes its minute aggregate
// transactionally. Replays and out-of-order samples therefore cannot skew the
// retained value.
func (s *Service) Ingest(ctx context.Context, nodeID string, at time.Time, payload []byte) error {
	ts := at.Unix()
	ts -= ts % 5
	if !json.Valid(payload) {
		return fmt.Errorf("telemetry payload is not JSON")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	qtx := s.q.WithTx(tx)
	if err := qtx.InsertTelemetry5s(ctx, db.InsertTelemetry5sParams{NodeID: nodeID, Ts: ts, Payload: string(payload)}); err != nil {
		tx.Rollback()
		return err
	}
	minute := ts - ts%60
	rows, err := tx.QueryContext(ctx, "SELECT payload FROM telemetry_5s WHERE node_id = ? AND ts >= ? AND ts < ? ORDER BY ts", nodeID, minute, minute+60)
	if err != nil {
		tx.Rollback()
		return err
	}
	var payloads []json.RawMessage
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			tx.Rollback()
			return err
		}
		payloads = append(payloads, json.RawMessage(raw))
	}
	rows.Close()
	aggregate, err := aggregateMinute(payloads)
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := qtx.InsertTelemetry1m(ctx, db.InsertTelemetry1mParams{NodeID: nodeID, Ts: minute, Payload: string(aggregate)}); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func aggregateMinute(payloads []json.RawMessage) ([]byte, error) {
	var sum any
	for index, payload := range payloads {
		var document any
		if err := json.Unmarshal(payload, &document); err != nil {
			return nil, err
		}
		sum = averageNumeric(sum, document, float64(index))
	}
	last := any(map[string]any{})
	if len(payloads) > 0 {
		if err := json.Unmarshal(payloads[len(payloads)-1], &last); err != nil {
			return nil, err
		}
	}
	return json.Marshal(map[string]any{"samples": len(payloads), "average": sum, "last": last})
}

func averageNumeric(current, incoming any, priorCount float64) any {
	switch value := incoming.(type) {
	case float64:
		if previous, ok := current.(float64); ok {
			return (previous*priorCount + value) / (priorCount + 1)
		}
		return value
	case map[string]any:
		out, _ := current.(map[string]any)
		if out == nil {
			out = map[string]any{}
		}
		for key, child := range value {
			out[key] = averageNumeric(out[key], child, priorCount)
		}
		return out
	case []any:
		out, _ := current.([]any)
		for len(out) < len(value) {
			out = append(out, nil)
		}
		for index, child := range value {
			out[index] = averageNumeric(out[index], child, priorCount)
		}
		return out
	default:
		return current
	}
}

func (s *Service) Prune(ctx context.Context, now time.Time) error {
	if err := s.q.DeleteTelemetry5sOlder(ctx, now.Add(-RawRetention).Unix()); err != nil {
		return err
	}
	return s.q.DeleteTelemetry1mOlder(ctx, now.Add(-MinuteRetention).Unix())
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

func (s *Service) History(ctx context.Context, nodeID, resolution string, from, to int64, limit int) ([]Sample, error) {
	table := "telemetry_5s"
	if resolution == "1m" {
		table = "telemetry_1m"
	} else if resolution != "5s" {
		return nil, fmt.Errorf("resolution must be 5s or 1m")
	}
	if limit < 1 || limit > 10000 {
		limit = 2000
	}
	rows, err := s.db.QueryContext(ctx, "SELECT node_id, ts, payload FROM "+table+" WHERE node_id = ? AND ts >= ? AND ts <= ? ORDER BY ts LIMIT ?", nodeID, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Sample, 0)
	for rows.Next() {
		var sample Sample
		var payload string
		if err := rows.Scan(&sample.NodeID, &sample.TS, &payload); err != nil {
			return nil, err
		}
		sample.Payload = json.RawMessage(payload)
		out = append(out, sample)
	}
	return out, rows.Err()
}

// Prometheus renders current node telemetry without retaining unbounded label
// sets. Only known scalar paths are emitted; node_id is the sole label.
func (s *Service) Prometheus(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.node_id, t.ts, t.payload FROM telemetry_5s t JOIN (SELECT node_id, MAX(ts) ts FROM telemetry_5s GROUP BY node_id) latest ON latest.node_id=t.node_id AND latest.ts=t.ts`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var nodeID, payload string
		var ts int64
		if err := rows.Scan(&nodeID, &ts, &payload); err != nil {
			return "", err
		}
		var document map[string]any
		if json.Unmarshal([]byte(payload), &document) != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("lmw_node_telemetry_timestamp_seconds{node_id=%q} %d", nodeID, ts))
		flattenMetrics(&lines, "", nodeID, document)
	}
	sort.Strings(lines)
	return "# TYPE lmw_node_telemetry_timestamp_seconds gauge\n" + strings.Join(lines, "\n") + "\n", rows.Err()
}

func flattenMetrics(lines *[]string, prefix, nodeID string, value map[string]any) {
	for key, raw := range value {
		name := strings.Trim(strings.ReplaceAll(prefix+"_"+key, "-", "_"), "_")
		switch typed := raw.(type) {
		case float64:
			*lines = append(*lines, fmt.Sprintf("lmw_node_%s{node_id=%q} %v", name, nodeID, typed))
		case map[string]any:
			flattenMetrics(lines, name, nodeID, typed)
		}
	}
}
