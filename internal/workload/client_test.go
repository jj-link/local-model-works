package workload

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/nodes"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestDoSendsExactIdentityAndWaitsForAck(t *testing.T) {
	registry := nodes.NewRegistry()
	connection := nodes.NewConn("node-1")
	registry.Register(connection)
	broker := commands.New()
	client := New(registry, broker, "node-1", "", "run-1", 0)
	done := make(chan *agentv1.CommandResult, 1)
	errs := make(chan error, 1)
	go func() {
		result, err := client.Do(context.Background(), agentv1.WorkloadOp_WORKLOAD_OP_PAUSE, nil, time.Second)
		done <- result
		errs <- err
	}()
	message := <-connection.SendCh
	command := message.GetWorkloadCommand()
	if command == nil || command.GetRunId() != "run-1" || command.GetDeploymentId() != "" || command.GetRank() != 0 || command.GetOp() != agentv1.WorkloadOp_WORKLOAD_OP_PAUSE {
		t.Fatalf("command = %+v", command)
	}
	broker.Deliver(&agentv1.CommandResult{CommandId: command.GetCommandId(), Ok: true, ContainerState: "paused"})
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if result := <-done; !result.GetOk() || result.GetContainerState() != "paused" {
		t.Fatalf("result = %+v", result)
	}
}

func TestDoRejectsInvalidAndOfflineClients(t *testing.T) {
	if _, err := (*Client)(nil).Do(context.Background(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, time.Second); err == nil || err.Error() != "workload.client_invalid" {
		t.Fatalf("nil client error = %v", err)
	}
	client := New(nodes.NewRegistry(), commands.New(), "offline", "", "run-1", 0)
	if _, err := client.Do(context.Background(), agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, time.Second); err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("offline error = %v", err)
	}
}
