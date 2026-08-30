package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

type ownershipRuntime struct {
	info    *runtime.ContainerInfo
	list    []runtime.ContainerInfo
	calls   []string
	created bool
}

func (r *ownershipRuntime) Ping(context.Context) (string, error) { return "test", nil }
func (r *ownershipRuntime) Pull(context.Context, *runtime.PullSpec) error {
	r.calls = append(r.calls, "pull")
	return nil
}
func (r *ownershipRuntime) PrepareHost(context.Context, *runtime.ContainerSpec) error {
	r.calls = append(r.calls, "host-prepare")
	return nil
}
func (r *ownershipRuntime) Create(context.Context, *runtime.ContainerSpec) (string, error) {
	r.calls = append(r.calls, "create")
	r.created = true
	return "created", nil
}
func (r *ownershipRuntime) Start(context.Context, string) error {
	r.calls = append(r.calls, "start")
	return nil
}
func (r *ownershipRuntime) Stop(context.Context, string, int) error {
	r.calls = append(r.calls, "stop")
	return nil
}
func (r *ownershipRuntime) Remove(context.Context, string, bool) error {
	r.calls = append(r.calls, "remove")
	return nil
}
func (r *ownershipRuntime) Inspect(context.Context, string) (*runtime.ContainerInfo, error) {
	r.calls = append(r.calls, "inspect")
	if r.info == nil {
		return nil, errors.New("missing")
	}
	copy := *r.info
	return &copy, nil
}
func (r *ownershipRuntime) ListByLabel(context.Context, string, string) ([]runtime.ContainerInfo, error) {
	return r.list, nil
}
func (r *ownershipRuntime) LogsFollow(context.Context, string, bool, bool) (io.ReadCloser, error) {
	r.calls = append(r.calls, "logs-follow")
	return io.NopCloser(strings.NewReader("")), nil
}
func (r *ownershipRuntime) LogsStreams(context.Context, string) (io.ReadCloser, io.ReadCloser, error) {
	r.calls = append(r.calls, "logs-streams")
	return io.NopCloser(strings.NewReader("")), io.NopCloser(strings.NewReader("")), nil
}

func testWorkloadCommand(op agentv1.WorkloadOp, spec []byte) *agentv1.WorkloadCommand {
	return &agentv1.WorkloadCommand{
		CommandId:     "command",
		DeploymentId:  "deployment-1234",
		RunId:         "run-1234",
		Rank:          0,
		Op:            op,
		ContainerSpec: spec,
	}
}

func commandResult(t *testing.T, a *Agent) *agentv1.CommandResult {
	t.Helper()
	select {
	case message := <-a.sendQ:
		result := message.GetCommandResult()
		if result == nil {
			t.Fatalf("message = %T, want command result", message.GetBody())
		}
		return result
	default:
		t.Fatal("missing command result")
		return nil
	}
}

func TestHostPreparationUsesManagedBoundedSpec(t *testing.T) {
	swappiness := 0
	encoded, err := json.Marshal(runtime.ContainerSpec{
		Labels: runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving"),
		HostPreparation: &runtime.HostPreparationSpec{
			RequireSwap: true, Swappiness: &swappiness, DropPageCache: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &ownershipRuntime{}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", fake, nil)
	a.handleWorkload(context.Background(), testWorkloadCommand(agentv1.WorkloadOp_WORKLOAD_OP_HOST_PREPARE, encoded))
	result := commandResult(t, a)
	if !result.GetOk() || len(fake.calls) != 1 || fake.calls[0] != "host-prepare" {
		t.Fatalf("result=%+v calls=%v", result, fake.calls)
	}
}

func TestHostPreparationHelperIsExcludedFromWorkloadState(t *testing.T) {
	labels := runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving")
	labels[runtime.LabelModule] = runtime.HostPreparationModule
	fake := &ownershipRuntime{list: []runtime.ContainerInfo{{
		ID: "host-helper", State: "running", Labels: labels,
	}}}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", fake, nil)

	a.workloads.tick(t.Context())
	fake.list = nil
	a.workloads.tick(t.Context())

	select {
	case message := <-a.sendQ:
		t.Fatalf("host helper emitted workload state: %+v", message)
	default:
	}
	a.workloads.mu.Lock()
	defer a.workloads.mu.Unlock()
	if len(a.workloads.last) != 0 {
		t.Fatalf("tracked workload states = %+v, want none", a.workloads.last)
	}
}

func TestUnmanagedSameNameRejectedAcrossLifecycle(t *testing.T) {
	ops := []agentv1.WorkloadOp{
		agentv1.WorkloadOp_WORKLOAD_OP_CREATE,
		agentv1.WorkloadOp_WORKLOAD_OP_START,
		agentv1.WorkloadOp_WORKLOAD_OP_STOP,
		agentv1.WorkloadOp_WORKLOAD_OP_REMOVE,
		agentv1.WorkloadOp_WORKLOAD_OP_INSPECT,
		agentv1.WorkloadOp_WORKLOAD_OP_LOGS,
	}
	for _, op := range ops {
		t.Run(op.String(), func(t *testing.T) {
			fake := &ownershipRuntime{info: &runtime.ContainerInfo{ID: "unmanaged-id", State: "running", Labels: map[string]string{}}}
			a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", fake, nil)
			var spec []byte
			if op == agentv1.WorkloadOp_WORKLOAD_OP_CREATE {
				encoded, err := json.Marshal(runtime.ContainerSpec{
					Image:  "example@sha256:" + strings.Repeat("a", 64),
					Labels: runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving"),
				})
				if err != nil {
					t.Fatal(err)
				}
				spec = encoded
			}
			a.handleWorkload(context.Background(), testWorkloadCommand(op, spec))
			result := commandResult(t, a)
			if result.GetOk() || !strings.Contains(result.GetError(), "container.unmanaged") {
				t.Fatalf("result = %+v", result)
			}
			for _, call := range fake.calls {
				if call != "inspect" {
					t.Fatalf("unmanaged container was touched: calls=%v", fake.calls)
				}
			}
		})
	}
}

func TestManagedIdentityMismatchRejected(t *testing.T) {
	fake := &ownershipRuntime{info: &runtime.ContainerInfo{
		ID: "managed-other", State: "running",
		Labels: runtime.ManagedLabels("another-deployment", "run-1234", "recipe", "1.0.0", 0, "serving"),
	}}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", fake, nil)
	a.handleWorkload(context.Background(), testWorkloadCommand(agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil))
	result := commandResult(t, a)
	if result.GetOk() || !strings.Contains(result.GetError(), "container.identity_mismatch") {
		t.Fatalf("result = %+v", result)
	}
	if len(fake.calls) != 1 || fake.calls[0] != "inspect" {
		t.Fatalf("identity-mismatched container was touched: %v", fake.calls)
	}
}

func TestWorkloadIdentityRejectsTraversalAndCollisions(t *testing.T) {
	invalid := [][2]string{{"../escape", "run"}, {"deployment", "a/b"}, {"", ""}}
	for _, ids := range invalid {
		if err := validateWorkloadIdentity(ids[0], ids[1], 0); err == nil {
			t.Fatalf("identity (%q, %q) accepted", ids[0], ids[1])
		}
	}
	if err := validateWorkloadIdentity("deployment", "", 0); err != nil {
		t.Fatalf("deployment-only identity: %v", err)
	}
	if err := validateWorkloadIdentity("", "run", 0); err != nil {
		t.Fatalf("run-only identity: %v", err)
	}
	if err := validateWorkloadIdentity("deployment", "run", -1); err == nil {
		t.Fatal("negative rank accepted")
	}
}

func TestDirectLogRequestRejectsTraversalBeforeRuntimeOrFilesystem(t *testing.T) {
	fake := &ownershipRuntime{info: &runtime.ContainerInfo{
		ID: "managed", State: "running",
		Labels: runtime.ManagedLabels("deployment", "run", "recipe", "1.0.0", 0, "serving"),
	}}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", fake, nil)
	a.handleLogRequest(context.Background(), &agentv1.LogRequest{
		DeploymentId: "../escape",
		RunId:        "run",
		Rank:         0,
		Stream:       "stdout",
	})
	if len(fake.calls) != 0 {
		t.Fatalf("invalid direct log request reached runtime: %v", fake.calls)
	}
	select {
	case message := <-a.sendQ:
		t.Fatalf("invalid direct log request emitted message: %T", message.GetBody())
	default:
	}
}

func TestShortIDUsesFullIdentity(t *testing.T) {
	first := shortID("abcdefgh-one")
	second := shortID("abcdefgh-two")
	if first == second {
		t.Fatalf("distinct identities collide: %q", first)
	}
	if strings.ContainsAny(first+second, `/\.`) {
		t.Fatalf("short IDs are not path-safe: %q %q", first, second)
	}
}

func TestExitedStateCarriesCrashMetadata(t *testing.T) {
	info := &runtime.ContainerInfo{
		ID:        "container-exited",
		State:     "exited",
		ExitCode:  137,
		Error:     "container runtime failure",
		OOMKilled: true,
		Labels:    runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving"),
	}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", &ownershipRuntime{info: info}, nil)

	a.workloads.reportState(info)

	select {
	case message := <-a.sendQ:
		update := message.GetStateUpdate()
		if update == nil {
			t.Fatalf("message = %T, want state update", message.GetBody())
		}
		if update.GetDeploymentId() != "deployment-1234" || update.GetContainerId() != info.ID ||
			update.GetState() != "exited" || update.GetRank() != 0 {
			t.Fatalf("identity/state = %+v", update)
		}
		if update.GetExitCode() != 137 || !update.GetOomKilled() ||
			update.GetDiagnosticMessage() != info.Error {
			t.Fatalf("crash metadata = %+v", update)
		}
	default:
		t.Fatal("missing state update")
	}
}

func TestRunningStateRefreshesWithoutTransition(t *testing.T) {
	info := &runtime.ContainerInfo{
		ID:     "container-running",
		State:  "running",
		Labels: runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving"),
	}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", &ownershipRuntime{info: info}, nil)

	a.workloads.reportState(info)
	<-a.sendQ

	a.workloads.mu.Lock()
	last := a.workloads.last[info.ID]
	last.reportedAt = time.Now().Add(-stateRefreshPeriod)
	a.workloads.last[info.ID] = last
	a.workloads.mu.Unlock()

	a.workloads.reportState(info)
	select {
	case message := <-a.sendQ:
		if update := message.GetStateUpdate(); update == nil || update.GetState() != "running" {
			t.Fatalf("refresh message = %+v, want running state update", message)
		}
	default:
		t.Fatal("missing periodic running state refresh")
	}
}

func TestSuccessfulExitedInspectCarriesCrashMetadata(t *testing.T) {
	info := &runtime.ContainerInfo{
		ID:        "container-exited",
		State:     "exited",
		ExitCode:  137,
		Error:     "container runtime failure",
		OOMKilled: true,
		Labels:    runtime.ManagedLabels("deployment-1234", "run-1234", "recipe", "1.0.0", 0, "serving"),
	}
	a := New(config.Agent{StateRoot: t.TempDir()}, "test", "test", &ownershipRuntime{info: info}, nil)

	a.handleWorkload(context.Background(), testWorkloadCommand(agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil))

	result := commandResult(t, a)
	if !result.GetOk() || result.GetContainerId() != info.ID || result.GetContainerState() != "exited" {
		t.Fatalf("inspect identity/state = %+v", result)
	}
	if result.GetExitCode() != 137 || !result.GetOomKilled() || result.GetError() != info.Error {
		t.Fatalf("inspect crash metadata = %+v", result)
	}
}
