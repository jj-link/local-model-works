package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/workload"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

const (
	workerPullTimeout  = 30 * time.Minute
	workerOpTimeout    = 2 * time.Minute
	workerPollInterval = 2 * time.Second
	workerRunLimit     = 24 * time.Hour
)

var secretFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type workerSettings struct {
	RunnerNodeID    string
	WorkerImage     string
	SSHHosts        []map[string]string
	DefaultRoles    map[string]any
	DefaultAdvisors map[string]any
}

func (m *Module) loadWorkerSettings(ctx context.Context) (workerSettings, error) {
	values, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return workerSettings{}, err
	}
	settings := workerSettings{}
	if value, ok := values["runner_node_id"].(string); ok {
		settings.RunnerNodeID = value
	}
	if value, ok := values["worker_image"].(string); ok {
		settings.WorkerImage = value
	}
	if value, ok := values["default_role_assignments"].(map[string]any); ok {
		settings.DefaultRoles = value
	}
	if value, ok := values["default_advisor_assignments"].(map[string]any); ok {
		settings.DefaultAdvisors = value
	}
	if rawHosts, ok := values["ssh_hosts"].([]any); ok {
		for _, rawHost := range rawHosts {
			host, ok := rawHost.(map[string]any)
			if !ok {
				continue
			}
			settings.SSHHosts = append(settings.SSHHosts, map[string]string{
				"alias": fmt.Sprint(host["alias"]), "hostname": fmt.Sprint(host["hostname"]), "user": fmt.Sprint(host["user"]),
			})
		}
	}
	if settings.RunnerNodeID == "" || settings.WorkerImage == "" {
		return workerSettings{}, errors.New("autoresearch.runner_not_configured")
	}
	if !strings.Contains(settings.WorkerImage, "@sha256:") {
		return workerSettings{}, errors.New("autoresearch.worker_image_unpinned")
	}
	return settings, nil
}

func mergeProjectDefaults(config map[string]any, settings workerSettings) {
	for key, defaults := range map[string]map[string]any{
		"roles": settings.DefaultRoles, "advisors": settings.DefaultAdvisors,
	} {
		current, _ := config[key].(map[string]any)
		if current == nil {
			current = map[string]any{}
			config[key] = current
		}
		for name, value := range defaults {
			if _, overridden := current[name]; !overridden {
				current[name] = value
			}
		}
	}
}

func (m *Module) projectConfigWithDefaults(ctx context.Context, raw string) map[string]any {
	config := configSnapshot(raw)
	values, _, err := m.env.Settings.Get(ctx, descriptor.ID)
	if err != nil {
		return config
	}
	settings := workerSettings{}
	settings.DefaultRoles, _ = values["default_role_assignments"].(map[string]any)
	settings.DefaultAdvisors, _ = values["default_advisor_assignments"].(map[string]any)
	mergeProjectDefaults(config, settings)
	return config
}

func imageParts(reference string) (string, string, error) {
	index := strings.LastIndex(reference, "@sha256:")
	if index <= 0 || len(reference[index+1:]) != len("sha256:")+64 {
		return "", "", errors.New("autoresearch.worker_image_unpinned")
	}
	return reference[:index], reference[index+1:], nil
}

func workerSpec(runID, imageRef, imageDigest, projectRoot, scratch, credentials, user string, command []string) *runtime.ContainerSpec {
	mounts := []runtime.MountSpec{
		{Source: projectRoot, Dest: "/project", ReadOnly: false},
		{Source: scratch, Dest: "/scratch", ReadOnly: false},
	}
	if credentials != "" {
		mounts = append(mounts, runtime.MountSpec{Source: credentials, Dest: "/run/lmw-credentials", ReadOnly: true})
	}
	return &runtime.ContainerSpec{
		Image: imageRef, ImageDigest: imageDigest,
		Cmd: command, WorkingDir: "/project/artifacts", User: user, NetworkMode: "bridge",
		ReadonlyRootfs: true, NoNewPrivileges: true, CapDrop: []string{"ALL"},
		TmpfsBytes: 1 << 30, ShmBytes: 1 << 30, PidsLimit: 512, MemoryBytes: 16 << 30, CPU: 8,
		Mounts: mounts,
		Labels: runtime.ManagedLabels("", runID, imageDigest, "1", 0, descriptor.ID),
		Env: []string{
			"HOME=/scratch/home", "CLAUDE_PLUGIN_ROOT=/opt/agon", "AGON_RUNNER=/usr/local/bin/lmw-agon-runner",
			"LMW_CREDENTIAL_DIR=/run/lmw-credentials", "TMPDIR=/scratch/tmp",
		},
	}
}

func projectWorkerUser(root string) (string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("autoresearch.project_owner_unavailable")
	}
	uid, gid := int(stat.Uid), int(stat.Gid)
	if uid == 0 {
		uid, gid = 10001, 10001
		if err := filepath.Walk(root, func(path string, _ os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			return os.Chown(path, uid, gid)
		}); err != nil {
			return "", err
		}
	}
	if uid <= 0 || gid < 0 {
		return "", errors.New("autoresearch.worker_user_invalid")
	}
	return strconv.Itoa(uid) + ":" + strconv.Itoa(gid), nil
}

func writeCredentialFiles(root string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	credentials := filepath.Join(root, "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		return "", err
	}
	for name, value := range secrets {
		filename := secretFilename.ReplaceAllString(name, "_")
		if filename == "" {
			return "", errors.New("autoresearch.secret_name_invalid")
		}
		if err := os.WriteFile(filepath.Join(credentials, filename), []byte(value), 0o600); err != nil {
			return "", err
		}
	}
	return credentials, nil
}

func runOperation(ctx context.Context, client *workload.Client, op agentv1.WorkloadOp, spec []byte, timeout time.Duration) (*agentv1.CommandResult, error) {
	result, err := client.Do(ctx, op, spec, timeout)
	if err != nil {
		return nil, err
	}
	if err := acknowledge(result, op.String()); err != nil {
		return nil, err
	}
	return result, nil
}

func waitContainer(ctx context.Context, client *workload.Client, limit time.Duration) (*agentv1.CommandResult, error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(workerPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("autoresearch.worker_timeout")
		case <-ticker.C:
			result, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_INSPECT, nil, workerOpTimeout)
			if err != nil {
				return nil, err
			}
			switch result.GetContainerState() {
			case "exited", "dead":
				return result, nil
			case "running", "created", "paused":
			default:
				return nil, fmt.Errorf("autoresearch.worker_state_invalid: %s", result.GetContainerState())
			}
		}
	}
}

func (m *Module) preflightSharedRoot(ctx context.Context, settings workerSettings, projectRoot, scratch, imageRef, imageDigest, runID, user string) error {
	token := runID + "-shared-root"
	name := ".shared-root-" + strings.ReplaceAll(runID, "-", "")
	sentinel := filepath.Join(projectRoot, ".lmw", name)
	if err := os.WriteFile(sentinel, []byte(token), 0o600); err != nil {
		return err
	}
	defer os.Remove(sentinel)
	preflightRunID := runID + "-preflight"
	client := workload.New(m.env.Nodes, m.env.Commands, settings.RunnerNodeID, "", preflightRunID, 0)
	spec := workerSpec(preflightRunID, imageRef, imageDigest, projectRoot, scratch, "", user, []string{
		"preflight", "--sentinel", "/project/.lmw/" + name, "--expect", token,
	})
	specJSON, _ := json.Marshal(spec)
	if _, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, workerOpTimeout); err != nil {
		return fmt.Errorf("autoresearch.runner_not_colocated: %w", err)
	}
	defer func() {
		_, _ = client.Do(context.Background(), agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, workerOpTimeout)
	}()
	if _, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, workerOpTimeout); err != nil {
		return fmt.Errorf("autoresearch.runner_not_colocated: %w", err)
	}
	result, err := waitContainer(ctx, client, time.Minute)
	if err != nil || result.GetExitCode() != 0 {
		return errors.New("autoresearch.runner_not_colocated")
	}
	if _, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, workerOpTimeout); err != nil {
		return err
	}
	return nil
}

func factoryCommand(job *jobs.Context, factory string) []string {
	return []string{
		"supervise", "--run-id", job.RunID, "--project-root", "/project", "--scratch", "/scratch",
		"--factory", factory, "--config", "/project/.lmw/config.json",
	}
}

func (m *Module) executeWorker(ctx context.Context, job *jobs.Context, factory string) (map[string]any, error) {
	projectID, _ := job.Input["project_id"].(string)
	project, err := m.env.Q.GetAutoResearchProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	settings, err := m.loadWorkerSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !project.RunnerNodeID.Valid || project.RunnerNodeID.String != settings.RunnerNodeID || !m.env.Nodes.Online(settings.RunnerNodeID) {
		return nil, errors.New("autoresearch.runner_not_colocated")
	}
	projectRoot := m.projectRoot(projectID)
	if err := initializeProjectRoot(projectRoot); err != nil {
		return nil, err
	}
	user, err := projectWorkerUser(projectRoot)
	if err != nil {
		return nil, err
	}
	scratch := filepath.Join(projectRoot, "scratch", job.RunID)
	for _, directory := range []string{"tmp", "home/.claude", "home/.codex"} {
		if err := os.MkdirAll(filepath.Join(scratch, filepath.FromSlash(directory)), 0o700); err != nil {
			return nil, err
		}
	}
	credentials, err := writeCredentialFiles(scratch, job.Secrets)
	if err != nil {
		return nil, err
	}
	if credentials != "" {
		defer os.RemoveAll(credentials)
	}
	projectConfig := configSnapshot(project.ConfigJson)
	mergeProjectDefaults(projectConfig, settings)
	if err := m.resolveProjectProviders(ctx, projectConfig); err != nil {
		return nil, err
	}
	config := map[string]any{
		"schema": 1, "project_id": projectID, "run_id": job.RunID, "factory": factory,
		"project": projectConfig, "input": job.Input, "worker": map[string]any{"ssh_hosts": settings.SSHHosts},
	}
	configJSON, _ := json.MarshalIndent(config, "", "  ")
	if err := os.WriteFile(filepath.Join(projectRoot, ".lmw", "config.json"), append(configJSON, '\n'), 0o600); err != nil {
		return nil, err
	}
	imageRef, imageDigest, err := imageParts(settings.WorkerImage)
	if err != nil {
		return nil, err
	}
	pullClient := workload.New(m.env.Nodes, m.env.Commands, settings.RunnerNodeID, "", job.RunID+"-preflight", 0)
	pullSpec := workerSpec(job.RunID+"-preflight", imageRef, imageDigest, projectRoot, scratch, "", user, []string{"preflight"})
	pullJSON, _ := json.Marshal(pullSpec)
	if _, err := runOperation(ctx, pullClient, agentv1.WorkloadOp_WORKLOAD_OP_PULL, pullJSON, workerPullTimeout); err != nil {
		return nil, err
	}
	if err := m.preflightSharedRoot(ctx, settings, projectRoot, scratch, imageRef, imageDigest, job.RunID, user); err != nil {
		return nil, err
	}
	client := workload.New(m.env.Nodes, m.env.Commands, settings.RunnerNodeID, "", job.RunID, 0)
	spec := workerSpec(job.RunID, imageRef, imageDigest, projectRoot, scratch, credentials, user, factoryCommand(job, factory))
	specJSON, _ := json.Marshal(spec)
	if _, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_CREATE, specJSON, workerOpTimeout); err != nil {
		return nil, err
	}
	removed := false
	defer func() {
		if removed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = client.Do(cleanup, agentv1.WorkloadOp_WORKLOAD_OP_STOP, nil, 30*time.Second)
		_, _ = client.Do(cleanup, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, 30*time.Second)
	}()
	if _, err := runOperation(ctx, client, agentv1.WorkloadOp_WORKLOAD_OP_START, nil, workerOpTimeout); err != nil {
		return nil, err
	}
	result, err := waitContainer(ctx, client, workerRunLimit)
	if err != nil {
		return nil, err
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
	_, _ = m.env.Runs.WaitLogEnd(drainCtx, job.RunID, "", 0, "stdout")
	cancel()
	if result.GetExitCode() != 0 {
		return nil, fmt.Errorf("autoresearch.worker_failed: exit %d: %s", result.GetExitCode(), result.GetError())
	}
	if _, err := runOperation(context.Background(), client, agentv1.WorkloadOp_WORKLOAD_OP_REMOVE, nil, workerOpTimeout); err != nil {
		return nil, err
	}
	removed = true
	output := map[string]any{"project_id": projectID, "changed_paths": []string{}}
	paper := filepath.Join(m.paperRoot(projectID), "build", "manuscript.pdf")
	if _, err := os.Stat(paper); err == nil {
		output["paper_path"] = "workspace/project/paper/build/manuscript.pdf"
	}
	return output, nil
}

func (m *Module) runFactory(ctx context.Context, job *jobs.Context) (map[string]any, error) {
	factory, _ := job.Input["factory"].(string)
	return m.runFactoryLifecycle(ctx, job, factory)
}
