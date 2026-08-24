//go:build integration

package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/jj-link/local-model-works/internal/commands"
	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/id"
	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestSweGymGradeUsesAgentWorkloadPath(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(root, "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	queries := db.New(sqlDB)
	runsService := runs.New(sqlDB, queries, events.NewEventBus(queries), root)
	runID, err := runsService.Create(ctx, "coding-traces", "swe-gym-grade", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	nodeRegistry := nodes.NewRegistry()
	broker := commands.New()
	connection := nodes.NewConn("node-1")
	nodeRegistry.Register(connection)
	defer connection.Close()
	module := &Module{env: &moduleapi.Env{Q: queries, DB: sqlDB, Runs: runsService, Nodes: nodeRegistry, Commands: broker}}

	gradeResult := map[string]any{
		"status": "resolved", "failure_kind": nil, "exit_status": 0,
		"stdout": "all tests passed", "stderr": "", "report": map[string]any{"resolved": true},
		"fail_to_pass_report": map[string]any{"success": []string{"test_fix"}},
		"pass_to_pass_report": map[string]any{"success": []string{"test_existing"}},
	}
	encoded, _ := json.Marshal(gradeResult)
	var operationsMu sync.Mutex
	var operations []agentv1.WorkloadOp
	go func() {
		for message := range connection.SendCh {
			command := message.GetWorkloadCommand()
			if command == nil {
				continue
			}
			operationsMu.Lock()
			operations = append(operations, command.Op)
			operationsMu.Unlock()
			if command.Op == agentv1.WorkloadOp_WORKLOAD_OP_START {
				log := append([]byte("LMW_GRADE_RESULT:"), encoded...)
				log = append(log, '\n')
				_ = runsService.AppendLog(runID, "", 0, "stdout", log)
				runsService.MarkLogEnd(runID, "", 0, "stdout", uint64(len(log)))
			}
			result := &agentv1.CommandResult{CommandId: command.CommandId, Ok: true, ContainerId: "container-1"}
			if command.Op == agentv1.WorkloadOp_WORKLOAD_OP_INSPECT {
				result.ContainerState = "exited"
			}
			broker.Deliver(result)
		}
	}()

	workspace := filepath.Join(root, "jobs", runID)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	task := sweGymTask{
		InstanceID: "getmoto__moto-5752", Repo: "getmoto/moto", BaseCommit: "abc", Version: "4.0",
		ProblemStatement: "repair moto", FailToPass: []string{"test_fix"}, PassToPass: []string{"test_existing"},
		Image: "docker.io/xingyaoww/sweb.eval.x86_64.getmoto_s_moto-5752", ImageDigest: "sha256:2665296135d73d6ed10e225ac3942fcc9086811fdfa0ede89a8465a63716c134",
	}
	output, err := module.runGrade(ctx, &jobs.Context{
		RunID: runID, Workspace: workspace,
		Input: map[string]any{"trace_id": "trace-1", "task": task, "patch": "diff --git a/a b/a", "node_id": "node-1", "timeout_seconds": float64(1800)},
		PublishArtifact: func(kind, path string) (jobs.PublishedArtifact, error) {
			info, statErr := os.Stat(filepath.Join(workspace, path))
			if statErr != nil {
				return jobs.PublishedArtifact{}, statErr
			}
			return jobs.PublishedArtifact{ID: "artifact-1", Kind: kind, Path: path, Size: info.Size()}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if output["status"] != "resolved" {
		t.Fatalf("grade output = %#v", output)
	}
	operationsMu.Lock()
	gotOperations := append([]agentv1.WorkloadOp(nil), operations...)
	operationsMu.Unlock()
	wantOperations := []agentv1.WorkloadOp{
		agentv1.WorkloadOp_WORKLOAD_OP_PULL,
		agentv1.WorkloadOp_WORKLOAD_OP_CREATE,
		agentv1.WorkloadOp_WORKLOAD_OP_START,
		agentv1.WorkloadOp_WORKLOAD_OP_INSPECT,
		agentv1.WorkloadOp_WORKLOAD_OP_REMOVE,
	}
	if !reflect.DeepEqual(gotOperations, wantOperations) {
		t.Fatalf("workload operations = %v, want %v", gotOperations, wantOperations)
	}
}

func TestSweGymCancelResume(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "lmw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	queries := db.New(sqlDB)
	experimentID, _ := id.New()
	if err := queries.CreateSweGymExperiment(ctx, db.CreateSweGymExperimentParams{ID: experimentID, Config: "{}", ConfigDigest: "sha256:config", Plan: "{}", PlanDigest: "sha256:plan", Manifest: "{}", TotalItems: 2}); err != nil {
		t.Fatal(err)
	}
	resolvedID, _ := id.New()
	retryID, _ := id.New()
	for index, itemID := range []string{resolvedID, retryID} {
		if err := queries.CreateSweGymWorkItem(ctx, db.CreateSweGymWorkItemParams{ID: itemID, ExperimentID: experimentID, TaskID: "task", RolloutIndex: int64(index)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := queries.FinishSweGymWorkItem(ctx, db.FinishSweGymWorkItemParams{State: "resolved", ID: resolvedID}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.FinishSweGymWorkItem(ctx, db.FinishSweGymWorkItemParams{State: "cancelled", ID: retryID}); err != nil {
		t.Fatal(err)
	}
	if _, err := queries.SetSweGymWorkItemQueued(ctx, retryID); err != nil {
		t.Fatal(err)
	}
	items, err := queries.ListSweGymWorkItems(ctx, experimentID)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, item := range items {
		states[item.ID] = item.State
	}
	if states[resolvedID] != "resolved" || states[retryID] != "queued" {
		t.Fatalf("resume states = %v", states)
	}
	if strings.TrimSpace(states[resolvedID]) == "queued" {
		t.Fatal("resume duplicated successful work")
	}
}
