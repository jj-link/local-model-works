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

	"github.com/google/uuid"

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

type runnerNodeInventory struct {
	Hostname string `json:"hostname"`
}

func (m *Module) localRunnerNodeID(ctx context.Context) (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("resolve local runner hostname: %w", err)
	}
	nodeRows, err := m.env.Q.ListNodes(ctx)
	if err != nil {
		return "", fmt.Errorf("list nodes for local runner: %w", err)
	}

	var matching, online []string
	for _, node := range nodeRows {
		if node.Status == "pending" || !node.Inventory.Valid {
			continue
		}
		var inventory runnerNodeInventory
		if err := json.Unmarshal([]byte(node.Inventory.String), &inventory); err != nil {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(inventory.Hostname), strings.TrimSpace(hostname)) {
			continue
		}
		matching = append(matching, node.ID)
		if m.env.Nodes != nil && m.env.Nodes.Online(node.ID) {
			online = append(online, node.ID)
		}
	}

	switch len(online) {
	case 1:
		return online[0], nil
	case 0:
		switch len(matching) {
		case 0:
			return "", errors.New("autoresearch.runner_not_configured: no approved local runner is registered")
		case 1:
			return matching[0], nil
		}
	}
	return "", errors.New("autoresearch.runner_not_configured: multiple approved local runners match this host")
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
	if strings.TrimSpace(settings.RunnerNodeID) == "" {
		settings.RunnerNodeID, err = m.localRunnerNodeID(ctx)
		if err != nil {
			return workerSettings{}, err
		}
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
	if settings.WorkerImage == "" {
		return workerSettings{}, errors.New("autoresearch.runner_not_configured")
	}
	if !strings.Contains(settings.WorkerImage, "@sha256:") {
		return workerSettings{}, errors.New("autoresearch.runner_not_configured: worker image must be pinned by digest")
	}
	return settings, nil
}

func (m *Module) requireWorkerSettings(ctx context.Context) (workerSettings, error) {
	settings, err := m.loadWorkerSettings(ctx)
	if err != nil {
		return workerSettings{}, err
	}
	if m.env.Nodes == nil || !m.env.Nodes.Online(settings.RunnerNodeID) {
		return workerSettings{}, errors.New("autoresearch.runner_offline")
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
	advisors, _ := config["advisors"].(map[string]any)
	defaultAdvisor, _ := advisors["default"].(map[string]any)
	defaultProvider, hasDefaultProvider := defaultAdvisor["provider"]
	if !hasDefaultProvider {
		return
	}
	for name, raw := range advisors {
		if name == "default" {
			continue
		}
		advisor, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, configured := advisor["provider"]; !configured {
			advisor["provider"] = defaultProvider
		}
	}
}

func mapFromJSONValue(value any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		decoded = map[string]any{}
	}
	return decoded, nil
}

func effectiveProviderSnapshot(raw string, settings workerSettings, overrides any) (map[string]any, error) {
	config := configSnapshot(raw)
	mergeProjectDefaults(config, settings)
	if overrides == nil {
		return config, nil
	}
	overrideMap, err := mapFromJSONValue(overrides)
	if err != nil {
		return nil, err
	}
	roles, _ := config["roles"].(map[string]any)
	if roles == nil {
		roles = map[string]any{}
		config["roles"] = roles
	}
	for role, provider := range overrideMap {
		roles[role] = provider
	}
	return config, nil
}

func providerSnapshotFromInput(input map[string]any) (map[string]any, bool, error) {
	raw, ok := input["provider_config"]
	if !ok {
		return nil, false, nil
	}
	config, err := mapFromJSONValue(raw)
	return config, true, err
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

func scrubRunCredentials(projectRoot, runID string) error {
	if _, err := uuid.Parse(runID); err != nil {
		return fmt.Errorf("autoresearch.credential_cleanup_failed: invalid run id: %w", err)
	}
	scratch := filepath.Join(projectRoot, "scratch", runID)
	if err := os.RemoveAll(filepath.Join(scratch, "credentials")); err != nil {
		return err
	}
	legacyKey := filepath.Join(scratch, "ssh", "id_key")
	if err := os.Remove(legacyKey); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func scrubStartupCredentials(root string) error {
	if root == "" {
		return nil
	}
	projects, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		if _, err := uuid.Parse(project.Name()); err != nil {
			continue
		}
		projectRoot := filepath.Join(root, project.Name())
		runs, err := os.ReadDir(filepath.Join(projectRoot, "scratch"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, run := range runs {
			if !run.IsDir() {
				continue
			}
			if _, err := uuid.Parse(run.Name()); err != nil {
				continue
			}
			if err := scrubRunCredentials(projectRoot, run.Name()); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeCredentialFiles(root string, secrets map[string]string) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}
	filenames := make(map[string]string, len(secrets))
	for name := range secrets {
		filename := secretFilename.ReplaceAllString(name, "_")
		if filename == "" {
			return "", errors.New("autoresearch.secret_name_invalid")
		}
		if previous, exists := filenames[filename]; exists && previous != name {
			return "", fmt.Errorf("autoresearch.secret_name_collision: %q and %q", previous, name)
		}
		filenames[filename] = name
	}
	credentials := filepath.Join(root, "credentials")
	if err := os.MkdirAll(credentials, 0o700); err != nil {
		return "", err
	}
	for filename, name := range filenames {
		if err := os.WriteFile(filepath.Join(credentials, filename), []byte(secrets[name]), 0o600); err != nil {
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

func (m *Module) executeWorker(ctx context.Context, job *jobs.Context, factory string) (output map[string]any, runErr error) {
	projectID, _ := job.Input["project_id"].(string)
	settings, err := m.requireWorkerSettings(ctx)
	if err != nil {
		return nil, err
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
	if err := scrubRunCredentials(projectRoot, job.RunID); err != nil {
		m.recordCredentialCleanupFailure(err)
		return nil, fmt.Errorf("autoresearch.credential_cleanup_failed: %w", err)
	}
	defer func() {
		if err := scrubRunCredentials(projectRoot, job.RunID); err != nil {
			m.recordCredentialCleanupFailure(err)
			output = nil
			runErr = fmt.Errorf("autoresearch.credential_cleanup_failed: %w", err)
		}
	}()
	for _, directory := range []string{"tmp", "home/.claude", "home/.codex"} {
		if err := os.MkdirAll(filepath.Join(scratch, filepath.FromSlash(directory)), 0o700); err != nil {
			return nil, err
		}
	}
	credentials, err := writeCredentialFiles(scratch, job.Secrets)
	if err != nil {
		return nil, err
	}

	projectConfig, configured, err := providerSnapshotFromInput(job.Input)
	if err != nil {
		return nil, err
	}
	if !configured {
		return nil, errors.New("autoresearch.provider_config_missing")
	}
	if err := m.resolveProjectProviders(ctx, projectConfig); err != nil {
		return nil, err
	}
	runInput := make(map[string]any, len(job.Input)+2)
	for key, value := range job.Input {
		runInput[key] = value
	}
	if factory == "idea" {
		if _, generated := candidateCount(job.Input); generated {
			prompt, _ := job.Input["prompt"].(string)
			intakeInputs, err := m.writeIdeaIntakeInputs(ctx, projectID, projectRoot, prompt)
			if err != nil {
				return nil, err
			}
			for key, value := range intakeInputs {
				runInput[key] = value
			}
		}
	}
	config := map[string]any{
		"schema": 1, "project_id": projectID, "run_id": job.RunID, "factory": factory,
		"project": projectConfig, "input": runInput, "worker": map[string]any{"ssh_hosts": settings.SSHHosts},
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
	output = map[string]any{"project_id": projectID, "changed_paths": []string{}}
	paper := filepath.Join(m.paperRoot(projectID), "build", "manuscript.pdf")
	if _, err := os.Stat(paper); err == nil {
		output["paper_path"] = "workspace/project/paper/build/manuscript.pdf"
	}
	return output, nil
}

func (m *Module) runFactory(ctx context.Context, job *jobs.Context) (output map[string]any, err error) {
	factory, _ := job.Input["factory"].(string)
	output, err = m.runFactoryLifecycle(ctx, job, factory)
	if err != nil {
		projectID, _ := job.Input["project_id"].(string)
		m.setProjectFailedBackground(projectID)
	}
	return output, err
}
