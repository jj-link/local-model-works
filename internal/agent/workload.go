package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/runtime"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// workloads owns the agent's container lifecycle state: which managed
// containers exist, their specs (for endpoint/log derivation), and the
// active log tailers.
type workloads struct {
	a *Agent

	mu            sync.Mutex
	monitorCtx    context.Context
	monitorCancel context.CancelFunc
	specs         map[string]*runtime.ContainerSpec // container name -> spec
	last          map[string]containerState         // container ID -> last reported state
	tailers       map[tailKey]*tailer
}

type containerState struct {
	state    string
	reported bool
	dep      string
	run      string
	rank     int32
}

type tailKey struct {
	runID        string
	deploymentID string
	rank         int32
}

// newWorkloads builds the workload state holder.
func newWorkloads(a *Agent) *workloads {
	return &workloads{
		a:       a,
		specs:   map[string]*runtime.ContainerSpec{},
		last:    map[string]containerState{},
		tailers: map[tailKey]*tailer{},
	}
}

var workloadID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// containerName is the deterministic name for one workload rank.
func containerName(deploymentID, runID string, rank int32) string {
	name := "lmw-" + shortID(deploymentID)
	if runID != "" {
		name += "-" + shortID(runID)
	}
	return fmt.Sprintf("%s-r%d", name, rank)
}
func shortID(id string) string {
	if id == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

func validateWorkloadIdentity(deploymentID, runID string, rank int32) error {
	if (deploymentID == "" && runID == "") ||
		(deploymentID != "" && !workloadID.MatchString(deploymentID)) ||
		(runID != "" && !workloadID.MatchString(runID)) ||
		rank < 0 {
		return fmt.Errorf("workload.identity_invalid")
	}
	return nil
}

func validateContainerIdentity(labels map[string]string, deploymentID, runID string, rank int32) error {
	if labels[runtime.LabelManaged] != "true" {
		return fmt.Errorf("container.unmanaged")
	}
	if labels[runtime.LabelDeployment] != deploymentID ||
		labels[runtime.LabelRun] != runID ||
		labels[runtime.LabelRank] != fmt.Sprint(rank) {
		return fmt.Errorf("container.identity_mismatch")
	}
	return nil
}

func validateSpecIdentity(spec *runtime.ContainerSpec, deploymentID, runID string, rank int32) error {
	if err := runtime.ValidateManagedSpec(spec); err != nil {
		return err
	}
	return validateContainerIdentity(spec.Labels, deploymentID, runID, rank)
}

// handleWorkload executes one container lifecycle command and reports the
// result.
func (a *Agent) handleWorkload(ctx context.Context, wc *agentv1.WorkloadCommand) {
	w := a.workloads
	cmdID := wc.GetCommandId()
	deploymentID, runID := wc.GetDeploymentId(), wc.GetRunId()
	if err := validateWorkloadIdentity(deploymentID, runID, wc.GetRank()); err != nil {
		a.result(cmdID, false, 0, err.Error(), "", "")
		return
	}
	var spec *runtime.ContainerSpec
	if len(wc.GetContainerSpec()) > 0 {
		var s runtime.ContainerSpec
		if err := json.Unmarshal(wc.GetContainerSpec(), &s); err != nil {
			a.result(cmdID, false, 0, fmt.Sprintf("container spec: %v", err), "", "")
			return
		}
		spec = &s
	}
	name := containerName(deploymentID, runID, wc.GetRank())
	if spec != nil {
		spec.Name = name
		w.mu.Lock()
		w.specs[name] = spec
		w.mu.Unlock()
	}

	switch wc.GetOp() {
	case agentv1.WorkloadOp_WORKLOAD_OP_PULL:
		if spec == nil {
			a.result(cmdID, false, 0, "pull requires a container spec", "", "")
			return
		}
		if err := a.rt.Pull(ctx, &runtime.PullSpec{Reference: runtime.ImageRef(spec)}); err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		a.result(cmdID, true, 0, "", "", "")
	case agentv1.WorkloadOp_WORKLOAD_OP_CREATE:
		if spec == nil {
			a.result(cmdID, false, 0, "create requires a container spec", "", "")
			return
		}
		if err := validateSpecIdentity(spec, deploymentID, runID, wc.GetRank()); err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}

		if info, err := a.rt.Inspect(ctx, name); err == nil {
			if identityErr := validateContainerIdentity(info.Labels, deploymentID, runID, wc.GetRank()); identityErr != nil {
				a.result(cmdID, false, 0, identityErr.Error(), "", "")
				return
			}
			a.result(cmdID, false, 0, "container.exists: "+name, "", "")
			return
		}
		id, err := a.rt.Create(ctx, spec)
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		a.result(cmdID, true, 0, "", id, "")
	case agentv1.WorkloadOp_WORKLOAD_OP_START:
		id, err := a.resolve(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		if err := a.rt.Start(ctx, id); err != nil {
			a.result(cmdID, false, 0, err.Error(), id, "")
			return
		}
		a.result(cmdID, true, 0, "", id, "")
		w.startTailer(ctx, wc.GetRunId(), wc.GetDeploymentId(), wc.GetRank(), id)
	case agentv1.WorkloadOp_WORKLOAD_OP_PAUSE:
		info, err := a.resolveInfo(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		if info.State != "running" {
			a.result(cmdID, false, 0, "container.not_running", info.ID, info.State)
			return
		}
		if err := a.rt.Pause(ctx, info.ID); err != nil {
			a.result(cmdID, false, 0, err.Error(), info.ID, info.State)
			return
		}
		a.result(cmdID, true, 0, "", info.ID, "paused")
	case agentv1.WorkloadOp_WORKLOAD_OP_UNPAUSE:
		info, err := a.resolveInfo(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		if info.State != "paused" {
			a.result(cmdID, false, 0, "container.not_paused", info.ID, info.State)
			return
		}
		if err := a.rt.Unpause(ctx, info.ID); err != nil {
			a.result(cmdID, false, 0, err.Error(), info.ID, info.State)
			return
		}
		a.result(cmdID, true, 0, "", info.ID, "running")
	case agentv1.WorkloadOp_WORKLOAD_OP_STOP:
		id, err := a.resolve(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		if err := a.rt.Stop(ctx, id, 15); err != nil {
			a.result(cmdID, false, 0, err.Error(), id, "")
			return
		}
		a.result(cmdID, true, 0, "", id, "")
	case agentv1.WorkloadOp_WORKLOAD_OP_REMOVE:
		id, err := a.resolve(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		force := false
		if info, ierr := a.rt.Inspect(ctx, id); ierr == nil && info.State == "running" {
			force = true
		}
		if err := a.rt.Remove(ctx, id, force); err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		w.stopTailer(wc.GetRunId(), wc.GetDeploymentId(), wc.GetRank())
		a.result(cmdID, true, 0, "", "", "")
	case agentv1.WorkloadOp_WORKLOAD_OP_INSPECT:
		id, err := a.resolve(name, deploymentID, runID, wc.GetRank())
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		info, err := a.rt.Inspect(ctx, id)
		if err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		a.result(cmdID, true, int32(info.ExitCode), info.Error, info.ID, info.State)
	case agentv1.WorkloadOp_WORKLOAD_OP_LOGS:
		if _, err := a.resolve(name, deploymentID, runID, wc.GetRank()); err != nil {
			a.result(cmdID, false, 0, err.Error(), "", "")
			return
		}
		a.handleLogRequest(ctx, &agentv1.LogRequest{
			RunId:        wc.GetRunId(),
			DeploymentId: wc.GetDeploymentId(),
			Rank:         wc.GetRank(),
			Stream:       "stdout",
		})
	default:
		a.result(cmdID, false, 0, "unknown workload op", "", "")
	}
}

// resolve finds a managed container by its deterministic name.
func (a *Agent) resolve(name, deploymentID, runID string, rank int32) (string, error) {
	info, err := a.resolveInfo(name, deploymentID, runID, rank)
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

func (a *Agent) resolveInfo(name, deploymentID, runID string, rank int32) (*runtime.ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := a.rt.Inspect(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("container.missing: %s", name)
	}
	if err := validateContainerIdentity(info.Labels, deploymentID, runID, rank); err != nil {
		return nil, fmt.Errorf("%w: %s", err, name)
	}
	return info, nil
}

func (a *Agent) result(cmdID string, ok bool, exit int32, errMsg, containerID, state string) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_CommandResult{
		CommandResult: &agentv1.CommandResult{
			CommandId:      cmdID,
			Ok:             ok,
			ExitCode:       exit,
			Error:          errMsg,
			ContainerId:    containerID,
			ContainerState: state,
		},
	}})
}

// reconcile re-derives container reality after (re)connect or controller
// restart: start the state monitor, report current state, and tail logs
// for running containers.
func (a *Agent) reconcile(sessCtx context.Context, req *agentv1.ReconcileRequest) {
	w := a.workloads
	w.startMonitor(sessCtx)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	w.tick(ctx)
	list, err := a.rt.ListByLabel(ctx, runtime.LabelManaged, "true")
	if err != nil {
		return
	}
	for _, c := range list {
		if c.State == "running" {
			dep, run, rank := labelsOf(&c)
			w.startTailer(sessCtx, run, dep, rank, c.ID)
		}
	}
}

func labelsOf(c *runtime.ContainerInfo) (deployment, run string, rank int32) {
	deployment = c.Labels[runtime.LabelDeployment]
	run = c.Labels[runtime.LabelRun]
	if r := c.Labels[runtime.LabelRank]; r != "" {
		_, _ = fmt.Sscanf(r, "%d", &rank)
	}
	return
}

// startMonitor runs container state monitoring for the session lifetime.
// It is idempotent: a second call while one is live is a no-op.
const monitorPeriod = 2 * time.Second

func (w *workloads) startMonitor(ctx context.Context) {
	w.mu.Lock()
	if w.monitorCtx != nil {
		w.mu.Unlock()
		return
	}
	c, cancelFn := context.WithCancel(ctx)
	w.monitorCtx = c
	w.monitorCancel = cancelFn
	w.mu.Unlock()
	go w.monitorLoop(c)
}

func (w *workloads) monitorLoop(ctx context.Context) {
	defer func() {
		w.mu.Lock()
		w.monitorCtx = nil
		w.monitorCancel = nil
		w.mu.Unlock()
	}()
	t := time.NewTicker(monitorPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(context.Background())
		}
	}
}

func (w *workloads) tick(ctx context.Context) {
	list, err := w.a.rt.ListByLabel(ctx, runtime.LabelManaged, "true")
	if err != nil {
		return
	}
	seen := map[string]bool{}
	for _, c := range list {
		seen[c.ID] = true
		w.reportState(&c)
	}
	w.mu.Lock()
	var gone []struct {
		id string
		st containerState
	}
	for id, st := range w.last {
		if !seen[id] && st.reported {
			gone = append(gone, struct {
				id string
				st containerState
			}{id, st})
			delete(w.last, id)
		}
	}
	w.mu.Unlock()
	for _, g := range gone {
		w.sendState(&agentv1.StateUpdate{
			DeploymentId:      g.st.dep,
			ContainerId:       g.id,
			State:             "missing",
			Rank:              g.st.rank,
			DiagnosticCode:    "container.missing",
			DiagnosticMessage: "managed container no longer exists",
		})
	}
}

// reportState sends a StateUpdate when a container's observed state changes.
func (w *workloads) reportState(c *runtime.ContainerInfo) {
	dep, run, rank := labelsOf(c)
	w.mu.Lock()
	last := w.last[c.ID]
	changed := !last.reported || last.state != c.State
	if changed {
		w.last[c.ID] = containerState{
			state:    c.State,
			reported: true,
			dep:      dep,
			run:      run,
			rank:     rank,
		}
	}
	w.mu.Unlock()
	if !changed {
		return
	}
	update := &agentv1.StateUpdate{
		DeploymentId: dep,
		ContainerId:  c.ID,
		State:        c.State,
		Rank:         rank,
	}
	if c.Error != "" {
		update.DiagnosticCode = "container.error"
		update.DiagnosticMessage = c.Error
	}
	w.mu.Lock()
	spec, ok := w.specs[containerName(dep, run, rank)]
	w.mu.Unlock()
	if ok && len(spec.Ports) > 0 {
		port := spec.Ports[0].Host
		if port == 0 {
			port = spec.Ports[0].Container
		}
		update.EndpointPort = uint32(port)
		update.EndpointHost = "0.0.0.0"
	}
	w.sendState(update)
}

func (w *workloads) sendState(su *agentv1.StateUpdate) {
	w.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_StateUpdate{StateUpdate: su}})
}

// applyCertificate persists a rotated node certificate. The rotated cert
// reuses the node's original key, so only the certificate changes.
func (a *Agent) applyCertificate(cert *agentv1.Certificate) error {
	certPEM := []byte(cert.GetNodeCertificatePem())
	if err := os.WriteFile(a.cfg.NodeCertPath(), certPEM, 0o644); err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(a.cfg.NodeKeyPath())
	if err != nil {
		return fmt.Errorf("node key for rotated cert: %w", err)
	}
	pair, err := tlsX509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("rotated keypair: %w", err)
	}
	a.certMu.Lock()
	a.nodeCert = pair
	a.certMu.Unlock()
	if caPEM := cert.GetCaCertificatePem(); caPEM != "" {
		if werr := os.WriteFile(a.caPEMPath(), []byte(caPEM), 0o644); werr != nil {
			return werr
		}
		a.mu.Lock()
		a.caPEM = []byte(caPEM)
		if pub, ok := caPubFromPEM([]byte(caPEM)); ok {
			a.caPub = pub
		}
		a.mu.Unlock()
	}
	return nil
}
