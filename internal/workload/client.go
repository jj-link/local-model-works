// Package workload dispatches acknowledged container operations to enrolled agents.
package workload

import (
	"context"
	"fmt"
	"time"

	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/nodes"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// Client is bound to one managed workload identity on one node.
type Client struct {
	nodes        *nodes.Registry
	commands     *commands.Broker
	nodeID       string
	deploymentID string
	runID        string
	rank         int32
}

// New creates a workload client. Either deploymentID or runID must be set.
func New(nodesRegistry *nodes.Registry, broker *commands.Broker, nodeID, deploymentID, runID string, rank int32) *Client {
	return &Client{
		nodes: nodesRegistry, commands: broker, nodeID: nodeID,
		deploymentID: deploymentID, runID: runID, rank: rank,
	}
}

// Do sends one exact workload operation and waits for that command's acknowledgement.
func (c *Client) Do(ctx context.Context, op agentv1.WorkloadOp, specJSON []byte, timeout time.Duration) (*agentv1.CommandResult, error) {
	if c == nil || c.nodes == nil || c.commands == nil || c.nodeID == "" || (c.deploymentID == "" && c.runID == "") || c.rank < 0 {
		return nil, fmt.Errorf("workload.client_invalid")
	}
	commandID, err := id.New()
	if err != nil {
		return nil, err
	}
	message := &agentv1.ServerMessage{Body: &agentv1.ServerMessage_WorkloadCommand{
		WorkloadCommand: &agentv1.WorkloadCommand{
			CommandId: commandID, Op: op, DeploymentId: c.deploymentID,
			RunId: c.runID, Rank: c.rank, ContainerSpec: specJSON,
		},
	}}
	if !c.nodes.Send(c.nodeID, message) {
		return nil, fmt.Errorf("node %s offline", c.nodeID)
	}
	result, release := c.commands.Wait(commandID)
	defer release()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response, ok := <-result:
		if !ok || response == nil {
			return nil, fmt.Errorf("workload command %s ended without acknowledgement", commandID)
		}
		return response, nil
	case <-timer.C:
		return nil, fmt.Errorf("timed out after %s waiting for %s ack", timeout, op)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
