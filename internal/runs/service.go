// Package runs owns the run ledger: the state machine, resource leases,
// run-state transitions, and bounded byte-cursor log streaming.
//
// A run is one unit of operator-requested work (serve, stop, benchmark,
// recipe-import). The serve kind is long-lived: its run tracks a
// deployment for the lifetime of the workload. Every other kind is
// one-shot; on controller restart one-shot nonterminal runs are marked
// interrupted while deployments are reconciled from agent reports.
package runs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/cjson"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/id"
)

// State is one run state.
type State string

const (
	Queued      State = "queued"
	Planning    State = "planning"
	Waiting     State = "waiting"
	Running     State = "running"
	Verifying   State = "verifying"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Cancelling  State = "cancelling"
	Cancelled   State = "cancelled"
	Interrupted State = "interrupted"
)

// Terminal reports whether no further transition is possible.
func (s State) Terminal() bool {
	switch s {
	case Succeeded, Failed, Cancelled, Interrupted:
		return true
	}
	return false
}

// transitions is the allowed state graph.
var transitions = map[State][]State{
	Queued:     {Planning, Waiting, Running, Cancelling, Failed},
	Planning:   {Waiting, Running, Verifying, Cancelling, Failed},
	Waiting:    {Running, Cancelling, Failed},
	Running:    {Verifying, Cancelling, Failed, Interrupted},
	Verifying:  {Succeeded, Failed, Cancelling},
	Cancelling: {Cancelled, Failed},
}

// IsOneShot reports whether a run kind cannot outlive its work: on
// controller restart one-shot nonterminal runs become interrupted.
func IsOneShot(kind string) bool { return kind != "serve" && kind != "recipe-update" }

// ErrUnknown is a run that does not exist.
var ErrUnknown = errors.New("unknown run")

// ErrInvalidTransition is a state move outside the graph.
var ErrInvalidTransition = errors.New("invalid run transition")

// Resources is the rendered lease set of a run, normalized from the
// leases table (with the runs.resources snapshot as fallback for rows
// that hold no leases).
type Resources struct {
	Nodes        []string `json:"nodes,omitempty"`
	Accelerators []string `json:"accelerators,omitempty"`
	Fabrics      []string `json:"fabrics,omitempty"`
}

// Run is the API view of one ledger row.
type Run struct {
	ID             string         `json:"id"`
	Module         string         `json:"module"`
	Kind           string         `json:"kind"`
	State          string         `json:"state"`
	Resources      Resources      `json:"resources"`
	Input          map[string]any `json:"input"`
	Progress       map[string]any `json:"progress"`
	Output         map[string]any `json:"output,omitempty"`
	ErrorCode      *string        `json:"error_code,omitempty"`
	ErrorMessage   *string        `json:"error_message,omitempty"`
	DeploymentID   *string        `json:"deployment_id,omitempty"`
	LegacyIdentity *string        `json:"legacy_identity,omitempty"`
	CreatedAt      string         `json:"created_at"`
	StartedAt      *string        `json:"started_at,omitempty"`
	FinishedAt     *string        `json:"finished_at,omitempty"`
}

// Filter selects a ledger page. Zero Limit defaults to 50, max 200.
type Filter struct {
	Module        *string
	State         *string
	DeploymentID  *string
	CreatedBefore *string
	Limit         int
}

type Service struct {
	db      *sql.DB
	q       *db.Queries
	bus     *events.EventBus
	runRoot string

	logEndMu sync.Mutex
	logEnds  map[logEndKey]uint64
	logEndCh map[logEndKey]chan struct{}
}

type logEndKey struct {
	runID        string
	deploymentID string
	rank         int32
	stream       string
}

func New(database *sql.DB, q *db.Queries, bus *events.EventBus, runRoot string) *Service {
	return &Service{
		db: database, q: q, bus: bus, runRoot: runRoot,
		logEnds:  map[logEndKey]uint64{},
		logEndCh: map[logEndKey]chan struct{}{},
	}
}

// DB exposes the raw handle for multi-row transactions (run + deployment +
// leases must commit together).
func (s *Service) DB() *sql.DB { return s.db }

// Create appends a queued run. deploymentID may be "".
func (s *Service) Create(ctx context.Context, module, kind string, input map[string]any, deploymentID string) (string, error) {
	rid, err := id.New()
	if err != nil {
		return "", err
	}
	inj, err := cjson.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("run input: %w", err)
	}
	resj, _ := cjson.Marshal(map[string]any{})
	dep := sql.NullString{}
	if deploymentID != "" {
		dep = sql.NullString{String: deploymentID, Valid: true}
	}
	if err := s.q.CreateRun(ctx, db.CreateRunParams{
		ID: rid, Module: module, Kind: kind, State: string(Queued),
		Resources: string(resj), Input: string(inj), DeploymentID: dep, LegacyIdentity: sql.NullString{},
	}); err != nil {
		return "", err
	}
	_ = s.bus.Publish(ctx, "run.created", "", mustJSON(map[string]any{
		"run_id": rid, "module": module, "kind": kind,
	}))
	return rid, nil
}

// SetProgress persists a canonical progress snapshot and publishes it.
func (s *Service) SetProgress(ctx context.Context, rid string, progress map[string]any) error {
	encoded, err := cjson.Marshal(progress)
	if err != nil {
		return fmt.Errorf("run progress: %w", err)
	}
	if err := s.q.SetRunProgress(ctx, db.SetRunProgressParams{Progress: string(encoded), ID: rid}); err != nil {
		return err
	}
	return s.bus.Publish(ctx, "run.progress", "", mustJSON(map[string]any{
		"run_id": rid, "progress": progress,
	}))
}

// SetOutput persists canonical executor/coordinator output.
func (s *Service) SetOutput(ctx context.Context, rid string, output map[string]any) error {
	encoded, err := cjson.Marshal(output)
	if err != nil {
		return fmt.Errorf("run output: %w", err)
	}
	if err := s.q.SetRunOutput(ctx, db.SetRunOutputParams{Output: sql.NullString{String: string(encoded), Valid: true}, ID: rid}); err != nil {
		return err
	}
	return s.bus.Publish(ctx, "run.state", "", mustJSON(map[string]any{
		"run_id": rid, "output_updated": true,
	}))
}

// Get returns one run.
func (s *Service) Get(ctx context.Context, rid string) (Run, error) {
	row, err := s.q.GetRun(ctx, rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrUnknown
		}
		return Run{}, err
	}
	return s.view(ctx, row)
}

// List returns a ledger page, newest first.
func (s *Service) List(ctx context.Context, f Filter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var mod, st, cb any
	if f.Module != nil {
		mod = *f.Module
	}
	if f.State != nil {
		st = *f.State
	}
	if f.CreatedBefore != nil {
		cb = *f.CreatedBefore
	}
	rows, err := s.q.ListRuns(ctx, db.ListRunsParams{
		Module: mod, State: st, CreatedBefore: cb, Limit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Run, 0, len(rows))
	for _, r := range rows {
		if f.DeploymentID != nil && (!r.DeploymentID.Valid || r.DeploymentID.String != *f.DeploymentID) {
			continue
		}
		v, err := s.view(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// SetState moves a run to a new state, validating the transition graph.
// errorCode/errorMessage are recorded only when nonempty.
func (s *Service) SetState(ctx context.Context, rid string, to State, errorCode, errorMessage string) error {
	row, err := s.q.GetRun(ctx, rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknown
		}
		return err
	}
	from := State(row.State)
	if !canTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	started, finished := sql.NullString{}, sql.NullString{}
	if from == Queued && !row.StartedAt.Valid && !to.Terminal() {
		started = sql.NullString{String: now(), Valid: true}
	}
	if to.Terminal() {
		finished = sql.NullString{String: now(), Valid: true}
	}
	ec, em := sql.NullString{}, sql.NullString{}
	if errorCode != "" {
		ec = sql.NullString{String: errorCode, Valid: true}
	}
	if errorMessage != "" {
		em = sql.NullString{String: errorMessage, Valid: true}
	}
	if err := s.q.UpdateRunState(ctx, db.UpdateRunStateParams{
		State: string(to), StartedAt: started, FinishedAt: finished,
		ErrorCode: ec, ErrorMessage: em, ID: rid,
	}); err != nil {
		return err
	}
	_ = s.bus.Publish(ctx, "run.state", "", mustJSON(map[string]any{
		"run_id": rid, "from": string(from), "to": string(to),
	}))
	return nil
}

// Cancel requests cancellation from any nonterminal state. The caller is
// responsible for the agent-side stop (deployment stop, grader stop); this
// records the cancelling state.
func (s *Service) Cancel(ctx context.Context, rid string) error {
	row, err := s.q.GetRun(ctx, rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUnknown
		}
		return err
	}
	if State(row.State).Terminal() {
		return fmt.Errorf("%w: %s is terminal", ErrInvalidTransition, row.State)
	}
	return s.SetState(ctx, rid, Cancelling, "", "")
}

// Complete marks a run succeeded/failed and releases the owner's leases.
// A run with a deployment releases the deployment's leases (the run
// inherits ownership); otherwise its own.
func (s *Service) Complete(ctx context.Context, rid string, to State, errorCode, errorMessage string) error {
	if err := s.SetState(ctx, rid, to, errorCode, errorMessage); err != nil {
		return err
	}
	row, err := s.q.GetRun(ctx, rid)
	if err != nil {
		return err
	}
	kind, owner := "run", rid
	if row.DeploymentID.Valid {
		kind, owner = "deployment", row.DeploymentID.String
	}
	err = s.q.ReleaseLeases(ctx, db.ReleaseLeasesParams{OwnerKind: kind, OwnerID: owner})
	return err
}

// MarkInterrupted moves nonterminal one-shot runs to interrupted at
// controller start. It returns the number of rows moved.
func (s *Service) MarkInterrupted(ctx context.Context) (int, error) {
	rows, err := s.q.ListNonTerminalRuns(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		if !IsOneShot(r.Kind) {
			continue
		}
		if err := s.SetState(ctx, r.ID, Interrupted, "run.interrupted", "controller restart"); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// AcquireLeases records exclusive ownership of each resource for
// (ownerKind, ownerID) on the caller's transaction queries. A
// double-acquire fails the insert with a constraint error, which the
// caller rolls back as a conflict.
func (s *Service) AcquireLeases(ctx context.Context, qtx *db.Queries, ownerKind, ownerID string, resources []string) error {
	for _, r := range resources {
		if err := qtx.AcquireLease(ctx, db.AcquireLeaseParams{
			Resource: r, OwnerKind: ownerKind, OwnerID: ownerID,
		}); err != nil {
			return fmt.Errorf("lease %s: %w", r, err)
		}
	}
	return nil
}

// ReleaseLeasesFor releases every active lease of one owner.
func (s *Service) ReleaseLeasesFor(ctx context.Context, ownerKind, ownerID string) error {
	return s.q.ReleaseLeases(ctx, db.ReleaseLeasesParams{OwnerKind: ownerKind, OwnerID: ownerID})
}

// ActiveOwners returns the owners currently holding a resource.
func (s *Service) ActiveOwners(ctx context.Context, resource string) []db.ActiveLeaseOwnersRow {
	rows, err := s.q.ActiveLeaseOwners(ctx, resource)
	if err != nil {
		return nil
	}
	return rows
}

// ResourcesOf renders the lease set of one owner as API resources.
// Resource identities: node:<node_id>, gpu:<node_id>:<accel_uuid>,
// fabric:<fabric_id>, port:<node_id>:<host_port>.
func (s *Service) ResourcesOf(ctx context.Context, ownerKind, ownerID string) Resources {
	res := Resources{}
	rows, err := s.q.LeasesForOwner(ctx, db.LeasesForOwnerParams{OwnerKind: ownerKind, OwnerID: ownerID})
	if err != nil {
		return res
	}
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	for _, r := range rows {
		parts := strings.SplitN(r.Resource, ":", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "node":
			add(&res.Nodes, parts[1])
		case "gpu":
			// Keep the full lease identity (gpu:<node>:<uuid>) so values
			// round-trip into ActiveOwners without re-prefixing.
			add(&res.Accelerators, r.Resource)
		case "fabric":
			add(&res.Fabrics, parts[1])
		}
	}
	sort.Strings(res.Nodes)
	sort.Strings(res.Accelerators)
	sort.Strings(res.Fabrics)
	return res
}

// LogPath is the controller-side log file for one deployment rank/stream.
// It returns "" when a path component could escape runRoot.
func (s *Service) LogPath(runID, deploymentID string, rank int32, stream string) string {
	if deploymentID == "" {
		deploymentID = "adhoc"
	}
	if runID == "" || runID == "." || runID == ".." || deploymentID == "." ||
		deploymentID == ".." || filepath.Base(runID) != runID || filepath.Base(deploymentID) != deploymentID {
		return ""
	}
	if stream != "stdout" && stream != "stderr" {
		stream = "stdout"
	}
	return filepath.Join(s.runRoot, runID, "logs", deploymentID,
		filepath.Base(strings.ReplaceAll(deploymentID, " ", "_"))+"-rank"+itoa(int(rank))+"_"+stream+".log")
}

// AppendLog appends one streamed chunk to the run log file.
func (s *Service) AppendLog(runID, deploymentID string, rank int32, stream string, data []byte) error {
	path := s.LogPath(runID, deploymentID, rank, stream)
	if path == "" {
		return errors.New("run log path: unsafe id")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

// MaxLogChunk bounds one read for cursor streaming.
const MaxLogChunk = 1 << 20

// ReadLog reads up to maxBytes from offset. It returns the chunk, the next
// offset, and the total file size. A missing file yields no error with an
// empty chunk.
func (s *Service) ReadLog(runID, deploymentID string, rank int32, stream string, offset uint64, maxBytes int) (chunk []byte, nextOffset, size uint64, err error) {
	if maxBytes <= 0 {
		maxBytes = 256 << 10
	}
	if maxBytes > MaxLogChunk {
		maxBytes = MaxLogChunk
	}
	path := s.LogPath(runID, deploymentID, rank, stream)
	if path == "" {
		return nil, 0, 0, errors.New("run log path: unsafe id")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, err
	}
	size = uint64(st.Size())
	if offset > size {
		offset = size
	}
	if offset == size {
		return nil, size, size, nil
	}
	if _, err := f.Seek(int64(offset), io.SeekStart); err != nil {
		return nil, 0, size, err
	}
	n := int(min(uint64(maxBytes), size-offset))
	chunk = make([]byte, n)
	if _, err := io.ReadFull(f, chunk); err != nil && err != io.ErrUnexpectedEOF {
		return nil, 0, size, err
	}
	return chunk, offset + uint64(len(chunk)), size, nil
}

// MarkLogEnd records that one log stream reached EOF at endOffset (the
// agent's tailer sent its terminal LogChunk). Idempotent: a later call with
// a larger offset wins (resume/restart), smaller offsets are ignored.
func (s *Service) MarkLogEnd(runID, deploymentID string, rank int32, stream string, endOffset uint64) {
	if deploymentID == "" {
		deploymentID = "adhoc"
	}
	key := logEndKey{runID: runID, deploymentID: deploymentID, rank: rank, stream: stream}
	s.logEndMu.Lock()
	defer s.logEndMu.Unlock()
	if prev, ok := s.logEnds[key]; ok && prev >= endOffset {
		return
	}
	s.logEnds[key] = endOffset
	if ch, ok := s.logEndCh[key]; ok {
		close(ch)
		delete(s.logEndCh, key)
	}
}

// LogEnded reports the recorded EOF offset for one stream, if the terminal
// marker has been seen.
func (s *Service) LogEnded(runID, deploymentID string, rank int32, stream string) (uint64, bool) {
	if deploymentID == "" {
		deploymentID = "adhoc"
	}
	key := logEndKey{runID: runID, deploymentID: deploymentID, rank: rank, stream: stream}
	s.logEndMu.Lock()
	defer s.logEndMu.Unlock()
	v, ok := s.logEnds[key]
	return v, ok
}

// WaitLogEnd blocks until the stream's terminal marker is recorded or ctx
// is done; it returns the recorded EOF offset.
func (s *Service) WaitLogEnd(ctx context.Context, runID, deploymentID string, rank int32, stream string) (uint64, error) {
	if deploymentID == "" {
		deploymentID = "adhoc"
	}
	key := logEndKey{runID: runID, deploymentID: deploymentID, rank: rank, stream: stream}
	s.logEndMu.Lock()
	if v, ok := s.logEnds[key]; ok {
		s.logEndMu.Unlock()
		return v, nil
	}
	ch, ok := s.logEndCh[key]
	if !ok {
		ch = make(chan struct{})
		s.logEndCh[key] = ch
	}
	s.logEndMu.Unlock()
	select {
	case <-ch:
		v, _ := s.LogEnded(runID, deploymentID, rank, stream)
		return v, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// view renders a ledger row.
func (s *Service) view(ctx context.Context, row db.Run) (Run, error) {
	in := map[string]any{}
	_ = json.Unmarshal([]byte(row.Input), &in)
	progress := map[string]any{}
	_ = json.Unmarshal([]byte(row.Progress), &progress)
	out := map[string]any{}
	if row.Output.Valid {
		_ = json.Unmarshal([]byte(row.Output.String), &out)
	}
	ownKind, ownID := "run", row.ID
	if row.DeploymentID.Valid {
		ownKind, ownID = "deployment", row.DeploymentID.String
	}
	res := s.ResourcesOf(ctx, ownKind, ownID)
	if len(res.Nodes) == 0 && len(res.Accelerators) == 0 && len(res.Fabrics) == 0 && row.Resources != "" {
		// Fall back to the creation snapshot when no lease rows exist.
		var snap struct {
			Nodes        []string `json:"nodes"`
			Accelerators []string `json:"accelerators"`
			Fabrics      []string `json:"fabrics"`
		}
		if err := json.Unmarshal([]byte(row.Resources), &snap); err == nil {
			res = Resources{Nodes: snap.Nodes, Accelerators: snap.Accelerators, Fabrics: snap.Fabrics}
		}
	}
	v := Run{
		ID:        row.ID,
		Module:    row.Module,
		Kind:      row.Kind,
		State:     row.State,
		Resources: res,
		Input:     in,
		Progress:  progress,
		CreatedAt: row.CreatedAt,
	}
	if out != nil {
		v.Output = out
	}
	if row.ErrorCode.Valid {
		v.ErrorCode = &row.ErrorCode.String
	}
	if row.ErrorMessage.Valid {
		v.ErrorMessage = &row.ErrorMessage.String
	}
	if row.DeploymentID.Valid {
		v.DeploymentID = &row.DeploymentID.String
	}
	if row.LegacyIdentity.Valid {
		v.LegacyIdentity = &row.LegacyIdentity.String
	}
	if row.StartedAt.Valid {
		v.StartedAt = &row.StartedAt.String
	}
	if row.FinishedAt.Valid {
		v.FinishedAt = &row.FinishedAt.String
	}
	return v, nil
}

func canTransition(from, to State) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func min(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
