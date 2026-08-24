package traces

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
)

func testService(t *testing.T) (*Service, *sql.DB, *db.Queries, string) {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	q := db.New(sqlDB)
	runID := "00000000-0000-4000-8000-000000000001"
	if err := q.CreateRun(ctx, db.CreateRunParams{
		ID: runID, Module: "coding-traces", Kind: "swe-gym-rollout", State: "running",
		Resources: "{}", Input: "{}", LegacyIdentity: sql.NullString{},
	}); err != nil {
		t.Fatal(err)
	}
	return New(sqlDB, q, NewRedactor("super-secret-value")), sqlDB, q, runID
}

func startTestTrace(t *testing.T, service *Service, runID string) *Recorder {
	t.Helper()
	recorder, err := service.Start(context.Background(), StartInput{
		RunID: runID, TaskID: "getmoto__moto-5752", Problem: "fix it",
		Repository: "getmoto/moto", BaseRevision: "deadbeef",
		ModelSource: "lmw_deployment", Model: "test-model", Sampling: map[string]any{"temperature": 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	return recorder
}

func traceLine(t *testing.T, sequence int64, eventID, content string) []byte {
	t.Helper()
	line, err := json.Marshal(Event{
		Sequence: sequence, EventID: eventID, OccurredAt: "2026-08-24T00:00:00Z",
		Kind: "message", Payload: json.RawMessage(`{"role":"user","content":` + string(mustJSON(t, content)) + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return append(line, '\n')
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIngestReplayGapEOFAndRedaction(t *testing.T) {
	ctx := context.Background()
	service, sqlDB, _, runID := testService(t)
	recorder := startTestTrace(t, service, runID)
	line := traceLine(t, 0, "event-0", "token super-secret-value and sk-1234567890abcdefghijkl")
	chunk := Chunk{TraceID: recorder.TraceID(), NodeID: "node-1", Source: "openhands", Data: line}
	ack, err := service.IngestChunk(ctx, chunk)
	if err != nil {
		t.Fatal(err)
	}
	if ack.CommittedOffset != int64(len(line)) || ack.NextSequence != 1 {
		t.Fatalf("unexpected ack: %+v", ack)
	}

	replay, err := service.IngestChunk(ctx, chunk)
	if err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	if replay != ack {
		t.Fatalf("replay ack = %+v, want %+v", replay, ack)
	}
	_, err = service.IngestChunk(ctx, Chunk{TraceID: recorder.TraceID(), NodeID: "node-1", Source: "openhands", Offset: ack.CommittedOffset + 1})
	if !errors.Is(err, ErrGap) {
		t.Fatalf("gap error = %v", err)
	}
	conflict := traceLine(t, 0, "event-0", "different")
	_, err = service.IngestChunk(ctx, Chunk{TraceID: recorder.TraceID(), NodeID: "node-1", Source: "openhands", Data: conflict})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict error = %v", err)
	}

	final, err := service.IngestChunk(ctx, Chunk{TraceID: recorder.TraceID(), NodeID: "node-1", Source: "openhands", Offset: ack.CommittedOffset, Final: true, EndOffset: ack.CommittedOffset})
	if err != nil {
		t.Fatal(err)
	}
	if !final.Final {
		t.Fatal("final chunk was not acknowledged")
	}
	var persisted string
	if err := sqlDB.QueryRowContext(ctx, `SELECT payload FROM coding_trace_events WHERE trace_id=? AND sequence=0`, recorder.TraceID()).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "super-secret-value") || strings.Contains(persisted, "sk-123456") {
		t.Fatalf("secret persisted: %s", persisted)
	}
	if !strings.Contains(persisted, "[REDACTED:stored-secret]") || !strings.Contains(persisted, "[REDACTED:token]") {
		t.Fatalf("redaction markers missing: %s", persisted)
	}
}

func TestFinalizeEligibilityAndStats(t *testing.T) {
	ctx := context.Background()
	service, _, q, runID := testService(t)
	recorder := startTestTrace(t, service, runID)
	if err := recorder.Append(ctx, Event{Kind: "model.response", Payload: json.RawMessage(`{"content":"patch"}`), InputTokens: 10, OutputTokens: 5}); err != nil {
		t.Fatal(err)
	}
	exit := int64(0)
	if err := recorder.Finalize(ctx, FinalizeInput{
		FinalDiff: "diff --git a/a b/a\n+super-secret-value",
		Verification: Verification{Command: "python test.py", TimeoutSeconds: 60, ExitStatus: &exit,
			Stdout: "Bearer super-secret-value", Status: "resolved",
			FailToPassReport: map[string]any{"passed": []string{"test_fix"}, "token": "super-secret-value"},
			PassToPassReport: map[string]any{"passed": []string{"test_existing"}}},
	}); err != nil {
		t.Fatal(err)
	}
	trace, err := q.GetCodingTrace(ctx, recorder.TraceID())
	if err != nil {
		t.Fatal(err)
	}
	if trace.State != "completed" || !trace.SuccessLabel.Valid || trace.SuccessLabel.Int64 != 1 || trace.TokenCount != 15 || trace.TurnCount != 1 || !trace.Digest.Valid {
		t.Fatalf("unexpected completed trace: %+v", trace)
	}
	if strings.Contains(trace.FinalDiff.String, "super-secret-value") || !strings.Contains(trace.FinalDiff.String, "[REDACTED:stored-secret]") {
		t.Fatalf("unsafe final diff: %s", trace.FinalDiff.String)
	}
	verification, err := q.GetCodingTraceVerification(ctx, recorder.TraceID())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(verification.Stdout+verification.FailToPassReport, "super-secret-value") {
		t.Fatalf("unsafe verification: %+v", verification)
	}
	if err := service.Interrupt(ctx, recorder.TraceID(), "late"); !errors.Is(err, ErrNotRecording) {
		t.Fatalf("second terminal transition = %v", err)
	}
}

func TestInterruptedTraceHasNoVerifierLabel(t *testing.T) {
	ctx := context.Background()
	service, _, q, runID := testService(t)
	recorder := startTestTrace(t, service, runID)
	if err := recorder.Interrupt(ctx, "runtime_disconnected"); err != nil {
		t.Fatal(err)
	}
	trace, err := q.GetCodingTrace(ctx, recorder.TraceID())
	if err != nil {
		t.Fatal(err)
	}
	if trace.State != "interrupted" || trace.SuccessLabel.Valid || trace.FailureKind.String != "runtime_disconnected" {
		t.Fatalf("unexpected interrupted trace: %+v", trace)
	}
}

func TestRetentionKeepsPinnedTraces(t *testing.T) {
	ctx := context.Background()
	service, sqlDB, _, runID := testService(t)
	past := time.Now().Add(-time.Hour)
	recorder, err := service.Start(ctx, StartInput{RunID: runID, TaskID: "task", Problem: "problem", Repository: "repo", BaseRevision: "base", ModelSource: "external_api", Model: "model", RetainUntil: &past})
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Interrupt(ctx, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if err := service.Pin(ctx, recorder.TraceID(), true); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.DeleteExpired(ctx, time.Now()); err != nil || deleted != 0 {
		t.Fatalf("pinned deletion: deleted=%d err=%v", deleted, err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE coding_traces SET pinned=0 WHERE id=?`, recorder.TraceID()); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.DeleteExpired(ctx, time.Now()); err != nil || deleted != 1 {
		t.Fatalf("expired deletion: deleted=%d err=%v", deleted, err)
	}
}
