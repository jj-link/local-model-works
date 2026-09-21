package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/errdefs"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

type upstreamRequest struct {
	Spec         ContainerSpec     `json:"spec"`
	Installation string            `json:"installation"`
	RecordDir    string            `json:"recordDir"`
	Operation    string            `json:"operation"`
	Before       map[string]string `json:"before"`
	Socket       string            `json:"socket"`
	SourceParent string            `json:"sourceParent,omitempty"`
}

// RunUpstreamWorker handles the private durable supervisor mode of lmw-agent.
// The worker is detached from the control session, never from its installation
// record, and writes its outcome before exiting. Other arguments are untouched.
func RunUpstreamWorker(args []string) (bool, error) {
	if len(args) == 0 || args[0] != "upstream-worker" {
		return false, nil
	}
	if len(args) != 2 || !filepath.IsAbs(args[1]) {
		return true, fmt.Errorf("upstream.worker_arguments_invalid")
	}
	dir := args[1]
	var request upstreamRequest
	if err := readUpstreamJSON(filepath.Join(dir, "request.json"), &request); err != nil {
		return true, err
	}
	if filepath.Dir(dir) != request.RecordDir {
		return true, fmt.Errorf("upstream.worker_installation_invalid")
	}
	if err := validateUpstream(&request.Spec); err != nil {
		return true, err
	}
	if request.Spec.Upstream.ObserveOnly != (request.Operation == "observe") {
		return true, fmt.Errorf("upstream.observer_lifecycle_invalid")
	}
	if request.Operation != "start" && request.Operation != "stop" && request.Operation != "observe" {
		return true, fmt.Errorf("upstream.worker_operation_invalid")
	}
	// No host commands execute until the parent has durably recorded this PID.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
		if time.Now().After(deadline) {
			return true, fmt.Errorf("upstream.worker_not_committed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var rec upstreamRecord
	if err := readUpstreamJSON(filepath.Join(request.RecordDir, "record.json"), &rec); err != nil {
		return true, err
	}
	if rec.Removed || rec.Workspace != request.Installation || rec.Job == nil || rec.Job.Dir != dir || rec.Job.PID != os.Getpid() || !upstreamProcessAlive(rec.Job.PID, rec.Job.ProcessIdentity) {
		return true, fmt.Errorf("upstream.worker_identity_invalid")
	}
	if !reflect.DeepEqual(rec.Spec, request.Spec) || rec.SourceParent != request.SourceParent {
		return true, fmt.Errorf("upstream.worker_review_changed")
	}
	base, err := NewDocker(request.Socket)
	if err != nil {
		return true, err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err = executeUpstreamJob(ctx, base, dir, &request, os.Stdout, os.Stderr)
	return true, finishUpstreamWorker(ctx, rec.Job.ProcessIdentity, err)
}

func executeUpstreamJob(ctx context.Context, base Runtime, dir string, request *upstreamRequest, stdout, stderr io.Writer) (result error) {
	status := upstreamStatus{Phase: "starting", Observed: make(map[string]string)}
	if request.Operation == "observe" {
		status.Phase = "installing"
	}
	if request.Operation == "stop" {
		status.Phase = "stopping"
		for name, id := range request.Before {
			status.Observed[name] = id
		}
	}
	persist := func() error { return writeUpstreamJSON(filepath.Join(dir, "status.json"), &status) }
	defer func() {
		status.Done = true
		if result != nil {
			status.Phase, status.Error, status.ExitCode = "dead", result.Error(), 1
			var exit *exec.ExitError
			if errors.As(result, &exit) {
				status.ExitCode = exit.ExitCode()
			}
			_, _ = fmt.Fprintln(stderr, result)
		}
		if err := persist(); err != nil {
			result = errors.Join(result, err)
		}
	}()
	if err := validateUpstream(&request.Spec); err != nil {
		return err
	}
	if request.Spec.Upstream.ObserveOnly != (request.Operation == "observe") {
		return fmt.Errorf("upstream.observer_lifecycle_invalid")
	}
	if err := persist(); err != nil {
		return err
	}
	repository := filepath.Join(request.Installation, "repository")
	if request.Operation == "start" || request.Operation == "observe" {
		var previous *upstreamRecord
		if request.SourceParent != "" {
			if !strings.HasPrefix(request.SourceParent, "~lmw-upstream-") || filepath.Base(request.SourceParent) != request.SourceParent {
				return fmt.Errorf("upstream.source_predecessor_invalid")
			}
			var parent upstreamRecord
			root := filepath.Dir(request.RecordDir)
			if err := readUpstreamJSON(filepath.Join(root, request.SourceParent, "record.json"), &parent); err != nil {
				return err
			}
			owner := &upstreamRuntime{root: root}
			if parent.ID != request.SourceParent {
				return fmt.Errorf("upstream.source_predecessor_invalid")
			}
			if err := owner.verifyWorkspace(&parent); err != nil {
				return err
			}
			previous = &parent
		}
		if err := prepareUpstreamWorkspace(ctx, &request.Spec, request.Installation, previous, stdout, stderr); err != nil {
			return err
		}
	} else if _, err := os.Stat(filepath.Join(repository, ".git")); err != nil {
		return fmt.Errorf("upstream.source_missing: %w", err)
	}
	if err := verifyUpstreamSource(ctx, &request.Spec, repository); err != nil {
		return err
	}
	working, err := upstreamPath(repository, request.Spec.Upstream.SourcePath, false)
	if err != nil {
		return err
	}
	if stat, err := os.Stat(working); err != nil || !stat.IsDir() {
		return fmt.Errorf("upstream.source_directory_missing: %s", working)
	}
	if request.Operation != "stop" {
		if err := configureUpstreamSource(ctx, &request.Spec, repository); err != nil {
			return err
		}
		if err := configureUpstreamEnv(&request.Spec, working, filepath.Join(request.Installation, "env-state.json")); err != nil {
			return err
		}
	}
	if err := recordUpstreamSource(ctx, &request.Spec, repository); err != nil {
		return err
	}
	environment, err := upstreamCommandEnvironment(os.Environ(), request.Spec.Env, request.Socket)
	if err != nil {
		return err
	}
	observe := func(observationContext context.Context) error {
		for name := range upstreamContainerNames(request.Spec.Upstream) {
			info, err := base.Inspect(observationContext, name)
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if request.Operation == "stop" {
				if request.Before[name] != info.ID {
					return fmt.Errorf("upstream.container_identity_changed: %s", name)
				}
			} else if info.ID != request.Before[name] || info.State == "running" {
				status.Observed[name] = info.ID
			}
		}
		return persist()
	}
	run := func(argv []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := verifyUpstreamSource(ctx, &request.Spec, repository); err != nil {
			return err
		}
		// Detect a foreign replacement between authored lifecycle commands.
		for name := range upstreamContainerNames(request.Spec.Upstream) {
			info, err := base.Inspect(ctx, name)
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.ID != request.Before[name] && info.ID != status.Observed[name] {
				return fmt.Errorf("upstream.foreign_container: %s", name)
			}
		}
		cmd, err := upstreamAuthoredCommand(ctx, argv, working, environment)
		if err != nil {
			return err
		}
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("upstream.command_start: %w", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				snapshotCtx, snapshotCancel := context.WithTimeout(context.Background(), 30*time.Second)
				observationErr := observe(snapshotCtx)
				snapshotErr := recordUpstreamSource(snapshotCtx, &request.Spec, repository)
				environmentErr := recordUpstreamEnv(&request.Spec, working, filepath.Join(request.Installation, "env-state.json"))
				snapshotCancel()
				if err != nil {
					return errors.Join(fmt.Errorf("upstream.command_failed (%s): %w", argv[0], err), snapshotErr, observationErr, environmentErr)
				}
				return errors.Join(observationErr, snapshotErr, environmentErr)
			case <-ticker.C:
				if ctx.Err() == nil {
					if err := observe(ctx); err != nil {
						_ = cmd.Process.Kill()
						<-done
						return err
					}
				}
			}
		}
	}
	if request.Operation == "stop" {
		if err := run(request.Spec.Upstream.Stop); err != nil {
			return err
		}
		for name := range upstreamContainerNames(request.Spec.Upstream) {
			ci, err := base.Inspect(ctx, name)
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if ci.ID != status.Observed[name] || !stoppedUpstreamContainer(ci.State) {
				return fmt.Errorf("upstream.stop_incomplete: %s", name)
			}
		}
		status.Phase = "exited"
		return nil
	}
	installed := filepath.Join(request.Installation, "installed.json")
	var installedRevision string
	if err := readUpstreamJSON(installed, &installedRevision); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if installedRevision != "" && !strings.EqualFold(installedRevision, request.Spec.Upstream.Revision) {
		return fmt.Errorf("upstream.installation_revision_changed")
	}
	inputs, err := upstreamInstallationInputs(&request.Spec)
	if err != nil {
		return err
	}
	inputsPath := filepath.Join(request.Installation, "installed-inputs.json")
	var installedInputs string
	if err := readUpstreamJSON(inputsPath, &installedInputs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if installedRevision == "" || installedInputs != inputs {
		status.Phase = "installing"
		if err := persist(); err != nil {
			return err
		}
		for _, argv := range request.Spec.Upstream.Install {
			if err := run(argv); err != nil {
				return err
			}
		}
		if err := writeUpstreamJSON(installed, request.Spec.Upstream.Revision); err != nil {
			return err
		}
		if err := writeUpstreamJSON(inputsPath, inputs); err != nil {
			return err
		}
	}
	if err := verifyUpstreamSource(ctx, &request.Spec, repository); err != nil {
		return err
	}
	if request.Operation == "observe" {
		status.Phase = "observing"
		return nil
	}
	status.Phase = "starting"
	if err := persist(); err != nil {
		return err
	}
	if err := run(request.Spec.Upstream.Start); err != nil {
		return err
	}
	status.Phase = "started"
	return nil
}

// A retained checkout is not proof that preparation used the selected model,
// image or configuration. Reuse completion only for identical install inputs;
// changed inputs rerun the original preparation against the same cached files.
func upstreamInstallationInputs(spec *ContainerSpec) (string, error) {
	if len(spec.Upstream.Install) == 0 {
		return "", nil
	}
	environment := slices.Clone(spec.Env)
	slices.Sort(environment)
	data, err := json.Marshal(struct {
		Commands      [][]string
		Environment   []string
		Configuration []sourceconfig.ResolvedFile
	}{spec.Upstream.Install, environment, spec.Upstream.Configuration})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func materializeUpstream(ctx context.Context, spec *ContainerSpec, repository string, stdout, stderr io.Writer) error {
	if _, err := os.Stat(repository); err == nil {
		return verifyUpstreamSource(ctx, spec, repository)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, marker := range []string{"installed.json", "source-state.json"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(repository), marker)); err == nil {
			return fmt.Errorf("upstream.source_missing: retained installation checkout has disappeared")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	staging := repository + "-checkout-" + randomUpstreamID()
	defer os.RemoveAll(staging)
	commands := [][]string{
		{"clone", "--no-checkout", "--", spec.Upstream.SourceURL, staging},
		{"-C", staging, "fetch", "origin", spec.Upstream.Revision},
		{"-C", staging, "checkout", "--detach", spec.Upstream.Revision},
		{"-C", staging, "submodule", "update", "--init", "--recursive"},
	}
	for _, argv := range commands {
		cmd := exec.CommandContext(ctx, "git", argv...)
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("upstream.source_checkout: %w", err)
		}
	}
	if err := verifyUpstreamSource(ctx, spec, staging); err != nil {
		return err
	}
	return os.Rename(staging, repository)
}

func verifyUpstreamSource(ctx context.Context, spec *ContainerSpec, repository string) error {
	for _, path := range []string{repository, filepath.Join(repository, ".git")} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("upstream.source_symlink")
		}
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-C", repository}, args...)...)
		data, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("upstream.source_verification: %w: %s", err, strings.TrimSpace(string(data)))
		}
		return strings.TrimSpace(string(data)), nil
	}
	head, err := git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if !strings.EqualFold(head, spec.Upstream.Revision) {
		return fmt.Errorf("upstream.source_revision_changed: got %s, require %s", head, spec.Upstream.Revision)
	}
	origin, err := git("remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if origin != spec.Upstream.SourceURL {
		return fmt.Errorf("upstream.source_origin_changed")
	}
	changed, err := git("diff", "HEAD", "--name-only", "--no-renames")
	if err != nil {
		return err
	}
	submodules, err := git("submodule", "status", "--recursive")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(submodules, "\n") {
		if line != "" && (line[0] == '+' || line[0] == '-' || line[0] == 'U') {
			return fmt.Errorf("upstream.source_submodule_changed: %s", line)
		}
	}
	config := ""
	var recorded map[string]string
	if err := readUpstreamJSON(filepath.Join(filepath.Dir(repository), "source-state.json"), &recorded); err == nil {
		actual, err := upstreamSourceFingerprint(ctx, spec, repository)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, recorded) {
			return fmt.Errorf("upstream.source_modified_outside_lifecycle")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if spec.Upstream.EnvFile != "" {
		config = filepath.ToSlash(filepath.Join(spec.Upstream.SourcePath, spec.Upstream.EnvFile))
	}
	for _, path := range strings.Split(changed, "\n") {
		if path != "" && path != config {
			return fmt.Errorf("upstream.source_modified: %s", path)
		}
	}
	return nil
}

// Resolve through existing ancestors as well as the final component. Authored
// symlinks within the checkout work; traversal outside it is never a config write.
func upstreamPath(root, relative string, allowMissing bool) (string, error) {
	if relative == "" {
		relative = "."
	}
	if !safeUpstreamRelative(relative) {
		return "", fmt.Errorf("upstream.path_invalid: %s", relative)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, relative)
	ancestor := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			rel, err := filepath.Rel(root, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("upstream.path_escapes_source: %s", relative)
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !allowMissing || !errors.Is(err, os.ErrNotExist) || ancestor == root || filepath.Dir(ancestor) == ancestor {
			return "", err
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = filepath.Dir(ancestor)
	}
}

func configureUpstreamEnv(spec *ContainerSpec, working, statePath string) error {
	u := spec.Upstream
	if u.EnvFile == "" {
		return nil
	}
	path, err := upstreamPath(working, u.EnvFile, true)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if u.EnvTemplate != "" {
			template, err := upstreamPath(working, u.EnvTemplate, false)
			if err != nil {
				return err
			}
			data, err = os.ReadFile(template)
			if err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	lines, state, err := renderUpstreamEnv(spec, lines, statePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		return err
	}
	return writeUpstreamJSON(statePath, state)
}

// Bind plain Docker invocations to the same daemon we observe. This is endpoint
// selection only: no GPU, network, privilege or security options are injected.
func upstreamCommandEnvironment(inherited, overrides []string, socket string) ([]string, error) {
	values := make(map[string]string, len(inherited)+len(overrides)+1)
	for _, entries := range [][]string{inherited, overrides} {
		for _, entry := range entries {
			key, value, ok := strings.Cut(entry, "=")
			if ok {
				values[key] = value
			}
		}
	}
	if socket != "" {
		if !filepath.IsAbs(socket) {
			return nil, fmt.Errorf("upstream.docker_socket_invalid")
		}
		endpoint := "unix://" + socket
		if host := values["DOCKER_HOST"]; host != "" && host != endpoint {
			return nil, fmt.Errorf("upstream.docker_endpoint_mismatch: commands select %s but runtime observes %s", host, endpoint)
		}
		for _, name := range []string{"DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "DOCKER_TLS"} {
			if values[name] != "" {
				return nil, fmt.Errorf("upstream.docker_environment_conflict: %s", name)
			}
		}
		values["DOCKER_HOST"] = endpoint
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result, nil
}

func upstreamAuthoredCommand(ctx context.Context, argv []string, working string, environment []string) (*exec.Cmd, error) {
	executable := argv[0]
	if !strings.ContainsRune(executable, filepath.Separator) {
		path := ""
		for _, entry := range environment {
			if value, ok := strings.CutPrefix(entry, "PATH="); ok {
				path = value
				break
			}
		}
		found := false
		for _, directory := range filepath.SplitList(path) {
			if directory == "" {
				directory = working
			} else if !filepath.IsAbs(directory) {
				directory = filepath.Join(working, directory)
			}
			candidate := filepath.Join(directory, executable)
			info, err := os.Stat(candidate)
			if err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
				executable, found = candidate, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("upstream.executable_missing: %s", argv[0])
		}
	}
	cmd := exec.CommandContext(ctx, executable, argv[1:]...)
	cmd.Args[0], cmd.Dir, cmd.Env = argv[0], working, environment
	return cmd, nil
}
