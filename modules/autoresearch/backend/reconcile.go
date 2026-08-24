package backend

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/workload"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

var _ moduleapi.NodeReconciler = (*Module)(nil)

func (m *Module) publishReconcile(nodeID, runID, code, message string) {
	if m.env.Bus == nil {
		return
	}
	payload, _ := json.Marshal(map[string]any{"run_id": runID, "code": code, "message": message})
	_ = m.env.Bus.Publish(context.Background(), "autoresearch.reconcile", nodeID, payload)
}

// ReconcileNode removes managed containers left by interrupted AutoResearch runs.
// The durable run remains interrupted; user Resume creates a linked replacement run.
func (m *Module) ReconcileNode(ctx context.Context, nodeID string) {
	rows, err := m.env.Q.ListInterruptedAutoResearchRunsByNode(ctx, nullString(nodeID))
	if err != nil {
		m.publishReconcile(nodeID, "", "autoresearch.reconcile_query", err.Error())
		return
	}
	for _, row := range rows {
		client := workload.New(m.env.Nodes, m.env.Commands, nodeID, "", row.RunID, 0)
		result, err := client.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, 15*time.Second)
		if err != nil {
			m.publishReconcile(nodeID, row.RunID, "autoresearch.reconcile_inspect", err.Error())
			return
		}
		if result == nil || !result.GetOk() {
			continue
		}
		if result.GetContainerState() == "paused" {
			resumed, resumeErr := client.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_UNPAUSE, nil, 15*time.Second)
			if resumeErr != nil || resumed == nil || !resumed.GetOk() {
				m.publishReconcile(nodeID, row.RunID, "autoresearch.reconcile_unpause", "could not unpause orphan")
				continue
			}
			result = resumed
		}
		if result.GetContainerState() == "running" {
			stopped, stopErr := client.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, 30*time.Second)
			if stopErr != nil || stopped == nil || !stopped.GetOk() {
				m.publishReconcile(nodeID, row.RunID, "autoresearch.reconcile_stop", "could not stop orphan")
				continue
			}
		}
		drainCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = m.env.Runs.WaitLogEnd(drainCtx, row.RunID, "", 0, "stdout")
		cancel()
		removed, removeErr := client.Do(ctx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, 30*time.Second)
		if removeErr != nil || removed == nil || !removed.GetOk() {
			m.publishReconcile(nodeID, row.RunID, "autoresearch.reconcile_remove", "could not remove orphan")
			continue
		}
		m.publishReconcile(nodeID, row.RunID, "autoresearch.reconciled", "interrupted container removed")
	}
}
