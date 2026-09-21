package agent

import (
	"context"
	"encoding/json"

	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/workerssh"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func (a *Agent) handleWorkerSSH(ctx context.Context, command *agentv1.WorkerSSHCommand) {
	if command == nil || command.GetCommandId() == "" {
		return
	}
	result := &agentv1.CommandResult{CommandId: command.GetCommandId()}
	if !runtime.UpstreamEnabled(a.rt) {
		result.Error = "worker SSH checks require the head agent administrator's host-execution opt-in"
	} else {
		resolved, err := workerssh.Check(ctx, workerssh.Request{
			Addresses: command.GetAddresses(), Username: command.GetUsername(),
			Target: command.GetTarget(), ResolveOnly: command.GetResolveOnly(),
		})
		if err == nil {
			result.OutputJson, err = json.Marshal(resolved)
		}
		result.Ok = err == nil
		if err != nil {
			result.Error = err.Error()
		}
	}
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_CommandResult{CommandResult: result}})
}
