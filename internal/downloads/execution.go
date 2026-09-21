package downloads

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func (s *Service) Lock(ctx context.Context, node, destination, identity, platform string, kind OwnerKind, owner, run, item string) error {
	err := s.q.AcquireDestinationLock(ctx, db.AcquireDestinationLockParams{NodeID: node, Destination: destination, Identity: identity, Platform: platform, OwnerKind: string(kind), OwnerID: owner, RunID: ns(run), ItemID: ns(item)})
	if err != nil {
		return failure("download.destination_busy", "Another acquisition owns this destination until its device confirms it has stopped", 409)
	}
	return nil
}
func (s *Service) MarkCancelling(ctx context.Context, node, destination string, kind OwnerKind, owner string) error {
	_, err := s.q.CancelDestinationLock(ctx, db.CancelDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner})
	return err
}
func (s *Service) Unlock(ctx context.Context, node, destination string, kind OwnerKind, owner string, proof QuiescenceProof) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	lock, err := q.GetDestinationLock(ctx, db.GetDestinationLockParams{NodeID: node, Destination: destination})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if lock.OwnerKind != string(kind) || lock.OwnerID != owner {
		return failure("download.lock_owner", "Destination belongs to another acquisition", 409)
	}
	if lock.State != "quiesced" {
		if _, err = q.QuiesceDestinationLock(ctx, db.QuiesceDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner, ExpectedState: lock.State, QuiescenceProof: ns(string(proof))}); err != nil {
			return err
		}
	}
	if _, err = q.ReleaseDestinationLock(ctx, db.ReleaseDestinationLockParams{NodeID: node, Destination: destination, OwnerKind: string(kind), OwnerID: owner}); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Service) quiesce(ctx context.Context, row db.DownloadItem, proof QuiescenceProof) error {
	v, err := itemView(row)
	if err != nil {
		return err
	}
	return s.Unlock(ctx, row.NodeID, v.Resource.Destination, OwnerDownload, row.ID, proof)
}
func (s *Service) transition(ctx context.Context, row db.DownloadItem, state ItemState, itemErr *ItemError) (bool, error) {
	errJSON := row.ErrorJson
	if itemErr != nil {
		errJSON = ns(encoded(itemErr))
	}
	n, err := s.q.TransitionDownloadItem(ctx, db.TransitionDownloadItemParams{ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State, ExpectedCommandID: row.CommandID, ExpectedTransferID: row.TransferID, State: string(state), CheckpointJson: row.CheckpointJson, ErrorJson: errJSON})
	return n == 1, err
}
func (s *Service) execute(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var input AttemptInput
	if err := json.Unmarshal([]byte(encoded(c.Input)), &input); err != nil {
		return nil, err
	}
	if !input.Plan.Ready || input.Plan.PlanDigest != planHash(&input.Plan) || len(input.Plan.Resources) == 0 {
		return nil, failure("download.plan_invalid", "Frozen download plan is invalid", 422)
	}
	// Generic job submission cannot turn a caller-supplied resource list into
	// authority. Derive the selected saved recipe again before creating any item.
	selected := input.Plan
	fresh, err := s.Plan(ctx, PlanRequest{RecipeDigest: selected.RecipeDigest, Targets: selected.Targets, WorkloadIndex: &selected.WorkloadIndex, Variants: selected.Variants, Credentials: input.Credentials, ResumeRunID: input.PredecessorRunID})
	if err != nil {
		return nil, err
	}
	if !fresh.Ready || fresh.PlanDigest != selected.PlanDigest {
		return nil, failure("download.plan_changed", "Saved recipe resources or reviewed acquisition actions changed before dispatch", 412)
	}
	// Sizes and observations deliberately do not bind the review hash. Device
	// dispatch uses their fresh authoritative values, never caller-supplied ones.
	input.Plan.Resources = fresh.Resources
	predecessors := map[string]string{}
	if input.PredecessorRunID != "" {
		rows, err := s.q.ListDownloadItems(ctx, input.PredecessorRunID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			predecessors[r.ResourceKey] = r.ID
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	q := s.q.WithTx(tx)
	for _, r := range input.Plan.Resources {
		if err = r.ResourceSpec.Validate(); err != nil {
			tx.Rollback()
			return nil, err
		}
		if r.Key != resourceKey(r) {
			tx.Rollback()
			return nil, failure("download.plan_invalid", "Resource key does not match frozen content", 422)
		}
		if err = q.CreateDownloadItem(ctx, db.CreateDownloadItemParams{ID: newID(), RunID: c.RunID, PredecessorItemID: ns(predecessors[r.Key]), ResourceKey: r.Key, NodeID: r.NodeID, ResourceJson: encoded(r)}); err != nil {
			tx.Rollback()
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	rows, err := s.q.ListDownloadItems(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	var firstErr error
	for _, row := range rows {
		current, e := s.runs.Get(ctx, c.RunID)
		if e != nil {
			return nil, e
		}
		if current.State == "cancelling" {
			_ = s.Cancel(ctx, c.RunID)
			break
		}
		if err = s.acquireItem(ctx, row, input.Credentials); err != nil {
			firstErr = err
			break
		}
	}
	if ctx.Err() != nil {
		book, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		_, _ = s.q.InterruptDownloadItemsForRun(book, c.RunID)
		return nil, ctx.Err()
	}
	current, _ := s.runs.Get(ctx, c.RunID)
	if firstErr != nil || current.State == "cancelling" {
		// Stop all outstanding work; an offline attempt deliberately keeps the job
		// cancelling, its command ID and destination lock until an acknowledgement.
		_ = s.Cancel(ctx, c.RunID)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			items, e := s.q.ListDownloadItems(ctx, c.RunID)
			if e != nil {
				return nil, e
			}
			done := true
			for _, it := range items {
				if it.State == "cancelling" || it.State == "pending" || it.State == "checking" || it.State == "transferring" || it.State == "verifying" {
					done = false
				}
			}
			if done {
				if firstErr != nil {
					return nil, firstErr
				}
				_ = s.runs.Complete(ctx, c.RunID, runs.Cancelled, "run.cancelled", "Download cancelled after device quiescence")
				return nil, context.Canceled
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	items, err := s.q.ListDownloadItems(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.State != "succeeded" {
			return nil, fmt.Errorf("download.incomplete: resource %s has not verified", item.ResourceKey)
		}
	}
	return map[string]any{"recipe_digest": input.Plan.RecipeDigest, "state": "files_downloaded", "resources": len(items)}, nil
}
func (s *Service) acquireItem(ctx context.Context, row db.DownloadItem, creds []CredentialSelection) error {
	v, err := itemView(row)
	if err != nil {
		return err
	}
	if err = s.Lock(ctx, row.NodeID, v.Resource.Destination, v.Resource.Identity, v.Resource.Platform, OwnerDownload, row.ID, row.RunID, row.ID); err != nil {
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.destination_busy", Message: err.Error(), Retryable: true})
		return err
	}
	changed, err := s.transition(ctx, row, ItemChecking, nil)
	if err != nil || !changed {
		s.quiesce(ctx, row, QuiescenceNeverDispatched)
		return err
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	// Every item is inspected under ownership again. An approved origin/peer may
	// become reusable, but never silently switches to another network source.
	out, err := s.inspect(ctx, row.NodeID, v.Resource.ResourceSpec, creds)
	if err != nil {
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.inspect_failed", Message: err.Error(), Retryable: true})
		s.quiesce(ctx, row, QuiescenceNeverDispatched)
		return err
	}
	if out.State == ResourceAvailable && out.VerifiedAt != nil {
		if err = s.recordObservation(ctx, v.Resource, out); err != nil {
			s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.observation_failed", Message: "Cannot persist verified placement", Retryable: true})
			s.quiesce(ctx, row, QuiescenceNeverDispatched)
			return err
		}
		if changed, err = s.transition(ctx, row, ItemSucceeded, nil); err != nil || !changed {
			return err
		}
		return s.quiesce(ctx, row, QuiescenceNeverDispatched)
	}
	if v.Resource.Action == ActionReuse {
		err = failure("download.plan_changed", "Previously verified resource is missing; review the changed plan", 412)
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.plan_changed", Message: err.Error(), Retryable: true})
		s.quiesce(ctx, row, QuiescenceNeverDispatched)
		return err
	}
	remaining := out.BytesRemaining
	if remaining == nil {
		remaining = out.SizeBytes
	}
	if out.Storage == nil || out.Storage.AvailableBytes == nil || remaining == nil || *remaining < 0 || *remaining > (1<<62)-StorageReserveBytes || *out.Storage.AvailableBytes < 2*(*remaining)+StorageReserveBytes {
		err = failure("download.storage_changed", "Current device capacity cannot safely accommodate acquisition and staging", 412)
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.storage_changed", Message: err.Error(), Retryable: true})
		s.quiesce(ctx, row, QuiescenceNeverDispatched)
		return err
	}
	var command string
	if v.Resource.Action != ActionPeerCopy {
		command = newID()
		n, bindErr := s.q.BindDownloadItemCommand(ctx, db.BindDownloadItemCommandParams{ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State, CommandID: ns(command)})
		if bindErr != nil || n != 1 {
			s.quiesce(ctx, row, QuiescenceNeverDispatched)
			if bindErr == nil {
				bindErr = failure("download.cancelled", "Acquisition was cancelled before dispatch", 409)
			}
			return bindErr
		}
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	changed, err = s.transition(ctx, row, ItemTransferring, nil)
	if err != nil || !changed {
		return err
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	if v.Resource.Action == ActionPeerCopy {
		out, err = s.copyPeer(ctx, row, v.Resource, creds)
	} else {
		out, err = s.command(ctx, row.NodeID, command, row.ID, v.Resource.ResourceSpec, agentv1.DownloadOp_DOWNLOAD_OP_FETCH, "", creds)
	}
	current, e := s.q.GetDownloadItem(ctx, row.ID)
	if e != nil {
		return e
	}
	row = current
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if row.State == "cancelling" || row.State == "cancelled" || row.State == "interrupted" {
		return context.Canceled
	}
	if err != nil {
		// An error or lost response is not proof that the writer stopped.
		// Cancellation must obtain its separate terminal acknowledgement.
		_, _ = s.transition(ctx, row, ItemCancelling, &ItemError{Code: "download.acquisition_failed", Message: err.Error(), Retryable: true})
		return err
	}
	if _, err = s.transition(ctx, row, ItemVerifying, nil); err != nil {
		return err
	}
	row, _ = s.q.GetDownloadItem(ctx, row.ID)
	// FETCH success is not enough: inspect the exact bytes/platform again before
	// publishing availability or finalizing the complete parent run.
	out, err = s.inspect(ctx, row.NodeID, v.Resource.ResourceSpec, creds)
	if err == nil && (out.State != ResourceAvailable || out.VerifiedAt == nil) {
		err = failure("download.verification_failed", "Resource did not pass final immutable verification", 422)
	}
	if err != nil {
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.verification_failed", Message: err.Error(), Retryable: true})
		s.quiesce(ctx, row, QuiescenceTerminalAck)
		return err
	}
	if err = s.recordObservation(ctx, v.Resource, out); err != nil {
		s.transition(ctx, row, ItemFailed, &ItemError{Code: "download.observation_failed", Message: "Cannot persist verified placement", Retryable: true})
		s.quiesce(ctx, row, QuiescenceTerminalAck)
		return err
	}
	if changed, err = s.transition(ctx, row, ItemSucceeded, nil); err != nil || !changed {
		return err
	}
	return s.quiesce(ctx, row, QuiescenceTerminalAck)
}
func (s *Service) Cancel(ctx context.Context, runID string) error {
	run, err := s.runs.Get(ctx, runID)
	if err != nil {
		return err
	}
	if run.Kind != JobKind {
		return failure("download.not_found", "Run is not a recipe download", 404)
	}
	if runs.State(run.State).Terminal() {
		return nil
	}
	if run.State != string(runs.Cancelling) {
		if err = s.runs.Cancel(ctx, runID); err != nil {
			return err
		}
	}
	rows, err := s.q.ListDownloadItems(ctx, runID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		switch row.State {
		case "pending", "checking", "transferring", "verifying":
			if _, e := s.transition(ctx, row, ItemCancelling, nil); e != nil {
				return e
			}
			row, _ = s.q.GetDownloadItem(ctx, row.ID)
		case "cancelling":
		default:
			continue
		}
		v, e := itemView(row)
		if e != nil {
			return e
		}
		_ = s.MarkCancelling(ctx, row.NodeID, v.Resource.Destination, OwnerDownload, row.ID)
		// Peer dispatch requires a persisted transfer binding; an origin-command
		// placeholder from an older attempt does not represent a peer writer.
		if !row.TransferID.Valid && (v.Resource.Action == ActionPeerCopy || !row.CommandID.Valid) {
			_ = s.quiesce(ctx, row, QuiescenceNeverDispatched)
			_, _ = s.transition(ctx, row, ItemCancelled, nil)
			continue
		}
		if s.nodes.Online(row.NodeID) {
			go s.cancelItem(context.WithoutCancel(ctx), row)
		}
	}
	s.finishCancelled(ctx, runID)
	return nil
}
func (s *Service) cancelItem(ctx context.Context, row db.DownloadItem) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	v, err := itemView(row)
	if err != nil {
		return
	}
	proof := QuiescenceTerminalAck
	if v.Resource.Action == ActionPeerCopy && !row.TransferID.Valid {
		proof = QuiescenceNeverDispatched
	} else if row.TransferID.Valid {
		err = s.cancelPeer(ctx, row, v.Resource)
	} else {
		_, err = s.command(ctx, row.NodeID, newID(), row.ID, v.Resource.ResourceSpec, agentv1.DownloadOp_DOWNLOAD_OP_CANCEL, row.CommandID.String, nil)
	}
	if err != nil {
		return
	}
	_ = s.quiesce(ctx, row, proof)
	latest, e := s.q.GetDownloadItem(ctx, row.ID)
	if e == nil && latest.State == "cancelling" {
		_, _ = s.transition(ctx, latest, ItemCancelled, nil)
	}
	s.finishCancelled(ctx, row.RunID)
}
func (s *Service) finishCancelled(ctx context.Context, runID string) {
	rows, err := s.q.ListDownloadItems(ctx, runID)
	if err != nil {
		return
	}
	failed := false
	for _, row := range rows {
		switch row.State {
		case "pending", "checking", "transferring", "verifying", "cancelling":
			return
		}
		if row.State == "failed" || row.ErrorJson.Valid {
			failed = true
		}
	}
	run, err := s.runs.Get(ctx, runID)
	if err != nil || run.State != "cancelling" {
		return
	}
	if failed {
		_ = s.runs.Complete(ctx, runID, runs.Failed, "download.failed", "A required resource failed; partial successes remain available")
		return
	}
	_ = s.runs.Complete(ctx, runID, runs.Cancelled, "run.cancelled", "Device acquisition has stopped")
}
func (s *Service) Reconcile(ctx context.Context) error {
	nodes, err := s.q.ListNodes(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		items, e := s.q.ListActiveDownloadItemsByNode(ctx, node.ID)
		if e != nil {
			return e
		}
		for _, item := range items {
			if seen[item.RunID] {
				continue
			}
			seen[item.RunID] = true
			if _, e = s.q.InterruptDownloadItemsForRun(ctx, item.RunID); e != nil {
				return e
			}
			_ = s.runs.SetState(ctx, item.RunID, runs.Interrupted, "download.interrupted", "Acquisition was interrupted; review a fresh plan and choose Resume")
		}
	}
	return nil
}
func (s *Service) OnReconnect(ctx context.Context, node string) {
	locks, err := s.q.ListUnquiescedDestinationLocksByNode(ctx, node)
	if err != nil {
		return
	}
	for _, lock := range locks {
		if lock.OwnerKind != string(OwnerDownload) || !lock.ItemID.Valid {
			continue
		}
		row, e := s.q.GetDownloadItem(ctx, lock.ItemID.String)
		if e != nil {
			continue
		}
		if !row.CommandID.Valid && !row.TransferID.Valid {
			_ = s.quiesce(ctx, row, QuiescenceNeverDispatched)
			continue
		}
		go s.cancelItem(context.WithoutCancel(ctx), row)
	}
}
func (s *Service) OnProgress(ctx context.Context, node string, p *agentv1.DownloadProgress) {
	row, err := s.q.GetDownloadItemByCommand(ctx, db.GetDownloadItemByCommandParams{CommandID: ns(p.CommandId), NodeID: node})
	if err != nil || row.ID != p.ItemId {
		return
	}
	var checkpoint Checkpoint
	if json.Unmarshal([]byte(row.CheckpointJson), &checkpoint) != nil {
		return
	}
	if checkpoint.Progress != nil && (p.BytesDone < checkpoint.Progress.BytesDone || p.FilesDone < checkpoint.Progress.FilesDone) {
		return
	}
	if len(p.CurrentFile) > MaxDestinationBytes || len(p.Phase) > 128 {
		return
	}
	if p.BytesTotal != nil && p.BytesDone > *p.BytesTotal {
		return
	}
	if p.FilesTotal != nil && p.FilesDone > *p.FilesTotal {
		return
	}
	checkpoint.Progress = &Progress{ItemID: p.ItemId, CommandID: p.CommandId, Phase: p.Phase, BytesDone: p.BytesDone, BytesTotal: p.BytesTotal, FilesDone: p.FilesDone, FilesTotal: p.FilesTotal, CurrentFile: p.CurrentFile}
	n, err := s.q.RecordDownloadItemCheckpoint(ctx, db.RecordDownloadItemCheckpointParams{ID: row.ID, NodeID: node, ExpectedState: row.State, ExpectedCommandID: row.CommandID, ExpectedTransferID: row.TransferID, ExpectedCheckpointJson: row.CheckpointJson, CheckpointJson: encoded(checkpoint)})
	if err != nil || n != 1 {
		return
	}
	items, err := s.q.ListDownloadItems(ctx, row.RunID)
	if err != nil {
		return
	}
	views := make([]Item, 0, len(items))
	for _, item := range items {
		v, e := itemView(item)
		if e == nil {
			views = append(views, v)
		}
	}
	_ = s.runs.SetProgress(ctx, row.RunID, map[string]any{"items": views})
}
