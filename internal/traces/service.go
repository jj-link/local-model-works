// Package traces stores the complete, ordered, sanitized trajectories emitted by
// SWE-Gym replication jobs.
package traces

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
)

const SchemaVersion = "localmodelworks/agent-trace/v1"

var (
	ErrGap          = errors.New("trace stream has a byte gap")
	ErrConflict     = errors.New("trace replay conflicts with committed data")
	ErrIncomplete   = errors.New("trace chunk ends with an incomplete record")
	ErrNotRecording = errors.New("trace is not recording")
)

type Service struct {
	db       *sql.DB
	q        *db.Queries
	redactor Redactor
}

func New(sqlDB *sql.DB, queries *db.Queries, redactor Redactor) *Service {
	return &Service{db: sqlDB, q: queries, redactor: redactor}
}

type StartInput struct {
	RunID        string
	ExperimentID string
	TaskID       string
	Problem      string
	Repository   string
	BaseRevision string
	ModelSource  string
	Model        string
	Scaffold     string
	Sampling     any
	RetainUntil  *time.Time
	SecretValues []string
}

func (s *Service) Start(ctx context.Context, in StartInput) (*Recorder, error) {
	traceID, err := id.New()
	if err != nil {
		return nil, err
	}
	sampling, err := canonicalJSON(in.Sampling)
	if err != nil {
		return nil, fmt.Errorf("sampling metadata: %w", err)
	}
	if in.Scaffold == "" {
		in.Scaffold = "openhands-codeact"
	}
	if in.ModelSource != "lmw_deployment" && in.ModelSource != "external_api" {
		return nil, fmt.Errorf("invalid model source %q", in.ModelSource)
	}
	params := db.CreateCodingTraceParams{
		ID: traceID, RunID: in.RunID, ExperimentID: nullString(in.ExperimentID),
		TaskID: in.TaskID, Problem: in.Problem, Repository: in.Repository,
		BaseRevision: in.BaseRevision, ModelSource: in.ModelSource, Model: in.Model,
		Scaffold: in.Scaffold, Sampling: string(sampling), SchemaVersion: SchemaVersion,
		RedactionVersion: RedactionVersion,
	}
	if in.RetainUntil != nil {
		params.RetainUntil = sql.NullString{String: in.RetainUntil.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	if err := s.q.CreateCodingTrace(ctx, params); err != nil {
		return nil, err
	}
	return &Recorder{service: s, traceID: traceID, source: "controller", secretValues: append([]string(nil), in.SecretValues...)}, nil
}

type Event struct {
	Sequence       int64           `json:"sequence"`
	EventID        string          `json:"event_id"`
	AgentID        string          `json:"agent_id,omitempty"`
	ParentAgentID  string          `json:"parent_agent_id,omitempty"`
	OccurredAt     string          `json:"occurred_at"`
	Kind           string          `json:"kind"`
	Payload        json.RawMessage `json:"payload"`
	InputTokens    int64           `json:"input_tokens,omitempty"`
	OutputTokens   int64           `json:"output_tokens,omitempty"`
	RedactionCount int64           `json:"redaction_count,omitempty"`
}

type Chunk struct {
	TraceID      string
	NodeID       string
	Rank         int64
	Source       string
	Offset       int64
	Data         []byte
	Final        bool
	EndOffset    int64
	SecretValues []string
}

type Ack struct {
	CommittedOffset int64 `json:"committed_offset"`
	NextSequence    int64 `json:"next_sequence"`
	Final           bool  `json:"final"`
}

func (s *Service) IngestChunk(ctx context.Context, chunk Chunk) (Ack, error) {
	if chunk.TraceID == "" || chunk.NodeID == "" || chunk.Source == "" || chunk.Offset < 0 {
		return Ack{}, fmt.Errorf("invalid trace chunk identity")
	}
	boundary := bytes.LastIndexByte(chunk.Data, '\n') + 1
	if chunk.Final && boundary != len(chunk.Data) {
		return Ack{}, ErrIncomplete
	}
	committable := chunk.Data[:boundary]
	redactor := s.redactor.WithValues(chunk.SecretValues...)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ack{}, err
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	streamKey := db.GetCodingTraceStreamParams{TraceID: chunk.TraceID, NodeID: chunk.NodeID, Rank: chunk.Rank, Source: chunk.Source}
	stream, err := qtx.GetCodingTraceStream(ctx, streamKey)
	if errors.Is(err, sql.ErrNoRows) {
		if err := qtx.CreateCodingTraceStream(ctx, db.CreateCodingTraceStreamParams(streamKey)); err != nil {
			return Ack{}, err
		}
		stream = db.CodingTraceStream{TraceID: chunk.TraceID, NodeID: chunk.NodeID, Rank: chunk.Rank, Source: chunk.Source}
	} else if err != nil {
		return Ack{}, err
	}
	if stream.EofAcknowledged != 0 {
		if chunk.Offset == stream.ByteOffset && len(chunk.Data) == 0 && chunk.Final && chunk.EndOffset == stream.ByteOffset {
			return Ack{CommittedOffset: stream.ByteOffset, NextSequence: stream.NextEventSequence, Final: true}, nil
		}
		return Ack{}, ErrConflict
	}
	if chunk.Offset > stream.ByteOffset {
		return Ack{}, ErrGap
	}
	if chunk.Offset < stream.ByteOffset {
		if chunk.Offset+int64(boundary) > stream.ByteOffset || chunk.Final {
			return Ack{}, ErrConflict
		}
		if err := verifyReplay(ctx, qtx, chunk.TraceID, committable, redactor); err != nil {
			return Ack{}, err
		}
		return Ack{CommittedOffset: stream.ByteOffset, NextSequence: stream.NextEventSequence}, nil
	}

	events, err := decodeEvents(committable, redactor)
	if err != nil {
		return Ack{}, err
	}
	next := stream.NextEventSequence
	for _, event := range events {
		if event.Sequence != next {
			if event.Sequence > next {
				return Ack{}, ErrGap
			}
			return Ack{}, ErrConflict
		}
		if err := qtx.InsertCodingTraceEvent(ctx, db.InsertCodingTraceEventParams{
			TraceID: chunk.TraceID, Sequence: event.Sequence, EventID: event.EventID,
			AgentID: event.AgentID, ParentAgentID: event.ParentAgentID,
			OccurredAt: event.OccurredAt, Kind: event.Kind, Payload: string(event.Payload),
			InputTokens: event.InputTokens, OutputTokens: event.OutputTokens,
			RedactionCount: event.redactions,
		}); err != nil {
			return Ack{}, err
		}
		next++
	}
	newOffset := stream.ByteOffset + int64(boundary)
	finalOffset := sql.NullInt64{}
	eof := int64(0)
	if chunk.Final {
		if chunk.EndOffset != newOffset {
			return Ack{}, fmt.Errorf("final offset %d does not match committed offset %d", chunk.EndOffset, newOffset)
		}
		finalOffset = sql.NullInt64{Int64: newOffset, Valid: true}
		eof = 1
	}
	rows, err := qtx.AdvanceCodingTraceStream(ctx, db.AdvanceCodingTraceStreamParams{
		ByteOffset: newOffset, NextEventSequence: next, FinalOffset: finalOffset,
		EofAcknowledged: eof, TraceID: chunk.TraceID, NodeID: chunk.NodeID,
		Rank: chunk.Rank, Source: chunk.Source, ByteOffset_2: stream.ByteOffset,
		NextEventSequence_2: stream.NextEventSequence,
	})
	if err != nil {
		return Ack{}, err
	}
	if rows != 1 {
		return Ack{}, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return Ack{}, err
	}
	return Ack{CommittedOffset: newOffset, NextSequence: next, Final: chunk.Final}, nil
}

type decodedEvent struct {
	Event
	redactions int64
}

func decodeEvents(data []byte, redactor Redactor) ([]decodedEvent, error) {
	if len(data) == 0 {
		return nil, nil
	}
	lines := bytes.Split(data, []byte{'\n'})
	out := make([]decodedEvent, 0, len(lines)-1)
	for _, line := range lines[:len(lines)-1] {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("empty trace record")
		}
		var raw map[string]any
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("trace record: %w", err)
		}
		clean, count := redactor.Redact(raw)
		canonical, err := canonicalJSON(clean)
		if err != nil {
			return nil, err
		}
		var event Event
		if err := json.Unmarshal(canonical, &event); err != nil {
			return nil, err
		}
		if event.EventID == "" || event.OccurredAt == "" || !validEventKind(event.Kind) || len(event.Payload) == 0 {
			return nil, fmt.Errorf("trace record missing required fields")
		}
		if event.InputTokens < 0 || event.OutputTokens < 0 {
			return nil, fmt.Errorf("trace token counts must be non-negative")
		}
		occurred, err := time.Parse(time.RFC3339Nano, event.OccurredAt)
		if err != nil {
			return nil, fmt.Errorf("trace occurred_at: %w", err)
		}
		event.OccurredAt = occurred.UTC().Format(time.RFC3339Nano)
		var payload any
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("trace payload: %w", err)
		}
		payloadJSON, err := canonicalJSON(payload)
		if err != nil {
			return nil, err
		}
		event.Payload = payloadJSON
		out = append(out, decodedEvent{Event: event, redactions: event.RedactionCount + int64(count)})
	}
	return out, nil
}

func verifyReplay(ctx context.Context, q *db.Queries, traceID string, data []byte, redactor Redactor) error {
	events, err := decodeEvents(data, redactor)
	if err != nil {
		return err
	}
	for _, event := range events {
		stored, err := q.GetCodingTraceEvent(ctx, db.GetCodingTraceEventParams{TraceID: traceID, Sequence: event.Sequence})
		if err != nil {
			return ErrConflict
		}
		if stored.EventID != event.EventID || stored.Kind != event.Kind || stored.Payload != string(event.Payload) || stored.OccurredAt != event.OccurredAt {
			return ErrConflict
		}
	}
	return nil
}

func validEventKind(kind string) bool {
	switch kind {
	case "agent.lifecycle", "message", "model.request", "model.response", "tool.call", "tool.result":
		return true
	default:
		return false
	}
}

type Verification struct {
	ID               string
	Command          string
	TimeoutSeconds   int64
	ExitStatus       *int64
	Stdout           string
	Stderr           string
	FailToPassReport any
	PassToPassReport any
	Status           string
	FailureKind      string
}

type FinalizeInput struct {
	FinalDiff    string
	Verification Verification
}

func (s *Service) Finalize(ctx context.Context, traceID string, in FinalizeInput) error {
	return s.finalize(ctx, traceID, in, nil)
}

func (s *Service) finalize(ctx context.Context, traceID string, in FinalizeInput, secretValues []string) error {
	if in.Verification.Status != "resolved" && in.Verification.Status != "unresolved" && in.Verification.Status != "infrastructure_error" {
		return fmt.Errorf("invalid verification status %q", in.Verification.Status)
	}
	if in.Verification.TimeoutSeconds <= 0 || in.Verification.Command == "" {
		return fmt.Errorf("verification command and timeout are required")
	}
	if in.Verification.ID == "" {
		var err error
		in.Verification.ID, err = id.New()
		if err != nil {
			return err
		}
	}
	redactor := s.redactor.WithValues(secretValues...)
	cleanDiff, diffRedactions := redactor.redactString(in.FinalDiff)
	cleanCommand, commandRedactions := redactor.redactString(in.Verification.Command)
	cleanStdout, stdoutRedactions := redactor.redactString(in.Verification.Stdout)
	cleanStderr, stderrRedactions := redactor.redactString(in.Verification.Stderr)
	cleanF2P, f2pRedactions := redactor.Redact(in.Verification.FailToPassReport)
	f2p, err := canonicalJSON(cleanF2P)
	if err != nil {
		return err
	}
	cleanP2P, p2pRedactions := redactor.Redact(in.Verification.PassToPassReport)
	p2p, err := canonicalJSON(cleanP2P)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	if err := qtx.CreateCodingTraceVerification(ctx, db.CreateCodingTraceVerificationParams{
		ID: in.Verification.ID, TraceID: traceID, Command: cleanCommand,
		TimeoutSeconds: in.Verification.TimeoutSeconds, ExitStatus: nullInt(in.Verification.ExitStatus),
		Stdout: cleanStdout, Stderr: cleanStderr, FailToPassReport: string(f2p),
		PassToPassReport: string(p2p), Status: in.Verification.Status,
		FailureKind: nullString(in.Verification.FailureKind),
	}); err != nil {
		return err
	}
	tokens, turns, eventRedactions, digest, err := traceStats(ctx, tx, traceID, cleanDiff, in.Verification)
	if err != nil {
		return err
	}
	label := sql.NullInt64{}
	if in.Verification.Status != "infrastructure_error" {
		label = sql.NullInt64{Int64: 0, Valid: true}
		if in.Verification.Status == "resolved" {
			label.Int64 = 1
		}
	}
	rows, err := qtx.CompleteCodingTrace(ctx, db.CompleteCodingTraceParams{
		FinalDiff: nullString(cleanDiff), VerificationID: nullString(in.Verification.ID),
		SuccessLabel: label, FailureKind: nullString(in.Verification.FailureKind),
		TokenCount: tokens, TurnCount: turns,
		RedactionCount: eventRedactions + int64(diffRedactions+commandRedactions+stdoutRedactions+stderrRedactions+f2pRedactions+p2pRedactions),
		Digest:         nullString(digest), ID: traceID,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotRecording
	}
	return tx.Commit()
}

func (s *Service) Interrupt(ctx context.Context, traceID, failureKind string) error {
	tokens, turns, redactions, digest, err := traceStats(ctx, s.db, traceID, "", Verification{Status: "infrastructure_error", FailureKind: failureKind})
	if err != nil {
		return err
	}
	rows, err := s.q.InterruptCodingTrace(ctx, db.InterruptCodingTraceParams{
		FailureKind: nullString(failureKind), TokenCount: tokens, TurnCount: turns,
		RedactionCount: redactions, Digest: nullString(digest), ID: traceID,
	})
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrNotRecording
	}
	return nil
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func traceStats(ctx context.Context, conn queryer, traceID, diff string, verification Verification) (tokens, turns, redactions int64, digest string, err error) {
	rows, err := conn.QueryContext(ctx, `SELECT sequence,event_id,occurred_at,kind,payload,input_tokens,output_tokens,redaction_count FROM coding_trace_events WHERE trace_id=? ORDER BY sequence`, traceID)
	if err != nil {
		return 0, 0, 0, "", err
	}
	defer rows.Close()
	hash := sha256.New()
	encoder := json.NewEncoder(hash)
	for rows.Next() {
		var sequence int64
		var eventID, occurredAt, kind, payload string
		var in, out, count int64
		if err := rows.Scan(&sequence, &eventID, &occurredAt, &kind, &payload, &in, &out, &count); err != nil {
			return 0, 0, 0, "", err
		}
		_ = encoder.Encode([]any{sequence, eventID, occurredAt, kind, json.RawMessage(payload), in, out, count})
		tokens += in + out
		redactions += count
		if kind == "model.response" {
			turns++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, "", err
	}
	_ = encoder.Encode([]any{diff, verification.Status, verification.FailureKind})
	return tokens, turns, redactions, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func (s *Service) Get(ctx context.Context, traceID string) (db.CodingTrace, error) {
	return s.q.GetCodingTrace(ctx, traceID)
}
func (s *Service) GetByRun(ctx context.Context, runID string) (db.CodingTrace, error) {
	return s.q.GetCodingTraceByRunID(ctx, runID)
}

func (s *Service) List(ctx context.Context, state, taskID *string, success *bool, before *string, limit int64) ([]db.CodingTrace, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var stateArg, taskArg, successArg, beforeArg any
	if state != nil {
		stateArg = *state
	}
	if taskID != nil {
		taskArg = *taskID
	}
	if success != nil {
		if *success {
			successArg = int64(1)
		} else {
			successArg = int64(0)
		}
	}
	if before != nil {
		beforeArg = *before
	}
	return s.q.ListCodingTraces(ctx, db.ListCodingTracesParams{State: stateArg, TaskID: taskArg, SuccessLabel: successArg, CreatedBefore: beforeArg, Limit: limit})
}

func (s *Service) Events(ctx context.Context, traceID string, from, limit int64) ([]db.CodingTraceEvent, error) {
	if from < 0 {
		from = 0
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	return s.q.ListCodingTraceEvents(ctx, db.ListCodingTraceEventsParams{TraceID: traceID, Sequence: from, Limit: limit})
}
func (s *Service) AllEvents(ctx context.Context, traceID string) ([]db.CodingTraceEvent, error) {
	return s.q.ListAllCodingTraceEvents(ctx, traceID)
}

func (s *Service) Pin(ctx context.Context, traceID string, pinned bool) error {
	value := int64(0)
	if pinned {
		value = 1
	}
	rows, err := s.q.SetCodingTracePinned(ctx, db.SetCodingTracePinnedParams{Pinned: value, ID: traceID})
	if err != nil {
		return err
	}
	if rows != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Service) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	return s.q.DeleteExpiredCodingTraces(ctx, sql.NullString{String: now.UTC().Format(time.RFC3339Nano), Valid: true})
}

func (s *Service) Delete(ctx context.Context, traceID string) error {
	rows, err := s.q.DeleteCodingTrace(ctx, traceID)
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrConflict
	}
	return nil
}

type Recorder struct {
	mu           sync.Mutex
	service      *Service
	traceID      string
	source       string
	offset       int64
	next         int64
	closed       bool
	secretValues []string
}

func (r *Recorder) TraceID() string { return r.traceID }

func (r *Recorder) Append(ctx context.Context, event Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrNotRecording
	}
	event.Sequence = r.next
	if event.EventID == "" {
		var err error
		event.EventID, err = id.New()
		if err != nil {
			return err
		}
	}
	if event.OccurredAt == "" {
		event.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := canonicalJSON(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	ack, err := r.service.IngestChunk(ctx, Chunk{TraceID: r.traceID, NodeID: "controller", Source: r.source, Offset: r.offset, Data: line, SecretValues: r.secretValues})
	if err != nil {
		return err
	}
	r.offset, r.next = ack.CommittedOffset, ack.NextSequence
	return nil
}

func (r *Recorder) Finalize(ctx context.Context, in FinalizeInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrNotRecording
	}
	if _, err := r.service.IngestChunk(ctx, Chunk{TraceID: r.traceID, NodeID: "controller", Source: r.source, Offset: r.offset, Final: true, EndOffset: r.offset}); err != nil {
		return err
	}
	if err := r.service.finalize(ctx, r.traceID, in, r.secretValues); err != nil {
		return err
	}
	for i := range r.secretValues {
		r.secretValues[i] = ""
	}
	r.secretValues = nil
	r.closed = true
	return nil
}

func (r *Recorder) Interrupt(ctx context.Context, failureKind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrNotRecording
	}
	if err := r.service.Interrupt(ctx, r.traceID, failureKind); err != nil {
		return err
	}
	r.closed = true
	for i := range r.secretValues {
		r.secretValues[i] = ""
	}
	r.secretValues = nil
	return nil
}

func canonicalJSON(value any) ([]byte, error) {
	if value == nil {
		value = map[string]any{}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return nil, err
	}
	if err := ensureEOF(decoder); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func ensureEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: strings.TrimSpace(value) != ""}
}

func nullInt(value *int64) sql.NullInt64 {
	if value == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *value, Valid: true}
}
