package deploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/downloads"
	"github.com/jj-link/local-model-works/internal/id"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// CancelTransfer records cancellation intent before contacting the writer. An
// offline destination keeps its lock and cancelling state until a terminal ack.
func (s *Service) CancelTransfer(ctx context.Context, transferID string) error {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()
	transfer, err := s.q.GetTransfer(ctx, transferID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnknown
	}
	if err != nil {
		return err
	}
	switch transfer.State {
	case "succeeded", "failed", "cancelled":
		return nil
	}
	if s.downloads == nil {
		return fmt.Errorf("shared downloads service is unavailable")
	}
	locks, err := s.q.ListDestinationLocksByOwner(ctx, db.ListDestinationLocksByOwnerParams{OwnerKind: string(downloads.OwnerTransfer), OwnerID: transferID})
	if err != nil {
		return err
	}
	for _, lock := range locks {
		if err := s.downloads.MarkCancelling(ctx, lock.NodeID, lock.Destination, downloads.OwnerTransfer, transferID); err != nil {
			return err
		}
	}
	updated, err := s.db.ExecContext(ctx, "UPDATE transfers SET state='cancelling', updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state NOT IN ('succeeded','failed','cancelled')", transferID)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil || count == 0 {
		return err
	}
	if !s.nodes.Online(transfer.DestNode) || !s.downloads.SupportsNode(ctx, transfer.DestNode) {
		return nil
	}
	commandID, err := id.New()
	if err != nil {
		return err
	}
	s.inflightMark(commandID, transferID, 0, "transfer-cancel")
	if !s.nodes.Send(transfer.DestNode, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{TransferCommand: &agentv1.TransferCommand{TransferId: commandID, Op: agentv1.TransferOp_TRANSFER_OP_CANCEL, TargetTransferId: transferID}}}) {
		s.inflightTake(commandID)
	}
	return nil
}

// OnTransferCommandResult must precede generic transfer state handling. Only a
// matching enrolled destination's terminal acknowledgement releases ownership;
// a placement report or offline observation never proves writer quiescence.
func (s *Service) OnTransferCommandResult(ctx context.Context, nodeID string, result *agentv1.CommandResult) bool {
	transferID := result.CommandId
	cancel := false
	if command, ok := s.inflightPeek(result.CommandId); ok && command.Op == "transfer-cancel" {
		transferID, cancel = command.DepID, true
	}
	transfer, err := s.q.GetTransfer(ctx, transferID)
	if err != nil {
		return cancel
	}
	if transfer.DestNode != nodeID {
		return true
	}
	if cancel {
		s.inflightTake(result.CommandId)
		if !result.Ok {
			return true
		}
	}
	locks, err := s.q.ListDestinationLocksByOwner(ctx, db.ListDestinationLocksByOwnerParams{OwnerKind: string(downloads.OwnerTransfer), OwnerID: transferID})
	if err != nil {
		return true
	}
	// Download-owned peer transfers are handled by their durable item owner.
	if !cancel && len(locks) == 0 {
		return false
	}
	if s.downloads == nil {
		return true
	}
	for _, lock := range locks {
		if err := s.downloads.Unlock(ctx, lock.NodeID, lock.Destination, downloads.OwnerTransfer, transferID, downloads.QuiescenceTerminalAck); err != nil {
			return true
		}
	}
	state := "succeeded"
	if cancel || transfer.State == "cancelling" {
		state = "cancelled"
	} else if !result.Ok {
		state = "failed"
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE transfers SET state=CASE WHEN state='cancelling' THEN 'cancelled' ELSE ? END, diagnostic=?, updated_at=strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id=? AND state NOT IN ('succeeded','failed','cancelled')", state, sql.NullString{String: result.Error, Valid: result.Error != ""}, transferID)
	return true
}

func (s *Service) reconcileTransfers(ctx context.Context, nodeID string) {
	locks, err := s.q.ListUnquiescedDestinationLocksByNode(ctx, nodeID)
	if err != nil {
		return
	}
	for _, lock := range locks {
		if lock.OwnerKind == string(downloads.OwnerTransfer) {
			_ = s.CancelTransfer(ctx, lock.OwnerID)
		}
	}
}
