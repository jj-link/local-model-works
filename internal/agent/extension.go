package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func (a *Agent) handleExtension(ctx context.Context, command *agentv1.ExtensionCommand) {
	if command.GetPhase() != "prepare" && command.GetPhase() != "verify" && command.GetPhase() != "stop" {
		a.extensionResult(command.GetCommandId(), false, 0, "extension phase is invalid", nil)
		return
	}
	if err := validateWorkloadIdentity(command.GetDeploymentId(), command.GetRunId(), command.GetRank()); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	key := fmt.Sprintf("%s|%s|%d", command.GetDeploymentId(), command.GetRunId(), command.GetRank())
	if command.GetPhase() == "stop" {
		a.extensionMu.Lock()
		a.extensionStopped[key] = true
		active := a.extensionRuns[key]
		if active != nil {
			active.cancel()
		}
		a.extensionMu.Unlock()
		if active != nil {
			select {
			case <-active.done:
			case <-ctx.Done():
				a.extensionResult(command.GetCommandId(), false, 0, ctx.Err().Error(), nil)
				return
			}
		}
		for _, phase := range []string{"prepare", "verify"} {
			name := containerName(command.GetDeploymentId(), command.GetRunId()+"-"+phase, command.GetRank())
			if _, err := a.rt.Inspect(ctx, name); err != nil {
				continue
			}
			_ = a.rt.Stop(ctx, name, 5)
			_ = a.rt.Remove(ctx, name, true)
		}
		a.extensionResult(command.GetCommandId(), true, 0, "", nil)
		return
	}
	var operationCtx context.Context
	var active *extensionRun
	for {
		a.extensionMu.Lock()
		if a.extensionStopped[key] {
			a.extensionMu.Unlock()
			a.extensionResult(command.GetCommandId(), false, 0, "extension was stopped", nil)
			return
		}
		existing := a.extensionRuns[key]
		if existing == nil {
			var cancel context.CancelFunc
			operationCtx, cancel = context.WithCancel(ctx)
			active = &extensionRun{cancel: cancel, done: make(chan struct{})}
			a.extensionRuns[key] = active
			a.extensionMu.Unlock()
			break
		}
		a.extensionMu.Unlock()
		select {
		case <-existing.done:
		case <-ctx.Done():
			a.extensionResult(command.GetCommandId(), false, 0, ctx.Err().Error(), nil)
			return
		}
	}
	defer func() {
		active.cancel()
		a.extensionMu.Lock()
		if a.extensionRuns[key] == active {
			delete(a.extensionRuns, key)
		}
		close(active.done)
		a.extensionMu.Unlock()
	}()
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(command.GetContainerSpec(), &spec); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	spec.Name = containerName(command.GetDeploymentId(), command.GetRunId()+"-"+command.GetPhase(), command.GetRank())
	if err := runtime.ValidateManagedSpec(&spec); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	workspace := filepath.Join(a.cfg.Workspace, command.GetRunId(), command.GetPhase(), fmt.Sprintf("rank-%d", command.GetRank()))
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	spec.Mounts = append(spec.Mounts, runtime.MountSpec{Source: workspace, Dest: "/lmw/workspace", ReadOnly: false})
	timeout := time.Duration(command.GetTimeoutSeconds()) * time.Second
	if timeout <= 0 || timeout > 2*time.Hour {
		timeout = 15 * time.Minute
	}
	execCtx, cancel := context.WithTimeout(operationCtx, timeout)
	defer cancel()
	if err := a.rt.Pull(execCtx, &runtime.PullSpec{Reference: runtime.ImageRef(&spec)}); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	id, err := a.rt.Create(execCtx, &spec)
	if err != nil {
		if _, inspectErr := a.rt.Inspect(execCtx, spec.Name); inspectErr != nil {
			a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
			return
		}
		_ = a.rt.Stop(execCtx, spec.Name, 5)
		_ = a.rt.Remove(execCtx, spec.Name, true)
		id, err = a.rt.Create(execCtx, &spec)
		if err != nil {
			a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
			return
		}
	}
	defer a.rt.Remove(context.Background(), id, true)
	if err := a.rt.Start(execCtx, id); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	var info *runtime.ContainerInfo
	for {
		info, err = a.rt.Inspect(execCtx, id)
		if err != nil {
			a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
			return
		}
		if info.State == "exited" || info.State == "dead" {
			break
		}
		select {
		case <-execCtx.Done():
			_ = a.rt.Stop(context.Background(), id, 5)
			a.extensionResult(command.GetCommandId(), false, 0, execCtx.Err().Error(), nil)
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
	limit := int64(command.GetOutputLimitBytes())
	if limit < 4096 || limit > 10<<20 {
		limit = 1 << 20
	}
	stdout, stderr, err := a.rt.LogsStreams(execCtx, id)
	if err != nil {
		a.extensionResult(command.GetCommandId(), false, int32(info.ExitCode), err.Error(), nil)
		return
	}
	output, outErr := io.ReadAll(io.LimitReader(stdout, limit+1))
	errorOutput, _ := io.ReadAll(io.LimitReader(stderr, limit+1))
	stdout.Close()
	stderr.Close()
	if outErr != nil || int64(len(output)) > limit || int64(len(errorOutput)) > limit {
		a.extensionResult(command.GetCommandId(), false, int32(info.ExitCode), "extension output exceeds limit", nil)
		return
	}
	output = bytes.TrimSpace(output)
	var result struct {
		Version int `json:"version"`
	}
	if info.ExitCode != 0 {
		a.extensionResult(command.GetCommandId(), false, int32(info.ExitCode), string(errorOutput), nil)
		return
	}
	if json.Unmarshal(output, &result) != nil || result.Version < 1 {
		a.extensionResult(command.GetCommandId(), false, 0, "extension output must be versioned JSON", nil)
		return
	}
	if err := validateExtensionOutput(command.GetOutputSchema(), output); err != nil {
		a.extensionResult(command.GetCommandId(), false, 0, err.Error(), nil)
		return
	}
	a.extensionResult(command.GetCommandId(), true, 0, "", output)
}

func validateExtensionOutput(schemaJSON, output []byte) error {
	if len(schemaJSON) == 0 {
		return fmt.Errorf("extension output schema is required")
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return fmt.Errorf("extension output schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("extension-output", schemaDoc); err != nil {
		return err
	}
	schema, err := compiler.Compile("extension-output")
	if err != nil {
		return err
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("extension output must be JSON: %w", err)
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("extension output schema: %w", err)
	}
	return nil
}

func (a *Agent) extensionResult(commandID string, ok bool, exitCode int32, message string, output []byte) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_CommandResult{
		CommandResult: &agentv1.CommandResult{
			CommandId: commandID, Ok: ok, ExitCode: exitCode, Error: message, OutputJson: output,
		},
	}})
}
