package backend

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/traces"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const workloadTraceSocket = "/run/lmw/trace.sock"

const (
	pullTimeout       = 30 * time.Minute
	commandTimeout    = 2 * time.Minute
	inspectInterval   = 3 * time.Second
	traceDrainTimeout = 2 * time.Minute
)

type workloadDispatch struct {
	env          *moduleapi.Env
	nodeID       string
	runID        string
	traceEnabled bool
	secrets      map[string]string
}

func (d *workloadDispatch) op(ctx context.Context, op agentv1.WorkloadOp, spec *runtime.ContainerSpec, timeout time.Duration) (*agentv1.CommandResult, error) {
	commandID, err := id.New()
	if err != nil {
		return nil, err
	}
	var specJSON []byte
	if spec != nil {
		specJSON, err = json.Marshal(spec)
		if err != nil {
			return nil, err
		}
	}
	command := &agentv1.WorkloadCommand{CommandId: commandID, Op: op, RunId: d.runID, Rank: 0, ContainerSpec: specJSON}
	if op == agentv1.WorkloadOp_WORKLOAD_OP_CREATE && d.traceEnabled {
		command.TraceEnabled = true
		command.TraceSchema = traces.SchemaVersion
		command.TraceSocket = workloadTraceSocket
		command.Secrets = make(map[string]string, len(d.secrets))
		for name, value := range d.secrets {
			command.Secrets[name] = value
		}
	}
	if !d.env.Nodes.Send(d.nodeID, &agentv1.ServerMessage{Body: &agentv1.ServerMessage_WorkloadCommand{WorkloadCommand: command}}) {
		return nil, fmt.Errorf("node %s offline", d.nodeID)
	}
	resultCh, release := d.env.Commands.Wait(commandID)
	defer release()
	select {
	case result := <-resultCh:
		if !result.GetOk() {
			return result, errors.New(result.GetError())
		}
		return result, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("workload %s timed out", op)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Module) runOrchestrator(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	experimentID, _ := c.Input["experiment_id"].(string)
	experiment, err := m.env.Q.GetSweGymExperiment(ctx, experimentID)
	if err != nil {
		return nil, err
	}
	var plan sweGymPlan
	if err := json.Unmarshal([]byte(experiment.Plan), &plan); err != nil {
		return nil, err
	}
	_, _ = m.env.Q.UpdateSweGymExperimentState(ctx, db.UpdateSweGymExperimentStateParams{State: "running", ID: experimentID})
	items, err := m.env.Q.ListSweGymWorkItems(ctx, experimentID)
	if err != nil {
		return nil, err
	}
	nodesRaw, _ := plan.NodeCapacity["eligible_nodes"].([]any)
	var nodeIDs []string
	for _, value := range nodesRaw {
		if node, ok := value.(string); ok {
			nodeIDs = append(nodeIDs, node)
		}
	}
	if len(nodeIDs) == 0 {
		if typed, ok := plan.NodeCapacity["eligible_nodes"].([]string); ok {
			nodeIDs = typed
		}
	}
	if len(nodeIDs) == 0 {
		return nil, fmt.Errorf("experiment has no eligible nodes")
	}
	workers := plan.Config.Workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(items) {
		workers = len(items)
	}
	queue := make(chan db.SweGymWorkItem)
	nodeLimits := make(map[string]chan struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodeLimits[nodeID] = make(chan struct{}, plan.Config.PerNodeWorkers)
	}
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range queue {
				if ctx.Err() != nil {
					return
				}
				nodeID := nodeIDs[int(item.RolloutIndex)%len(nodeIDs)]
				select {
				case nodeLimits[nodeID] <- struct{}{}:
				case <-ctx.Done():
					return
				}
				err := m.executeWorkItem(ctx, plan, item, nodeID)
				<-nodeLimits[nodeID]
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
				}
			}
		}()
	}
enqueue:
	for _, item := range items {
		if item.State == "queued" || item.State == "infrastructure_error" {
			select {
			case queue <- item:
			case <-ctx.Done():
				break enqueue
			}
		}
	}
	close(queue)
	wg.Wait()
	_ = m.env.Q.RecountSweGymExperiment(context.WithoutCancel(ctx), experimentID)
	if ctx.Err() != nil {
		_, _ = m.env.Q.FinishSweGymExperiment(context.WithoutCancel(ctx), db.FinishSweGymExperimentParams{State: "cancelled", ID: experimentID})
		return nil, ctx.Err()
	}
	if firstErr != nil {
		_, _ = m.env.Q.FinishSweGymExperiment(ctx, db.FinishSweGymExperimentParams{State: "failed", ID: experimentID})
		return nil, firstErr
	}
	exportID, _ := id.New()
	maxContextTokens, successCap := 32768, 2
	if settingsValue, _, settingsErr := m.env.Settings.Get(ctx, descriptor.ID); settingsErr == nil {
		maxContextTokens = number(settingsValue["export_max_context_tokens"], maxContextTokens)
		successCap = number(settingsValue["export_success_cap_per_task"], successCap)
	}
	selection := map[string]any{"experiment_id": experimentID, "seed": float64(plan.Config.Seed), "max_context_tokens": float64(maxContextTokens), "success_cap_per_task": float64(successCap), "tokenizer": "cl100k_base"}
	selectionJSON, _ := json.Marshal(selection)
	exportRun, err := m.env.Jobs.SubmitPrepared(ctx, "trace-export", map[string]any{"export_id": exportID, "selection": selection}, func(runID string) error {
		return m.env.Q.CreateCodingTraceExport(ctx, db.CreateCodingTraceExportParams{ID: exportID, RunID: runID, Selection: string(selectionJSON), Seed: plan.Config.Seed})
	})
	if err != nil {
		return nil, err
	}
	if _, err := waitRun(ctx, m.env.Runs, exportRun); err != nil {
		return nil, err
	}
	_, _ = m.env.Q.FinishSweGymExperiment(ctx, db.FinishSweGymExperimentParams{State: "completed", ID: experimentID})
	return map[string]any{"experiment_id": experimentID, "export_id": exportID, "completed_items": len(items)}, nil
}

func (m *Module) executeWorkItem(ctx context.Context, plan sweGymPlan, item db.SweGymWorkItem, nodeID string) error {
	task, sampling, err := workItemInputs(plan, item)
	if err != nil {
		return err
	}
	input := map[string]any{"experiment_id": item.ExperimentID, "work_item_id": item.ID, "task": task, "sampling": sampling, "config": plan.Config, "node_id": nodeID}
	for attempt := item.Attempt; attempt <= int64(plan.Config.RetryLimit); attempt++ {
		runID, err := m.env.Jobs.Submit(ctx, "swe-gym-rollout", input)
		if err != nil {
			return err
		}
		rows, err := m.env.Q.ClaimSweGymWorkItem(ctx, db.ClaimSweGymWorkItemParams{ChildRunID: sql.NullString{String: runID, Valid: true}, NodeID: sql.NullString{String: nodeID, Valid: true}, ID: item.ID})
		if err != nil || rows != 1 {
			_ = m.env.Jobs.Cancel(ctx, runID)
			return fmt.Errorf("work item claim conflict")
		}
		run, waitErr := waitRun(ctx, m.env.Runs, runID)
		state := "infrastructure_error"
		traceID := sql.NullString{}
		output := sql.NullString{}
		code := sql.NullString{}
		message := sql.NullString{}
		if waitErr == nil {
			if value, ok := run.Output["outcome"].(string); ok {
				state = value
			}
			if value, ok := run.Output["trace_id"].(string); ok {
				traceID = sql.NullString{String: value, Valid: true}
			}
			if data, err := json.Marshal(run.Output); err == nil {
				output = sql.NullString{String: string(data), Valid: true}
			}
		} else {
			code = sql.NullString{String: "run.failed", Valid: true}
			message = sql.NullString{String: waitErr.Error(), Valid: true}
			if trace, traceErr := m.env.Traces.GetByRun(context.WithoutCancel(ctx), runID); traceErr == nil {
				traceID = sql.NullString{String: trace.ID, Valid: true}
			}
			if errors.Is(waitErr, context.Canceled) {
				state = "cancelled"
			}
		}
		if _, err = m.env.Q.FinishSweGymWorkItem(context.WithoutCancel(ctx), db.FinishSweGymWorkItemParams{State: state, TraceID: traceID, Output: output, ErrorCode: code, ErrorMessage: message, ID: item.ID}); err != nil {
			return err
		}
		if state != "infrastructure_error" || attempt == int64(plan.Config.RetryLimit) {
			return nil
		}
		if rows, err = m.env.Q.SetSweGymWorkItemQueued(context.WithoutCancel(ctx), item.ID); err != nil || rows != 1 {
			return fmt.Errorf("work item retry conflict")
		}
	}
	return nil
}

func workItemInputs(plan sweGymPlan, item db.SweGymWorkItem) (sweGymTask, samplingSpec, error) {
	var task sweGymTask
	found := false
	for _, candidate := range plan.Tasks {
		if candidate.InstanceID == item.TaskID {
			task, found = candidate, true
			break
		}
	}
	if !found {
		return task, samplingSpec{}, fmt.Errorf("task missing from plan")
	}
	index := item.RolloutIndex
	for _, sampling := range plan.Sampling {
		if index < int64(sampling.Rollouts) {
			return task, sampling, nil
		}
		index -= int64(sampling.Rollouts)
	}
	return task, samplingSpec{}, fmt.Errorf("rollout index outside sampling matrix")
}

func (m *Module) runRollout(ctx context.Context, c *jobs.Context) (_ map[string]any, retErr error) {
	var task sweGymTask
	var sampling samplingSpec
	var config sweGymConfig
	if err := remarshal(c.Input["task"], &task); err != nil {
		return nil, err
	}
	if err := remarshal(c.Input["sampling"], &sampling); err != nil {
		return nil, err
	}
	if err := remarshal(c.Input["config"], &config); err != nil {
		return nil, err
	}
	nodeID, _ := c.Input["node_id"].(string)
	if nodeID == "" || !m.env.Nodes.Online(nodeID) {
		return nil, fmt.Errorf("rollout node offline")
	}
	var retainUntil *time.Time
	if settingsValue, _, settingsErr := m.env.Settings.Get(ctx, descriptor.ID); settingsErr == nil {
		if retentionDays := number(settingsValue["retention_days"], 0); retentionDays > 0 {
			deadline := time.Now().UTC().Add(time.Duration(retentionDays) * 24 * time.Hour)
			retainUntil = &deadline
		}
	}
	secretValues := make([]string, 0, len(c.Secrets))
	for _, value := range c.Secrets {
		secretValues = append(secretValues, value)
	}
	recorder, err := m.env.Traces.Start(ctx, traces.StartInput{RunID: c.RunID, ExperimentID: fmt.Sprint(c.Input["experiment_id"]), TaskID: task.InstanceID, Problem: task.ProblemStatement, Repository: task.Repo, BaseRevision: task.BaseCommit, ModelSource: config.ModelSource, Model: sampling.Model, Sampling: sampling, RetainUntil: retainUntil, SecretValues: secretValues})
	if err != nil {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			if _, streamErr := m.env.Q.GetCodingTraceStream(context.Background(), db.GetCodingTraceStreamParams{TraceID: recorder.TraceID(), NodeID: nodeID, Rank: 0, Source: "openhands"}); streamErr == nil {
				drainCtx, cancel := context.WithTimeout(context.Background(), traceDrainTimeout)
				_ = waitTraceEOF(drainCtx, m.env.Q, recorder.TraceID(), nodeID)
				cancel()
			}
			_ = recorder.Interrupt(context.Background(), infrastructureKind(retErr))
		}
	}()
	taskData, _ := json.Marshal(task)
	configData, _ := json.Marshal(config)
	samplingData, _ := json.Marshal(sampling)
	spec := &runtime.ContainerSpec{Image: task.Image, ImageDigest: task.ImageDigest, Cmd: []string{"python3", "-c", runnerBootstrap, base64.StdEncoding.EncodeToString(taskData), base64.StdEncoding.EncodeToString(configData), base64.StdEncoding.EncodeToString(samplingData)}, NetworkMode: "host", NoNewPrivileges: true, CapDrop: []string{"ALL"}, PidsLimit: 2048, MemoryBytes: 16 << 30, TmpfsBytes: 4 << 30, Labels: runtime.ManagedLabels("", c.RunID, "", "", 0, descriptor.ID)}
	dispatch := &workloadDispatch{env: m.env, nodeID: nodeID, runID: c.RunID, traceEnabled: true, secrets: c.Secrets}
	result, err := executeContainer(ctx, dispatch, spec, time.Duration(config.TimeoutSeconds)*time.Second, m.env.Runs)
	if err != nil {
		return nil, err
	}
	var runnerResult struct {
		GitPatch            string `json:"git_patch"`
		Error               string `json:"error"`
		InfrastructureError bool   `json:"infrastructure_error"`
	}
	if err := parseResult(result.logs, "LMW_RESULT:", &runnerResult); err != nil {
		return nil, err
	}
	if runnerResult.InfrastructureError {
		return nil, fmt.Errorf("OpenHands runner: %s", runnerResult.Error)
	}
	if err := waitTraceEOF(ctx, m.env.Q, recorder.TraceID(), nodeID); err != nil {
		return nil, err
	}
	gradeRun, err := m.env.Jobs.Submit(ctx, "swe-gym-grade", map[string]any{"trace_id": recorder.TraceID(), "task": task, "patch": runnerResult.GitPatch, "node_id": nodeID, "timeout_seconds": config.TimeoutSeconds})
	if err != nil {
		return nil, err
	}
	graded, err := waitRun(ctx, m.env.Runs, gradeRun)
	if err != nil {
		return nil, err
	}
	verification, err := verificationFromOutput(graded.Output, int64(config.TimeoutSeconds))
	if err != nil {
		return nil, err
	}
	if err := recorder.Finalize(ctx, traces.FinalizeInput{FinalDiff: runnerResult.GitPatch, Verification: verification}); err != nil {
		return nil, err
	}
	finished = true
	return map[string]any{"trace_id": recorder.TraceID(), "outcome": verification.Status, "grade_run_id": gradeRun, "patch_empty": strings.TrimSpace(runnerResult.GitPatch) == ""}, nil
}

type containerExecution struct {
	logs     []byte
	exitCode int32
}

func executeContainer(ctx context.Context, dispatch *workloadDispatch, spec *runtime.ContainerSpec, cap time.Duration, runSvc *runs.Service) (containerExecution, error) {
	if _, err := dispatch.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_PULL, spec, pullTimeout); err != nil {
		return containerExecution{}, err
	}
	created := false
	removed := false
	defer func() {
		if created && !removed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _ = dispatch.op(cleanupCtx, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, commandTimeout)
			_, _ = dispatch.op(cleanupCtx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, commandTimeout)
		}
	}()
	if _, err := dispatch.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, spec, commandTimeout); err != nil {
		return containerExecution{}, err
	}
	created = true
	if _, err := dispatch.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, commandTimeout); err != nil {
		return containerExecution{}, err
	}
	deadline := time.NewTimer(cap)
	defer deadline.Stop()
	ticker := time.NewTicker(inspectInterval)
	defer ticker.Stop()
	var terminal *agentv1.CommandResult
	for terminal == nil {
		select {
		case <-ctx.Done():
			return containerExecution{}, ctx.Err()
		case <-deadline.C:
			return containerExecution{}, fmt.Errorf("container timeout after %s", cap)
		case <-ticker.C:
			result, err := dispatch.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, commandTimeout)
			if err != nil {
				return containerExecution{}, err
			}
			if result.GetContainerState() != "running" {
				terminal = result
			}
		}
	}
	if _, err := runSvc.WaitLogEnd(ctx, dispatch.runID, "", 0, "stdout"); err != nil {
		return containerExecution{}, err
	}
	logs, _, _, err := runSvc.ReadLog(dispatch.runID, "", 0, "stdout", 0, 16<<20)
	if err != nil {
		return containerExecution{}, err
	}
	if _, err := dispatch.op(ctx, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, commandTimeout); err != nil {
		return containerExecution{}, err
	}
	removed = true
	if terminal.GetExitCode() != 0 {
		return containerExecution{}, fmt.Errorf("container exited %d", terminal.GetExitCode())
	}
	return containerExecution{logs: logs, exitCode: terminal.GetExitCode()}, nil
}

func (m *Module) runGrade(ctx context.Context, c *jobs.Context) (map[string]any, error) {
	var task sweGymTask
	if err := remarshal(c.Input["task"], &task); err != nil {
		return nil, err
	}
	patch, _ := c.Input["patch"].(string)
	nodeID, _ := c.Input["node_id"].(string)
	timeout := number(c.Input["timeout_seconds"], 1800)
	taskData, _ := json.Marshal(task)
	spec := &runtime.ContainerSpec{Image: task.Image, ImageDigest: task.ImageDigest, Cmd: []string{"python3", "-c", graderBootstrap, base64.StdEncoding.EncodeToString(taskData), base64.StdEncoding.EncodeToString([]byte(patch)), strconv.Itoa(timeout)}, NetworkMode: "host", NoNewPrivileges: true, CapDrop: []string{"ALL"}, PidsLimit: 2048, MemoryBytes: 16 << 30, TmpfsBytes: 4 << 30, Labels: runtime.ManagedLabels("", c.RunID, "", "", 0, descriptor.ID)}
	result, err := executeContainer(ctx, &workloadDispatch{env: m.env, nodeID: nodeID, runID: c.RunID}, spec, time.Duration(timeout+900)*time.Second, m.env.Runs)
	if err != nil {
		return nil, err
	}
	var output map[string]any
	if err := parseResult(result.logs, "LMW_GRADE_RESULT:", &output); err != nil {
		return nil, err
	}
	reportPath := "swe-gym-report.json"
	reportData, _ := json.Marshal(output)
	if err := os.WriteFile(filepath.Join(c.Workspace, reportPath), reportData, 0o600); err != nil {
		return nil, err
	}
	artifact, err := c.PublishArtifact("swe-gym-report", reportPath)
	if err != nil {
		return nil, err
	}
	output["report_artifact"] = artifact
	return output, nil
}

func waitRun(ctx context.Context, service *runs.Service, runID string) (runs.Run, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := service.Get(ctx, runID)
		if err != nil {
			return runs.Run{}, err
		}
		if runs.State(run.State).Terminal() {
			if run.State != string(runs.Succeeded) {
				if run.ErrorMessage != nil {
					return run, errors.New(*run.ErrorMessage)
				}
				return run, fmt.Errorf("run %s", run.State)
			}
			return run, nil
		}
		select {
		case <-ctx.Done():
			return runs.Run{}, ctx.Err()
		case <-ticker.C:
		}
	}
}
func waitTraceEOF(ctx context.Context, q *db.Queries, traceID, nodeID string) error {
	deadline := time.NewTimer(traceDrainTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		stream, err := q.GetCodingTraceStream(ctx, db.GetCodingTraceStreamParams{TraceID: traceID, NodeID: nodeID, Rank: 0, Source: "openhands"})
		if err == nil && stream.EofAcknowledged == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("trace EOF not acknowledged")
		case <-ticker.C:
		}
	}
}
func parseResult(data []byte, marker string, target any) error {
	text := string(data)
	index := strings.LastIndex(text, marker)
	if index < 0 {
		return fmt.Errorf("result marker %s missing", marker)
	}
	line := text[index+len(marker):]
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = line[:newline]
	}
	return json.Unmarshal([]byte(line), target)
}
func remarshal(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func infrastructureKind(err error) string {
	if err == nil {
		return "runner_incomplete"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "infrastructure_error"
}
func verificationFromOutput(output map[string]any, timeoutSeconds int64) (traces.Verification, error) {
	status, _ := output["status"].(string)
	if status == "" {
		return traces.Verification{}, fmt.Errorf("grade output missing status")
	}
	stdout, _ := output["stdout"].(string)
	stderr, _ := output["stderr"].(string)
	failureKind, _ := output["failure_kind"].(string)
	verification := traces.Verification{Command: "/tmp/eval.sh", TimeoutSeconds: timeoutSeconds, Stdout: stdout, Stderr: stderr, Status: status, FailureKind: failureKind, FailToPassReport: output["fail_to_pass_report"], PassToPassReport: output["pass_to_pass_report"]}
	if value, ok := output["exit_status"].(float64); ok {
		exit := int64(value)
		verification.ExitStatus = &exit
	}
	return verification, nil
}
