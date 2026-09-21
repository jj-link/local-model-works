package downloads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type NodeSender interface {
	Send(string, *agentv1.ServerMessage) bool
	Online(string) bool
}
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string                       { return e.Code + ": " + e.Message }
func failure(code, message string, status int) error { return &Error{code, message, status} }

type commandWaiter struct {
	node       string
	item       string
	spec       ResourceSpec
	credential string
	result     chan commandReply
}
type commandReply struct {
	output CommandOutput
	err    error
}

type Service struct {
	db         *sql.DB
	q          *db.Queries
	runs       *runs.Service
	nodes      NodeSender
	secrets    *auth.SecretBox
	ca         *ca.CA
	recipes    *recipe.Service
	jobs       *jobs.Registry
	mu         sync.Mutex
	dispatchMu sync.Mutex
	waiters    map[string]*commandWaiter
}

func New(database *sql.DB, q *db.Queries, ledger *runs.Service, nodes NodeSender, secrets *auth.SecretBox, authority *ca.CA, recipes *recipe.Service, registry *jobs.Registry) *Service {
	return &Service{db: database, q: q, runs: ledger, nodes: nodes, secrets: secrets, ca: authority, recipes: recipes, jobs: registry, waiters: make(map[string]*commandWaiter)}
}
func ns(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
func encoded(v any) string       { b, _ := json.Marshal(v); return string(b) }
func newID() string {
	v, err := id.New()
	if err != nil {
		panic(err)
	}
	return v
}
func (s *Service) JobSpec() jobs.Spec {
	return jobs.Spec{Kind: JobKind, Title: "Download recipe files", InputSchema: json.RawMessage(`{"type":"object","required":["plan"],"additionalProperties":false,"properties":{"plan":{"type":"object","required":["recipe_digest","resources","targets","plan_digest"],"properties":{"recipe_digest":{"type":"string"},"resources":{"type":"array","minItems":1,"maxItems":4096},"targets":{"type":"array","minItems":1,"maxItems":256},"plan_digest":{"type":"string"}}},"credentials":{"type":"array","maxItems":256},"predecessor_run_id":{"type":"string"}}}`), Executor: s.execute}
}
func (s *Service) Submit(ctx context.Context, req CreateRequest) (string, error) {
	if req.ResumeRunID != "" && req.Credentials == nil {
		prior, err := s.attempt(ctx, req.RecipeDigest, req.ResumeRunID)
		if err != nil {
			return "", err
		}
		req.Credentials = prior.Input.Credentials
	}
	plan, err := s.Plan(ctx, req.PlanRequest)
	if err != nil {
		return "", err
	}
	if req.PlanDigest == "" || req.PlanDigest != plan.PlanDigest {
		return "", failure("download.plan_changed", "Resources or source selection changed; review the new plan", 412)
	}
	if !plan.Ready {
		return "", failure("download.blocked", "Resolve every download plan blocker before continuing", 422)
	}
	if s.jobs == nil {
		return "", failure("download.unavailable", "Download job registry is unavailable", 503)
	}
	input := AttemptInput{Plan: *plan, Credentials: req.Credentials, PredecessorRunID: req.ResumeRunID}
	var body map[string]any
	if err = json.Unmarshal([]byte(encoded(input)), &body); err != nil {
		return "", err
	}
	return s.jobs.Submit(ctx, JobKind, body)
}
func (s *Service) Resume(ctx context.Context, runID string, req ResumeRequest) (string, error) {
	prior, err := s.attempt(ctx, req.RecipeDigest, runID)
	if err != nil {
		return "", err
	}
	if prior.State != "interrupted" && prior.State != "cancelled" && prior.State != "failed" {
		return "", failure("download.resume_state", "Only an interrupted, cancelled, or failed attempt can be resumed", 409)
	}
	p := prior.Input.Plan
	return s.Submit(ctx, CreateRequest{PlanRequest: PlanRequest{RecipeDigest: p.RecipeDigest, WorkloadIndex: &p.WorkloadIndex, Variants: p.Variants, Targets: p.Targets, Credentials: req.Credentials, ResumeRunID: runID}, PlanDigest: req.PlanDigest})
}
func (s *Service) List(ctx context.Context, digest string) ([]Attempt, error) {
	rows, err := s.q.ListRecipeDownloadRuns(ctx, digest)
	if err != nil {
		return nil, err
	}
	out := make([]Attempt, 0, len(rows))
	for _, r := range rows {
		a, err := s.attempt(ctx, digest, r.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, nil
}
func (s *Service) attempt(ctx context.Context, digest, runID string) (*Attempt, error) {
	row, err := s.q.GetRecipeDownloadRun(ctx, db.GetRecipeDownloadRunParams{ID: runID, Input: digest})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, failure("download.not_found", "Download attempt does not belong to this recipe", 404)
	}
	if err != nil {
		return nil, err
	}
	a := &Attempt{RunID: row.ID, State: row.State, Items: []Item{}}
	if err = json.Unmarshal([]byte(row.Input), &a.Input); err != nil {
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, row.CreatedAt)
	if row.FinishedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, row.FinishedAt.String)
		a.FinishedAt = &t
	}
	items, err := s.q.ListDownloadItems(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		v, err := itemView(item)
		if err != nil {
			return nil, err
		}
		a.Items = append(a.Items, v)
	}
	return a, nil
}
func itemView(row db.DownloadItem) (Item, error) {
	v := Item{ID: row.ID, RunID: row.RunID, PredecessorItemID: row.PredecessorItemID.String, ResourceKey: row.ResourceKey, NodeID: row.NodeID, State: ItemState(row.State), CommandID: row.CommandID.String, TransferID: row.TransferID.String}
	v.UpdatedAt, _ = time.Parse(time.RFC3339Nano, row.UpdatedAt)
	if err := json.Unmarshal([]byte(row.ResourceJson), &v.Resource); err != nil {
		return v, err
	}
	if err := json.Unmarshal([]byte(row.CheckpointJson), &v.Checkpoint); err != nil {
		return v, err
	}
	if row.ErrorJson.Valid {
		if err := json.Unmarshal([]byte(row.ErrorJson.String), &v.Error); err != nil {
			return v, err
		}
	}
	return v, nil
}

// inspect sends bounded, read-only work. The waiter is installed before dispatch
// and accepts only the enrolled sender and exact immutable resource requested.
func (s *Service) inspect(ctx context.Context, node string, spec ResourceSpec, credentials []CredentialSelection) (CommandOutput, error) {
	// Snapshot inspection hashes content. Allow bounded time proportional to
	// its declared size rather than timing out large, already-cached models.
	timeout := 45 * time.Second
	if spec.Kind == ResourceArtifact && spec.SizeBytes != nil && *spec.SizeBytes > 0 {
		seconds := min(*spec.SizeBytes/(256<<20), int64((30*time.Minute-timeout)/time.Second))
		timeout += time.Duration(seconds) * time.Second
	} else if spec.Kind == ResourceArtifact {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command, item := newID(), newID()
	return s.command(ctx, node, command, item, spec, agentv1.DownloadOp_DOWNLOAD_OP_INSPECT, "", credentials)
}
func (s *Service) command(ctx context.Context, node, command, item string, spec ResourceSpec, op agentv1.DownloadOp, target string, credentials []CredentialSelection) (CommandOutput, error) {
	if !s.SupportsNode(ctx, node) {
		return CommandOutput{}, failure("download.agent_upgrade_required", "Device does not advertise downloads-v1", 422)
	}
	material, err := s.credential(ctx, spec, credentials)
	if err != nil {
		return CommandOutput{}, err
	}
	w := &commandWaiter{node: node, item: item, spec: spec, result: make(chan commandReply, 1)}
	if material != nil {
		w.credential = material.Value
	}
	s.mu.Lock()
	s.waiters[command] = w
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiters, command); s.mu.Unlock() }()
	cmd := &agentv1.DownloadCommand{CommandId: command, ItemId: item, Op: op, ResourceJson: []byte(encoded(spec)), TargetCommandId: target}
	if material != nil {
		cmd.CredentialJson = []byte(encoded(material))
	}
	s.dispatchMu.Lock()
	if op == agentv1.DownloadOp_DOWNLOAD_OP_FETCH {
		row, e := s.q.GetDownloadItem(ctx, item)
		if e != nil || row.State != "transferring" || row.CommandID.String != command {
			s.dispatchMu.Unlock()
			return CommandOutput{}, failure("download.cancelled", "Acquisition no longer owns an active dispatch", 409)
		}
	}
	sent := s.nodes.Send(node, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_DownloadCommand{DownloadCommand: cmd}})
	s.dispatchMu.Unlock()
	if !sent {
		return CommandOutput{}, failure("download.node_offline", "Device is offline; acquisition requires explicit resume", 409)
	}
	select {
	case reply := <-w.result:
		return reply.output, reply.err
	case <-ctx.Done():
		return CommandOutput{}, ctx.Err()
	}
}
func (s *Service) OnResult(ctx context.Context, node string, result *agentv1.CommandResult) bool {
	s.mu.Lock()
	w := s.waiters[result.CommandId]
	s.mu.Unlock()
	if w != nil {
		if w.node != node {
			return true
		}
		var out CommandOutput
		err := json.Unmarshal(result.OutputJson, &out)
		if len(result.OutputJson) > MaxItemResourceJSONBytes {
			err = failure("download.result_invalid", "Device response exceeds protocol bounds", 422)
		}
		if err == nil {
			err = validateOutput(out, w.item, w.spec)
		}
		if !result.Ok {
			message := result.Error
			if w.credential != "" {
				message = strings.ReplaceAll(message, w.credential, "[redacted]")
			}
			message = strings.Map(func(r rune) rune {
				if r < 32 && r != '\n' && r != '\t' {
					return -1
				}
				return r
			}, message)
			if len(message) > 4096 {
				message = message[:4096]
			}
			if message == "" {
				message = "Device did not provide a failure diagnostic"
			}
			code := "download.device_failed"
			prefix, _, _ := strings.Cut(message, ":")
			if strings.HasPrefix(prefix, "download.") && !strings.ContainsAny(prefix, " \t\n/") {
				code = prefix
			}
			err = failure(code, message, 422)
		}
		select {
		case w.result <- commandReply{out, err}:
		default:
		}
		return true
	}
	// Interrupted attempts retain their commands and locks. A late matching terminal
	// acknowledgement proves quiescence but can never resurrect a terminal item.
	row, err := s.q.GetDownloadItemByCommand(ctx, db.GetDownloadItemByCommandParams{CommandID: ns(result.CommandId), NodeID: node})
	if err != nil {
		if s.onPeerResult(ctx, node, result) {
			return true
		}
		return s.ownsCommand(ctx, result.CommandId)
	}
	if row.State == string(ItemInterrupted) || row.State == string(ItemCancelling) {
		var out CommandOutput
		v, e := itemView(row)
		if e == nil && json.Unmarshal(result.OutputJson, &out) == nil && out.ItemID == row.ID && out.Identity == v.Resource.Identity && out.Path == v.Resource.Destination && out.Platform == v.Resource.Platform {
			s.quiesce(ctx, row, QuiescenceTerminalAck)
			if row.State == string(ItemCancelling) {
				s.transition(ctx, row, ItemCancelled, nil)
				s.finishCancelled(ctx, row.RunID)
			}
		}
	}
	return true
}

func (s *Service) ownsCommand(ctx context.Context, command string) bool {
	var owned bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM download_items WHERE command_id = ? OR transfer_id = ?)", command, command).Scan(&owned)
	return err == nil && owned
}

func validateOutput(out CommandOutput, item string, spec ResourceSpec) error {
	if out.ItemID != item || out.Identity != spec.Identity || out.Path != spec.Destination || out.Platform != spec.Platform {
		return failure("download.result_mismatch", "Device result does not match the frozen resource", 422)
	}
	if out.SizeBytes != nil && *out.SizeBytes < 0 {
		return failure("download.result_invalid", "Device reported negative resource size", 422)
	}
	if out.BytesRemaining != nil && (*out.BytesRemaining < 0 || out.SizeBytes != nil && *out.BytesRemaining > *out.SizeBytes) {
		return failure("download.result_invalid", "Device reported inconsistent verified remaining bytes", 422)
	}
	if out.TreeSizeBytes != nil && *out.TreeSizeBytes < 0 {
		return failure("download.result_invalid", "Device reported negative transfer-tree size", 422)
	}
	if spec.Kind == ResourceImage && out.State == ResourceAvailable {
		if out.IndexDigest != spec.IndexDigest || !resourceDigestPattern.MatchString(out.ManifestDigest) || (spec.ManifestDigest != "" && out.ManifestDigest != spec.ManifestDigest) {
			return failure("download.image_identity_mismatch", "Engine observation does not match the exact index/platform manifest", 422)
		}
	}
	switch out.State {
	case ResourceUnknown, ResourceMissing, ResourcePartial, ResourceVerifying, ResourceAvailable, ResourceInvalid:
	default:
		return failure("download.result_invalid", "Device reported an unknown resource state", 422)
	}
	if out.State == ResourceAvailable && (out.VerifiedAt == nil || time.Since(*out.VerifiedAt) > 2*time.Minute || out.VerifiedAt.After(time.Now().Add(time.Minute))) {
		return failure("download.verification_stale", "Device did not report fresh immutable verification", 422)
	}
	if out.Storage != nil {
		storage := out.Storage
		if storage.Destination != spec.Destination || storage.Filesystem == "" || len(storage.Filesystem) > MaxDestinationBytes {
			return failure("download.storage_invalid", "Device storage observation does not match the destination", 422)
		}
		if storage.AvailableBytes != nil && *storage.AvailableBytes < 0 || storage.TotalBytes != nil && *storage.TotalBytes < 0 {
			return failure("download.storage_invalid", "Device storage observation is negative", 422)
		}
	}
	return nil
}
func (s *Service) Acquire(ctx context.Context, req PlanRequest, requireExisting bool) error {
	plan, err := s.Plan(ctx, req)
	if err != nil {
		return err
	}
	if !requireExisting {
		joined, err := s.joinAcquisitions(ctx, plan)
		if err != nil {
			return err
		}
		if joined {
			return s.Acquire(ctx, req, false)
		}
	}
	if req.ReviewedPlan != nil {
		reviewed := req.ReviewedPlan
		if reviewed.RecipeDigest != plan.RecipeDigest || reviewed.WorkloadIndex != plan.WorkloadIndex || !maps.Equal(reviewed.Variants, plan.Variants) || encoded(reviewed.Targets) != encoded(plan.Targets) {
			return failure("download.plan_changed", "Selected devices or saved runtime choices changed", 412)
		}
		frozen := map[string]Resource{}
		for _, r := range reviewed.Resources {
			frozen[r.Key] = r
		}
		if len(frozen) != len(plan.Resources) {
			return failure("download.plan_changed", "Required resource set changed", 412)
		}
		for _, r := range plan.Resources {
			before, ok := frozen[r.Key]
			if !ok || before.IndexDigest != r.IndexDigest || before.ManifestDigest != r.ManifestDigest {
				return failure("download.plan_changed", "Reviewed resource destination, identity or platform changed", 412)
			}
			if r.Action != ActionReuse && (r.Action != before.Action || r.SourceNode != before.SourceNode || r.SourcePath != before.SourcePath || r.CredentialID != before.CredentialID) {
				return failure("download.plan_changed", "Reviewed acquisition source changed", 412)
			}
		}
	}
	if requireExisting {
		if !plan.Ready {
			return failure("download.recheck_required", "Required resources, identities or device observations could not be verified; review every blocker", 412)
		}
		for _, r := range plan.Resources {
			if r.Required && (r.Action != ActionReuse || r.Verification.State != ResourceAvailable || r.Verification.Stale) {
				return failure("download.recheck_required", "Required files changed or are missing; check existing files again", 412)
			}
		}
		if len(plan.Resources) == 0 {
			return failure("download.recheck_required", "No verified acquisition resources", 412)
		}
		return nil
	}
	runID, err := s.Submit(ctx, CreateRequest{PlanRequest: req, PlanDigest: plan.PlanDigest})
	if err != nil {
		return err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = s.Cancel(context.WithoutCancel(ctx), runID)
			return ctx.Err()
		case <-ticker.C:
			r, e := s.runs.Get(ctx, runID)
			if e != nil {
				return e
			}
			switch r.State {
			case "succeeded":
				return nil
			case "failed", "cancelled", "interrupted":
				return fmt.Errorf("download.acquisition_failed: download run %s is %s", runID, r.State)
			}
		}
	}
}

// A serving request may wait for an already-owned exact download, but never
// cancel that independent operator action or take over an interrupted writer.
func (s *Service) joinAcquisitions(ctx context.Context, plan *Plan) (bool, error) {
	owners := map[string]bool{}
	for _, resource := range plan.Resources {
		lock, err := s.q.GetDestinationLock(ctx, db.GetDestinationLockParams{NodeID: resource.NodeID, Destination: resource.Destination})
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		if lock.State == "quiesced" {
			continue
		}
		if lock.Identity != resource.Identity || lock.Platform != resource.Platform || lock.OwnerKind != string(OwnerDownload) || !lock.RunID.Valid {
			return false, failure("download.destination_busy", "Another resource owns the selected destination", 409)
		}
		owners[lock.RunID.String] = true
	}
	if len(owners) == 0 {
		return false, nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for len(owners) > 0 {
		for owner := range owners {
			run, err := s.runs.Get(ctx, owner)
			if err != nil {
				return false, err
			}
			switch run.State {
			case "succeeded":
				delete(owners, owner)
			case "cancelled", "interrupted", "failed", "cancelling":
				return false, failure("download.recheck_required", "Existing acquisition stopped or was interrupted; review a new explicit download plan", 412)
			}
		}
		if len(owners) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
	return true, nil
}
