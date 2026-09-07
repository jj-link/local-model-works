package downloads

import (
	"context"
	"fmt"

	"github.com/jj-link/local-model-works/internal/db"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type peerCredential struct {
	TransferID   string `json:"transfer_id"`
	RunID        string `json:"run_id"`
	SourceNode   string `json:"source_node"`
	DestNode     string `json:"dest_node"`
	ArtifactID   string `json:"artifact_id"`
	SrcPath      string `json:"src_path"`
	SourceDigest string `json:"source_digest"`
	SrcSize      int64  `json:"src_size"`
	DestPath     string `json:"dest_path"`
	ExpUnix      int64  `json:"exp_unix"`
	Identity     string `json:"identity,omitempty"`
	TreeDigest   string `json:"tree_digest,omitempty"`
	Operation    string `json:"operation,omitempty"`
	Signature    string `json:"signature"`
}

func (s *Service) copyPeer(ctx context.Context, row db.DownloadItem, r Resource, creds []CredentialSelection) (CommandOutput, error) {
	transferID := newID()
	prepared, err := s.preparePeer(ctx, r, "", row.RunID, transferID, creds)
	if err != nil {
		return CommandOutput{}, err
	}
	artifact, err := s.q.GetArtifactByIdentity(ctx, r.Identity)
	if err != nil {
		return CommandOutput{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandOutput{}, err
	}
	defer tx.Rollback()
	q := s.q.WithTx(tx)
	if err = q.CreateTransfer(ctx, db.CreateTransferParams{ID: transferID, ArtifactID: artifact.ID, SourceNode: r.SourceNode, DestNode: r.NodeID, DestPath: r.Destination, CredentialHash: prepared.CredentialHash}); err != nil {
		return CommandOutput{}, err
	}
	n, err := q.BindDownloadItemTransfer(ctx, db.BindDownloadItemTransferParams{TransferID: ns(transferID), ID: row.ID, NodeID: row.NodeID, ExpectedState: row.State})
	if err != nil || n != 1 {
		return CommandOutput{}, failure("download.cancelled", "Peer acquisition was cancelled before dispatch", 409)
	}
	if err = tx.Commit(); err != nil {
		return CommandOutput{}, err
	}
	w := &commandWaiter{node: r.NodeID, item: row.ID, spec: r.ResourceSpec, result: make(chan commandReply, 1)}
	key := "peer:" + transferID
	s.mu.Lock()
	s.waiters[key] = w
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiters, key); s.mu.Unlock() }()
	s.dispatchMu.Lock()
	current, e := s.q.GetDownloadItem(ctx, row.ID)
	if e != nil || current.State != "transferring" || current.TransferID.String != transferID {
		s.dispatchMu.Unlock()
		return CommandOutput{}, failure("download.cancelled", "Peer attempt is no longer active", 409)
	}
	sent := s.nodes.Send(r.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{TransferCommand: prepared.Command}})
	s.dispatchMu.Unlock()
	if !sent {
		return CommandOutput{}, failure("download.node_offline", "Destination went offline before peer acquisition", 409)
	}
	select {
	case reply := <-w.result:
		if reply.err != nil {
			return CommandOutput{}, reply.err
		}
		return s.inspect(ctx, r.NodeID, r.ResourceSpec, creds)
	case <-ctx.Done():
		return CommandOutput{}, ctx.Err()
	}
}
func (s *Service) onPeerResult(ctx context.Context, node string, result *agentv1.CommandResult) bool {
	s.mu.Lock()
	w := s.waiters["peer:"+result.CommandId]
	s.mu.Unlock()
	if w != nil {
		if w.node != node {
			return true
		}
		var err error
		if !result.Ok {
			err = failure("download.peer_failed", "Peer transfer failed immutable verification or was cancelled", 422)
		}
		select {
		case w.result <- commandReply{err: err}:
		default:
		}
		return true
	}
	row, err := s.q.GetDownloadItemByTransfer(ctx, db.GetDownloadItemByTransferParams{TransferID: ns(result.CommandId), NodeID: node})
	if err != nil {
		return false
	}
	if row.State == "cancelling" || row.State == "interrupted" {
		_ = s.quiesce(ctx, row, QuiescenceTerminalAck)
		if row.State == "cancelling" {
			_, _ = s.transition(ctx, row, ItemCancelled, nil)
			s.finishCancelled(ctx, row.RunID)
		}
	}
	return true
}
func (s *Service) cancelPeer(ctx context.Context, row db.DownloadItem, r Resource) error {
	if !s.SupportsNode(ctx, row.NodeID) {
		return failure("download.agent_upgrade_required", "Device cannot acknowledge safe transfer cancellation", 422)
	}
	command := newID()
	key := "peer:" + command
	w := &commandWaiter{node: row.NodeID, item: row.ID, spec: r.ResourceSpec, result: make(chan commandReply, 1)}
	s.mu.Lock()
	s.waiters[key] = w
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.waiters, key); s.mu.Unlock() }()
	s.dispatchMu.Lock()
	sent := s.nodes.Send(row.NodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_TransferCommand{TransferCommand: &agentv1.TransferCommand{TransferId: command, Op: agentv1.TransferOp_TRANSFER_OP_CANCEL, TargetTransferId: row.TransferID.String}}})
	s.dispatchMu.Unlock()
	if !sent {
		return fmt.Errorf("download.node_offline: cancellation pending")
	}
	select {
	case result := <-w.result:
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) CancelTransfer(ctx context.Context, transferID string) (bool, error) {
	transfer, err := s.q.GetTransfer(ctx, transferID)
	if err != nil {
		return false, nil
	}
	row, err := s.q.GetDownloadItemByTransfer(ctx, db.GetDownloadItemByTransferParams{TransferID: ns(transferID), NodeID: transfer.DestNode})
	if err != nil {
		return false, nil
	}
	return true, s.Cancel(ctx, row.RunID)
}
func (s *Service) OnTransferProgress(ctx context.Context, node string, p *agentv1.TransferProgress) bool {
	row, err := s.q.GetDownloadItemByTransfer(ctx, db.GetDownloadItemByTransferParams{TransferID: ns(p.TransferId), NodeID: node})
	if err != nil {
		return false
	}
	total := p.BytesTotal
	s.OnProgress(ctx, node, &agentv1.DownloadProgress{ItemId: row.ID, CommandId: row.CommandID.String, Phase: "peer-copy", BytesDone: p.BytesDone, BytesTotal: &total})
	return true
}
