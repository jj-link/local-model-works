package downloads

import (
	"context"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/runs"
)

// OnDisconnect never infers that a device writer stopped. Keep destination locks
// and frozen commands so reconnect can request a quiescence barrier, not replay.
func (s *Service) OnDisconnect(ctx context.Context, node string) {
	rows, err := s.q.ListActiveDownloadItemsByNode(ctx, node)
	if err != nil {
		return
	}
	parents := map[string]bool{}
	for _, row := range rows {
		if parents[row.RunID] {
			continue
		}
		parents[row.RunID] = true
		run, e := s.runs.Get(ctx, row.RunID)
		if e != nil {
			continue
		}
		if run.State == "cancelling" {
			continue
		}
		_, _ = s.q.InterruptDownloadItemsForRun(ctx, row.RunID)
		_ = s.runs.SetState(ctx, row.RunID, runs.Interrupted, "download.interrupted", "Device disconnected; a new reviewed plan and explicit Resume are required")
	}
	affected := map[string]bool{}
	var cancelRows []db.DownloadItem
	for parent := range parents {
		items, e := s.q.ListDownloadItems(ctx, parent)
		if e != nil {
			continue
		}
		for _, item := range items {
			affected[item.ID] = true
			if item.State == "interrupted" && item.NodeID != node && s.nodes.Online(item.NodeID) {
				cancelRows = append(cancelRows, item)
			}
		}
	}
	s.mu.Lock()
	for _, waiter := range s.waiters {
		if waiter.node != node && !affected[waiter.item] {
			continue
		}
		select {
		case waiter.result <- commandReply{err: failure("download.interrupted", "Device disconnected; acquisition remains locked until quiescence", 409)}:
		default:
		}
	}
	s.mu.Unlock()
	for _, item := range cancelRows {
		if !item.CommandID.Valid && !item.TransferID.Valid {
			_ = s.quiesce(ctx, item, QuiescenceNeverDispatched)
			continue
		}
		go s.cancelItem(context.WithoutCancel(ctx), item)
	}
}
