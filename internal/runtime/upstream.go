package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/errdefs"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

var upstreamSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
var upstreamName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var upstreamEnvKey = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// upstreamRecord is retained even after removal: rollback workspaces and ownership
// evidence must outlive the logical workload. Only this runtime writes it.
type upstreamRecord struct {
	ID       string            `json:"id"`
	Spec     ContainerSpec     `json:"spec"`
	Removed  bool              `json:"removed,omitempty"`
	Observed map[string]string `json:"observed"`
	// Superseded revokes the whole installation's current authority without
	// deleting historical IDs or the retained source workspace.
	Superseded   bool              `json:"superseded,omitempty"`
	Job          *upstreamJob      `json:"job,omitempty"`
	Stopped      bool              `json:"stopped,omitempty"`
	Workspace    string            `json:"workspace"`
	SourceParent string            `json:"sourceParent,omitempty"`
	Armed        bool              `json:"armed,omitempty"`
	Before       map[string]string `json:"before,omitempty"`
	Attached     map[string]bool   `json:"attached,omitempty"`
}

type upstreamJob struct {
	Dir             string `json:"dir"`
	Operation       string `json:"operation"`
	PID             int    `json:"pid"`
	ProcessIdentity string `json:"processIdentity"`
}

type upstreamStatus struct {
	Phase    string            `json:"phase"`
	Done     bool              `json:"done"`
	ExitCode int               `json:"exitCode"`
	Error    string            `json:"error,omitempty"`
	Observed map[string]string `json:"observed,omitempty"`
}

type upstreamRuntime struct {
	Runtime
	root           string
	allowExecution bool
	mu             sync.Mutex
	records        map[string]*upstreamRecord
}

// WithUpstream always retains installation inspection and ownership. Host lifecycle
// launches additionally require explicit node-administrator permission.
// Revocation does not cancel durable workers already approved and launched.
func WithUpstream(base Runtime, persistentRoot string, allowExecution bool) (Runtime, error) {
	if base == nil || persistentRoot == "" {
		return nil, fmt.Errorf("upstream.runtime_configuration_missing")
	}
	root, err := filepath.Abs(filepath.Join(persistentRoot, "upstream"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	r := &upstreamRuntime{Runtime: base, root: root, allowExecution: allowExecution && goruntime.GOOS == "linux", records: make(map[string]*upstreamRecord)}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "~lmw-upstream-") {
			continue
		}
		var rec upstreamRecord
		if err := readUpstreamJSON(filepath.Join(root, entry.Name(), "record.json"), &rec); err != nil {
			return nil, fmt.Errorf("upstream record %s: %w", entry.Name(), err)
		}
		if rec.ID != entry.Name() {
			return nil, fmt.Errorf("upstream.record_identity_invalid")
		}
		if err := validateUpstream(&rec.Spec); err != nil {
			return nil, err
		}
		if rec.Job != nil && (!upstreamName.MatchString(filepath.Base(rec.Job.Dir)) || filepath.Dir(rec.Job.Dir) != filepath.Join(root, rec.ID)) {
			return nil, fmt.Errorf("upstream.job_path_invalid")
		}
		if err := r.verifyWorkspace(&rec); err != nil {
			return nil, err
		}
		if rec.Observed == nil {
			rec.Observed = make(map[string]string)
		}
		if rec.Job != nil && !rec.Superseded {
			var status upstreamStatus
			if err := readUpstreamJSON(filepath.Join(rec.Job.Dir, "status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			for name, id := range status.Observed {
				rec.Observed[name] = id
			}
		}
		r.records[rec.ID] = &rec
	}
	return r, nil
}

// UpstreamEnabled reports node authority, not merely support for retained records.
func UpstreamEnabled(rt Runtime) bool {
	r, ok := rt.(*upstreamRuntime)
	return ok && r.allowExecution
}

func (r *upstreamRuntime) requireExecution() error {
	if !r.allowExecution {
		return fmt.Errorf("upstream.execution_disabled: node administrator must enable upstream execution; retained installations remain observable")
	}
	return nil
}

func validateUpstream(spec *ContainerSpec) error {
	if err := ValidateManagedSpec(spec); err != nil {
		return err
	}
	u := spec.Upstream
	if u == nil || !u.Approved {
		return fmt.Errorf("upstream.approval_required: host.upstream-exec and host/Docker authority must be reviewed")
	}
	if !IsSHA256Digest(spec.Labels[LabelRecipe]) {
		return fmt.Errorf("upstream.recipe_identity_invalid")
	}
	if !upstreamName.MatchString(spec.Name) {
		return fmt.Errorf("upstream.logical_name_invalid")
	}
	if !upstreamSHA.MatchString(u.Revision) {
		return fmt.Errorf("upstream.full_commit_required")
	}
	if strings.ContainsAny(u.SourceURL, "\x00\r\n") || !(strings.HasPrefix(u.SourceURL, "https://") || strings.HasPrefix(u.SourceURL, "ssh://") || filepath.IsAbs(u.SourceURL)) {
		return fmt.Errorf("upstream.source_url_invalid")
	}
	for _, path := range []string{u.SourcePath, u.EnvFile, u.EnvTemplate, u.LogFile} {
		if path != "" && !safeUpstreamRelative(path) {
			return fmt.Errorf("upstream.source_path_invalid: %q", path)
		}
	}
	if u.EnvTemplate != "" && u.EnvFile == "" {
		return fmt.Errorf("upstream.env_file_required")
	}
	if u.EnvFormat != "" && u.EnvFormat != "shell" && u.EnvFormat != "literal" {
		return fmt.Errorf("upstream.env_format_invalid")
	}
	if err := sourceconfig.ValidateResolved(u.Configuration); err != nil {
		return fmt.Errorf("upstream.configuration_invalid: %w", err)
	}
	commands := append([][]string{u.Start, u.Stop}, u.Install...)
	for _, argv := range commands {
		if len(argv) == 0 || argv[0] == "" {
			return fmt.Errorf("upstream.lifecycle_incomplete")
		}
		for _, arg := range argv {
			if strings.ContainsAny(arg, "\x00\r\n") {
				return fmt.Errorf("upstream.argv_invalid")
			}
		}
	}
	if len(u.Containers) == 0 {
		return fmt.Errorf("upstream.observed_containers_required")
	}
	seen := make(map[string]bool)
	for name := range upstreamContainerNames(u) {
		if !upstreamName.MatchString(name) || seen[name] {
			return fmt.Errorf("upstream.container_name_invalid: %q", name)
		}
		seen[name] = true
	}
	if seen[spec.Name] {
		return fmt.Errorf("upstream.logical_name_conflicts_authored_container")
	}
	envKeys := make(map[string]bool)
	for _, env := range spec.Env {
		key, value, ok := strings.Cut(env, "=")
		if !ok || !upstreamEnvKey.MatchString(key) || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("upstream.env_invalid")
		}
		if envKeys[key] {
			return fmt.Errorf("upstream.env_duplicate: %s", key)
		}
		envKeys[key] = true
	}
	if spec.HostPreparation != nil || len(spec.Mounts) != 0 || len(spec.Cmd) != 0 || len(spec.Entrypoint) != 0 {
		return fmt.Errorf("upstream.container_extensions_unsupported")
	}
	return nil
}

func safeUpstreamRelative(path string) bool {
	if filepath.IsAbs(path) || strings.ContainsAny(path, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (r *upstreamRuntime) PrepareHost(ctx context.Context, spec *ContainerSpec) error {
	if spec != nil && spec.Upstream != nil {
		if err := r.requireExecution(); err != nil {
			return err
		}
		return validateUpstream(spec)
	}
	return r.Runtime.PrepareHost(ctx, spec)
}

func (r *upstreamRuntime) record(id string) *upstreamRecord {
	if rec := r.records[id]; rec != nil && !rec.Removed {
		return rec
	}
	for _, rec := range r.records {
		if !rec.Removed && rec.Spec.Name == id {
			return rec
		}
	}
	return nil
}

func (r *upstreamRuntime) save(rec *upstreamRecord) error {
	return writeUpstreamJSON(filepath.Join(r.root, rec.ID, "record.json"), rec)
}

func (r *upstreamRuntime) Create(ctx context.Context, spec *ContainerSpec) (string, error) {
	if spec == nil || spec.Upstream == nil {
		return r.Runtime.Create(ctx, spec)
	}
	if err := r.requireExecution(); err != nil {
		return "", err
	}
	if err := validateUpstream(spec); err != nil {
		return "", err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.record(spec.Name) != nil {
		return "", fmt.Errorf("upstream.logical_name_conflict: %s", spec.Name)
	}
	if _, err := r.Runtime.Inspect(ctx, spec.Name); err == nil {
		return "", fmt.Errorf("upstream.logical_name_conflict: %s", spec.Name)
	} else if !errdefs.IsNotFound(err) {
		return "", err
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	var frozen ContainerSpec
	if err := json.Unmarshal(data, &frozen); err != nil {
		return "", err
	}
	id := "~lmw-upstream-" + randomUpstreamID()
	rec := &upstreamRecord{ID: id, Spec: frozen, Observed: make(map[string]string), Workspace: r.reusableWorkspace(&frozen)}
	if _, err := os.Lstat(filepath.Join(rec.Workspace, "repository")); errors.Is(err, os.ErrNotExist) {
		previous, err := r.sourcePredecessor(&frozen)
		if err != nil {
			return "", err
		}
		if previous != nil {
			rec.SourceParent = previous.ID
		}
	} else if err != nil {
		return "", err
	}
	if err := os.MkdirAll(rec.Workspace, 0700); err != nil {
		return "", err
	}
	if err := writeUpstreamJSON(filepath.Join(rec.Workspace, "source-identity.json"), r.workspace(&frozen)); err != nil {
		return "", err
	}
	staging := filepath.Join(r.root, ".create-"+id)
	if err := os.Mkdir(staging, 0700); err != nil {
		return "", err
	}
	if err := writeUpstreamJSON(filepath.Join(staging, "record.json"), rec); err != nil {
		return "", err
	}
	if err := os.Rename(staging, filepath.Join(r.root, id)); err != nil {
		return "", err
	}
	r.records[id] = rec
	return id, nil
}

// currentOwner deliberately rejects ambiguous legacy records. Historical worker
// output cannot revive an installation whose authority was durably revoked.
func (r *upstreamRuntime) currentOwner(name, id string) *upstreamRecord {
	if id == "" {
		return nil
	}
	var owner *upstreamRecord
	for _, rec := range r.records {
		if rec.Superseded || rec.Observed[name] != id {
			continue
		}
		if owner != nil {
			return nil
		}
		owner = rec
	}
	return owner
}

func upstreamNamesOverlap(a, b *upstreamRecord) bool {
	for name := range upstreamContainerNames(a.Spec.Upstream) {
		for other := range upstreamContainerNames(b.Spec.Upstream) {
			if name == other {
				return true
			}
		}
	}
	return false
}

// acquire runs under mu after preflight. Revoke every overlapping installation
// durably BEFORE granting the successor anything. A partial write therefore
// leaves unowned evidence, never two destructive owners. Do not roll back these
// revocations on launch failure: the next start must pass preflight again.
func (r *upstreamRuntime) acquire(rec *upstreamRecord, before map[string]string) error {
	for _, other := range r.records {
		if other.ID == rec.ID || other.Superseded || !upstreamNamesOverlap(rec, other) {
			continue
		}
		other.Superseded, other.Armed = true, false
		if err := r.save(other); err != nil {
			return err
		}
	}
	rec.Superseded = false
	rec.Job = nil
	rec.Observed = make(map[string]string, len(before))
	for name, id := range before {
		rec.Observed[name] = id
	}
	if err := r.save(rec); err != nil {
		rec.Superseded = true
		return errors.Join(err, r.save(rec))
	}
	return nil
}

// preflight never stops/removes a container. A start may reuse a stopped
// predecessor only when its exact engine ID is in retained ownership evidence.
func (r *upstreamRuntime) preflight(ctx context.Context, rec *upstreamRecord, starting bool) (map[string]string, error) {
	before := make(map[string]string)
	for name := range upstreamContainerNames(rec.Spec.Upstream) {
		info, err := r.Runtime.Inspect(ctx, name)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		before[name] = info.ID
		if r.currentOwner(name, info.ID) == rec {
			if starting && rec.Spec.Upstream.ObserveOnly && !stoppedUpstreamContainer(info.State) {
				return nil, fmt.Errorf("upstream.foreign_container: %s (%s)", name, info.ID)
			}
			continue
		}
		owned := false
		if starting && stoppedUpstreamContainer(info.State) {
			other := r.currentOwner(name, info.ID)
			owned = other != nil && !r.jobAlive(other) && !other.Armed
		}
		if !owned {
			return nil, fmt.Errorf("upstream.foreign_container: %s (%s)", name, info.ID)
		}
	}
	// Prevent two authored launchers from racing for an absent shared name.
	if starting {
		for _, other := range r.records {
			if other.Superseded || !upstreamNamesOverlap(rec, other) {
				continue
			}
			if other.Job != nil {
				var status upstreamStatus
				if err := readUpstreamJSON(filepath.Join(other.Job.Dir, "status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				if !status.Done {
					return nil, fmt.Errorf("upstream.lifecycle_unsettled: %s", other.ID)
				}
			}
			if other.ID == rec.ID {
				continue
			}
			if r.jobAlive(other) || other.Armed {
				return nil, fmt.Errorf("upstream.container_reserved: %s", other.ID)
			}
			// Ownership is installation-wide, including auxiliary containers
			// and names absent from the successor's declaration.
			for name := range upstreamContainerNames(other.Spec.Upstream) {
				ci, err := r.Runtime.Inspect(ctx, name)
				if errdefs.IsNotFound(err) {
					continue
				}
				if err != nil {
					return nil, err
				}
				if !stoppedUpstreamContainer(ci.State) {
					return nil, fmt.Errorf("upstream.container_reserved: %s", name)
				}
			}
		}
	}
	return before, nil
}

func stoppedUpstreamContainer(state string) bool {
	return state == "exited" || state == "created" || state == "dead"
}

func (r *upstreamRuntime) jobAlive(rec *upstreamRecord) bool {
	return rec.Job != nil && upstreamProcessAlive(rec.Job.PID, rec.Job.ProcessIdentity)
}

func (r *upstreamRuntime) Start(ctx context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.record(id)
	if rec == nil {
		return r.baseStart(ctx, id)
	}
	if err := validateUpstream(&rec.Spec); err != nil {
		return err
	}
	info, err := r.inspect(ctx, rec)
	if err != nil {
		return err
	}
	if r.jobAlive(rec) || info.State == "running" {
		return nil
	}
	if rec.Armed {
		if info.Error != "" {
			return fmt.Errorf("%s", info.Error)
		}
		return nil
	}
	if err := r.requireExecution(); err != nil {
		return err
	}
	// Reconcile completed workers before consulting ownership; workers write
	// status, not the durable installation record, and may outlive the agent.
	for _, other := range r.records {
		if other.ID != rec.ID && upstreamNamesOverlap(rec, other) {
			if _, err := r.inspect(ctx, other); err != nil {
				return err
			}
		}
	}
	before, err := r.preflight(ctx, rec, true)
	if err != nil {
		return err
	}
	if rec.Spec.Upstream.ObserveOnly {
		setup := len(rec.Spec.Upstream.Install) != 0 || len(rec.Spec.Upstream.Configuration) != 0
		if !setup {
			if _, err := os.Stat(filepath.Join(rec.Workspace, "configuration-state.json")); err == nil {
				setup = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if docker, ok := r.Runtime.(*dockerRuntime); ok {
			socket := strings.TrimPrefix(docker.cli.DaemonHost(), "unix://")
			if socket != "/var/run/docker.sock" && socket != "/run/docker.sock" {
				return fmt.Errorf("upstream.observer_custom_daemon_unsupported: coordinator remote Docker endpoint cannot be bound by this observer")
			}
			if _, err := upstreamCommandEnvironment(os.Environ(), rec.Spec.Env, socket); err != nil {
				return err
			}
		}
		if err := r.acquire(rec, before); err != nil {
			return err
		}
		previousBefore, previousArmed, previousStopped, previousAttached := rec.Before, rec.Armed, rec.Stopped, rec.Attached
		rec.Before, rec.Armed, rec.Stopped = before, true, false
		rec.Attached = make(map[string]bool)
		if setup {
			if err := r.launch(rec, "observe", before); err != nil {
				rec.Before, rec.Armed, rec.Stopped, rec.Attached = previousBefore, previousArmed, previousStopped, previousAttached
				return errors.Join(err, r.save(rec))
			}
			return nil
		}
		if err := r.save(rec); err != nil {
			rec.Before, rec.Armed, rec.Stopped, rec.Attached = previousBefore, previousArmed, previousStopped, previousAttached
			return errors.Join(err, r.save(rec))
		}
		return nil
	}
	if err := r.acquire(rec, before); err != nil {
		return err
	}
	return r.launch(rec, "start", before)
}

func (r *upstreamRuntime) baseStart(ctx context.Context, id string) error {
	if strings.HasPrefix(id, "~lmw-upstream-") {
		return errdefs.NotFound(fmt.Errorf("upstream.installation_missing: %s", id))
	}
	return r.Runtime.Start(ctx, id)
}

func (r *upstreamRuntime) launch(rec *upstreamRecord, operation string, before map[string]string) (result error) {
	if err := r.requireExecution(); err != nil {
		return err
	}
	previousJob, previousStopped := rec.Job, rec.Stopped
	changed := false
	defer func() {
		if result != nil && changed {
			rec.Job, rec.Stopped = previousJob, previousStopped
			result = errors.Join(result, r.save(rec))
		}
	}()
	dir := filepath.Join(r.root, rec.ID, "job-"+randomUpstreamID())
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	socket := ""
	if docker, ok := r.Runtime.(*dockerRuntime); ok {
		socket = strings.TrimPrefix(docker.cli.DaemonHost(), "unix://")
	}
	if socket == "" {
		return fmt.Errorf("upstream.worker_requires_docker_runtime")
	}
	if _, err := upstreamCommandEnvironment(os.Environ(), rec.Spec.Env, socket); err != nil {
		return err
	}
	environment, err := upstreamCommandEnvironment(os.Environ(), nil, socket)
	if err != nil {
		return err
	}
	request := upstreamRequest{Spec: rec.Spec, Installation: rec.Workspace, RecordDir: filepath.Join(r.root, rec.ID), Operation: operation, Before: before, Socket: socket, SourceParent: rec.SourceParent}
	if err := writeUpstreamJSON(filepath.Join(dir, "request.json"), &request); err != nil {
		return err
	}
	stdout, err := os.OpenFile(filepath.Join(r.root, rec.ID, "stdout.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(r.root, rec.ID, "stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer stderr.Close()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, "upstream-worker", dir)
	cmd.Env = environment
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := configureUpstreamProcess(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	identity, err := upstreamProcessIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	changed = true
	rec.Job = &upstreamJob{Dir: dir, Operation: operation, PID: cmd.Process.Pid, ProcessIdentity: identity}
	rec.Stopped = false
	if err := r.save(rec); err != nil {
		_ = terminateUpstreamProcess(rec.Job.PID, identity, true)
		_ = cmd.Wait()
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), nil, 0600); err != nil {
		_ = terminateUpstreamProcess(rec.Job.PID, identity, true)
		_ = cmd.Wait()
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func (r *upstreamRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec := r.record(id)
	if rec == nil {
		if strings.HasPrefix(id, "~lmw-upstream-") {
			return errdefs.NotFound(fmt.Errorf("upstream.installation_missing"))
		}
		return r.Runtime.Stop(ctx, id, timeoutSeconds)
	}
	if err := r.requireExecution(); err != nil {
		return err
	}
	// A rejected start has no lifecycle or ownership to undo. In particular,
	// authored stop scripts must never run against the foreign containers that
	// prevented this installation from acquiring its names.
	if rec.Job == nil && !rec.Armed && len(rec.Observed) == 0 && len(rec.Before) == 0 && len(rec.Attached) == 0 {
		stopped := rec.Stopped
		rec.Stopped = true
		if err := r.save(rec); err != nil {
			rec.Stopped = stopped
			return err
		}
		return nil
	}
	if _, err := r.inspect(ctx, rec); err != nil {
		return err
	}
	if rec.Superseded {
		return nil
	}
	if rec.Spec.Upstream.ObserveOnly {
		return r.stopObserver(ctx, rec, timeoutSeconds)
	}
	if err := r.cancel(ctx, rec, timeoutSeconds); err != nil {
		return err
	}
	// Cancellation can race with a final observed container; import its durable
	// evidence before deciding whether the authored stop command is safe.
	if _, err := r.inspect(ctx, rec); err != nil {
		return err
	}
	before, err := r.preflight(ctx, rec, false)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(rec.Workspace, "repository", ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) && len(before) == 0 {
			rec.Stopped = true
			return r.save(rec)
		}
		return fmt.Errorf("upstream.source_missing: %w", err)
	}
	if err := r.launch(rec, "stop", before); err != nil {
		return err
	}
	if timeoutSeconds <= 0 {
		timeoutSeconds = 15
	}
	deadline := time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := r.inspect(ctx, rec)
		if err != nil {
			return err
		}
		if !r.jobAlive(rec) {
			if info.Error != "" {
				return fmt.Errorf("%s", info.Error)
			}
			for name := range upstreamContainerNames(rec.Spec.Upstream) {
				ci, e := r.Runtime.Inspect(ctx, name)
				if e != nil && !errdefs.IsNotFound(e) {
					return e
				}
				if e == nil && !stoppedUpstreamContainer(ci.State) {
					return fmt.Errorf("upstream.stop_incomplete: %s remains %s", name, ci.State)
				}
			}
			rec.Stopped = true
			return r.save(rec)
		}
		select {
		case <-ctx.Done():
			_ = r.cancel(context.Background(), rec, 1)
			return ctx.Err()
		case <-deadline.C:
			_ = r.cancel(context.Background(), rec, 1)
			return fmt.Errorf("upstream.stop_timeout")
		case <-ticker.C:
		}
	}
}

func (r *upstreamRuntime) cancel(ctx context.Context, rec *upstreamRecord, seconds int) error {
	if !r.jobAlive(rec) {
		return nil
	}
	if seconds <= 0 {
		seconds = 15
	}
	if err := terminateUpstreamProcess(rec.Job.PID, rec.Job.ProcessIdentity, false); err != nil {
		return err
	}
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for r.jobAlive(rec) {
		select {
		case <-ctx.Done():
			return terminateUpstreamProcess(rec.Job.PID, rec.Job.ProcessIdentity, true)
		case <-deadline.C:
			return terminateUpstreamProcess(rec.Job.PID, rec.Job.ProcessIdentity, true)
		case <-ticker.C:
		}
	}
	return nil
}

func (r *upstreamRuntime) Remove(ctx context.Context, id string, force bool) error {
	// Stop owns the mutex itself and must finish before ownership is released.
	r.mu.Lock()
	rec := r.record(id)
	if rec == nil {
		r.mu.Unlock()
		if strings.HasPrefix(id, "~lmw-upstream-") {
			return errdefs.NotFound(fmt.Errorf("upstream.installation_missing"))
		}
		return r.Runtime.Remove(ctx, id, force)
	}
	info, err := r.inspect(ctx, rec)
	active := r.jobAlive(rec) || rec.Armed || (info != nil && (info.State == "running" || info.State == "paused"))
	for name := range upstreamContainerNames(rec.Spec.Upstream) {
		ci, e := r.Runtime.Inspect(ctx, name)
		if e != nil && !errdefs.IsNotFound(e) {
			r.mu.Unlock()
			return e
		}
		if e == nil && r.currentOwner(name, ci.ID) == rec && !stoppedUpstreamContainer(ci.State) {
			active = true
		}
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	if active && !force {
		return fmt.Errorf("upstream.installation_active")
	}
	if active {
		if err := r.Stop(ctx, id, 15); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec = r.record(id)
	if rec == nil {
		return errdefs.NotFound(fmt.Errorf("upstream.installation_missing"))
	}
	if r.jobAlive(rec) || rec.Armed {
		return fmt.Errorf("upstream.installation_active")
	}
	if rec.Superseded {
		rec.Removed = true
		return r.save(rec)
	}
	for name := range upstreamContainerNames(rec.Spec.Upstream) {
		ci, e := r.Runtime.Inspect(ctx, name)
		if errdefs.IsNotFound(e) {
			continue
		}
		if e != nil {
			return e
		}
		if r.currentOwner(name, ci.ID) != rec {
			if owner := r.currentOwner(name, ci.ID); owner != nil {
				continue
			}
			return fmt.Errorf("upstream.remove_ownership_changed: %s", name)
		}
		if !stoppedUpstreamContainer(ci.State) {
			return fmt.Errorf("upstream.remove_container_active: %s", name)
		}
		if !rec.Spec.Upstream.ObserveOnly {
			if err := r.Runtime.Remove(ctx, ci.ID, false); err != nil {
				return err
			}
		}
	}
	rec.Removed, rec.Armed = true, false
	return r.save(rec)
}

func (r *upstreamRuntime) Inspect(ctx context.Context, id string) (*ContainerInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if rec := r.record(id); rec != nil {
		return r.inspect(ctx, rec)
	}
	if strings.HasPrefix(id, "~lmw-upstream-") {
		return nil, errdefs.NotFound(fmt.Errorf("upstream.installation_missing: %s", id))
	}
	return r.Runtime.Inspect(ctx, id)
}

func (r *upstreamRuntime) inspect(ctx context.Context, rec *upstreamRecord) (*ContainerInfo, error) {
	if rec.Superseded {
		return &ContainerInfo{ID: rec.ID, Name: rec.Spec.Name, State: "exited", Status: "exited", Labels: rec.Spec.Labels}, nil
	}
	if rec.Spec.Upstream.ObserveOnly {
		return r.inspectObserver(ctx, rec)
	}
	info := &ContainerInfo{ID: rec.ID, Name: rec.Spec.Name, State: "created", Labels: rec.Spec.Labels}
	status := upstreamStatus{}
	if rec.Job != nil {
		if err := readUpstreamJSON(filepath.Join(rec.Job.Dir, "status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if status.Done && status.Error == "" && rec.Job.Operation == "stop" && !rec.Stopped {
			rec.Stopped = true
			if err := r.save(rec); err != nil {
				return nil, err
			}
		}
		changed := false
		for name, id := range status.Observed {
			if rec.Observed[name] != id {
				rec.Observed[name] = id
				changed = true
			}
		}
		if changed {
			if err := r.save(rec); err != nil {
				return nil, err
			}
		}
		info.State = status.Phase
		if info.State == "" {
			info.State = "starting"
		}
		info.ExitCode, info.Error = status.ExitCode, status.Error
		if !status.Done && !r.jobAlive(rec) {
			info.State, info.Error = "dead", "upstream.supervisor_lost: lifecycle outcome unknown"
		}
	}
	if rec.Stopped {
		info.State = "exited"
	}
	allRunning := true
	for _, name := range rec.Spec.Upstream.Containers {
		ci, err := r.Runtime.Inspect(ctx, name)
		if errdefs.IsNotFound(err) {
			allRunning = false
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.currentOwner(name, ci.ID) != rec {
			allRunning = false
			// A stopped original can remain available for rollback while a
			// successor owns the authored names. Never adopt those IDs here.
			if rec.Job != nil && !rec.Stopped && !r.jobAlive(rec) {
				info.State, info.Error = "dead", "upstream.container_identity_changed: "+name
			}
			continue
		}
		info.Ports = append(info.Ports, ci.Ports...)
		info.OOMKilled = info.OOMKilled || ci.OOMKilled
		if ci.State != "running" {
			allRunning = false
			if status.Done && !rec.Stopped && status.Error == "" {
				info.State = ci.State
				info.ExitCode = ci.ExitCode
			}
		}
		if ci.Error != "" {
			info.Error = ci.Error
		}
	}
	for _, name := range rec.Spec.Upstream.AuxiliaryContainers {
		ci, err := r.Runtime.Inspect(ctx, name)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if r.currentOwner(name, ci.ID) != rec && rec.Job != nil && !rec.Stopped && !r.jobAlive(rec) {
			info.Error = "upstream.container_identity_changed: " + name
		}
	}
	if info.Error == "" && allRunning && (rec.Stopped || rec.Job == nil || rec.Job.Operation == "start") {
		info.State = "running"
	}
	if info.Error != "" {
		info.State = "dead"
	}
	if status.Done && status.Phase == "started" && !allRunning && info.State == "started" {
		info.State, info.Error = "exited", "upstream.containers_not_running"
	}
	info.Status = info.State
	return info, nil
}

func (r *upstreamRuntime) ListByLabel(ctx context.Context, key, value string) ([]ContainerInfo, error) {
	list, err := r.Runtime.ListByLabel(ctx, key, value)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.records))
	for id, rec := range r.records {
		if !rec.Removed && rec.Spec.Labels[key] == value {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		info, err := r.inspect(ctx, r.records[id])
		if err != nil {
			return nil, err
		}
		list = append(list, *info)
	}
	return list, nil
}

func (r *upstreamRuntime) LogsFollow(ctx context.Context, id string, stdout, stderr bool) (io.ReadCloser, error) {
	r.mu.Lock()
	rec := r.record(id)
	r.mu.Unlock()
	if rec == nil {
		if strings.HasPrefix(id, "~lmw-upstream-") {
			return nil, errdefs.NotFound(fmt.Errorf("upstream.installation_missing: %s", id))
		}
		return r.Runtime.LogsFollow(ctx, id, stdout, stderr)
	}
	paths := []string{}
	if stdout {
		paths = append(paths, filepath.Join(r.root, rec.ID, "stdout.log"))
	}
	if stderr {
		paths = append(paths, filepath.Join(r.root, rec.ID, "stderr.log"))
	}
	if stdout && rec.Spec.Upstream.LogFile != "" {
		paths = append(paths, filepath.Join(rec.Workspace, "repository", rec.Spec.Upstream.SourcePath, rec.Spec.Upstream.LogFile))
	}
	local := followUpstreamFiles(ctx, paths, r.root)
	return r.followLogs(ctx, rec.ID, local, stdout, stderr), nil
}

func (r *upstreamRuntime) LogsStreams(ctx context.Context, id string) (io.ReadCloser, io.ReadCloser, error) {
	r.mu.Lock()
	rec := r.record(id)
	r.mu.Unlock()
	if rec == nil {
		if strings.HasPrefix(id, "~lmw-upstream-") {
			return nil, nil, errdefs.NotFound(fmt.Errorf("upstream.installation_missing: %s", id))
		}
		return r.Runtime.LogsStreams(ctx, id)
	}
	stdout, err := r.LogsFollow(ctx, id, true, false)
	if err != nil {
		return nil, nil, err
	}
	stderr, err := r.LogsFollow(ctx, id, false, true)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func randomUpstreamID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(bytes[:])
}
func readUpstreamJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}
func writeUpstreamJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".state-")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func followUpstreamFiles(ctx context.Context, paths []string, root string) io.ReadCloser {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		defer writer.Close()
		offsets := make([]int64, len(paths))
		previous := make([]os.FileInfo, len(paths))
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			for i, path := range paths {
				relative, err := filepath.Rel(root, path)
				if err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				resolved, err := upstreamPath(root, relative, true)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				path = resolved
				file, err := os.Open(path)
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				if err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				stat, err := file.Stat()
				if err == nil && (stat.Size() < offsets[i] || (previous[i] != nil && !os.SameFile(previous[i], stat))) {
					offsets[i] = 0
				}
				previous[i] = stat
				_, err = file.Seek(offsets[i], io.SeekStart)
				if err == nil {
					var n int64
					n, err = io.Copy(writer, file)
					offsets[i] += n
				}
				_ = file.Close()
				if err != nil {
					_ = writer.CloseWithError(err)
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return &upstreamLogReader{PipeReader: reader, cancel: cancel}
}

type upstreamLogReader struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (r *upstreamLogReader) Close() error { r.cancel(); return r.PipeReader.Close() }

// Local supervisor output is available immediately; engine logs attach as soon
// as an authored container ID has been durably observed, including after restart.
func (r *upstreamRuntime) followLogs(ctx context.Context, id string, local io.ReadCloser, stdout, stderr bool) io.ReadCloser {
	reader, writer := io.Pipe()
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		_ = local.Close()
		_ = writer.CloseWithError(ctx.Err())
	}()
	go func() {
		_, err := io.Copy(writer, local)
		if err != nil && ctx.Err() == nil {
			_ = writer.CloseWithError(err)
			cancel()
		}
	}()
	go func() {
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		attached := make(map[string]bool)
		for {
			r.mu.Lock()
			rec := r.record(id)
			ids := []string{}
			if rec != nil {
				_, err := r.inspect(ctx, rec)
				if err != nil {
					r.mu.Unlock()
					_ = writer.CloseWithError(err)
					return
				}
				for name, observed := range rec.Observed {
					if r.currentOwner(name, observed) == rec && !attached[observed] {
						ids = append(ids, observed)
					}
				}
			}
			r.mu.Unlock()
			if rec == nil {
				return
			}
			for _, observed := range ids {
				stream, err := r.Runtime.LogsFollow(ctx, observed, stdout, stderr)
				if errdefs.IsNotFound(err) {
					attached[observed] = true
					continue
				}
				if err != nil {
					_ = writer.CloseWithError(err)
					return
				}
				attached[observed] = true
				go func() {
					defer stream.Close()
					done := make(chan struct{})
					defer close(done)
					go func() {
						select {
						case <-ctx.Done():
							_ = stream.Close()
						case <-done:
						}
					}()
					if _, err := io.Copy(writer, stream); err != nil && ctx.Err() == nil {
						_ = writer.CloseWithError(err)
						cancel()
					}
				}()
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return &upstreamLogReader{PipeReader: reader, cancel: cancel}
}

func upstreamContainerNames(u *UpstreamSpec) iter.Seq[string] {
	return func(yield func(string) bool) {
		for _, name := range u.Containers {
			if !yield(name) {
				return
			}
		}
		for _, name := range u.AuxiliaryContainers {
			if !yield(name) {
				return
			}
		}
	}
}

// legacyWorkspace retains the exact pre-configuration hash for old records.
func (r *upstreamRuntime) legacyWorkspace(spec *ContainerSpec) string {
	env := append([]string(nil), spec.Env...)
	sort.Strings(env)
	owner := spec.Labels[LabelDeployment]
	if owner == "" {
		owner = "run:" + spec.Labels[LabelRun]
	}
	identity, _ := json.Marshal(struct {
		Deployment string
		Recipe     string
		Rank       string
		Source     *UpstreamSpec
		Env        []string
	}{owner, spec.Labels[LabelRecipe], spec.Labels[LabelRank], spec.Upstream, env})
	hash := sha256.Sum256(identity)
	return filepath.Join(r.root, "installations", hex.EncodeToString(hash[:]))
}

// Settings, recipe digest and successor deployment IDs do not identify source
// installations. Named-container preflight still grants only one writer.
func (r *upstreamRuntime) workspace(spec *ContainerSpec) string {
	names := make([]string, 0, len(spec.Upstream.Containers)+len(spec.Upstream.AuxiliaryContainers))
	for name := range upstreamContainerNames(spec.Upstream) {
		names = append(names, name)
	}
	sort.Strings(names)
	identity, _ := json.Marshal(struct {
		URL, Revision, Path, Rank string
		Containers                []string
	}{spec.Upstream.SourceURL, strings.ToLower(spec.Upstream.Revision), filepath.ToSlash(filepath.Clean(spec.Upstream.SourcePath)), spec.Labels[LabelRank], names})
	hash := sha256.Sum256(identity)
	return filepath.Join(r.root, "installations", hex.EncodeToString(hash[:]))
}

func (r *upstreamRuntime) verifyWorkspace(rec *upstreamRecord) error {
	stable := r.workspace(&rec.Spec)
	if rec.Workspace == stable || rec.Workspace == r.legacyWorkspace(&rec.Spec) {
		return nil
	}
	// A successor may inherit a legacy location. The marker binds that location
	// to the same immutable source and exclusive authored container names.
	if filepath.Dir(rec.Workspace) == filepath.Join(r.root, "installations") && IsSHA256Digest("sha256:"+filepath.Base(rec.Workspace)) {
		var identity string
		if err := readUpstreamJSON(filepath.Join(rec.Workspace, "source-identity.json"), &identity); err == nil && identity == stable {
			return nil
		}
	}
	return fmt.Errorf("upstream.workspace_identity_invalid")
}

func (r *upstreamRuntime) reusableWorkspace(spec *ContainerSpec) string {
	stable := r.workspace(spec)
	var preferred, historical string
	for _, rec := range r.records {
		if r.workspace(&rec.Spec) != stable {
			continue
		}
		if !rec.Removed && !rec.Superseded && (preferred == "" || rec.Workspace < preferred) {
			preferred = rec.Workspace
		}
		if historical == "" || rec.Workspace < historical {
			historical = rec.Workspace
		}
	}
	if preferred != "" {
		return preferred
	}
	if historical != "" {
		return historical
	}
	return stable
}

func (r *upstreamRuntime) inspectObserver(ctx context.Context, rec *upstreamRecord) (*ContainerInfo, error) {
	info := &ContainerInfo{ID: rec.ID, Name: rec.Spec.Name, State: "created", Labels: rec.Spec.Labels}
	if rec.Armed {
		info.State = "observing"
	}
	if rec.Stopped {
		info.State = "exited"
	}
	running := rec.Armed
	required := make(map[string]bool, len(rec.Spec.Upstream.Containers))
	for _, name := range rec.Spec.Upstream.Containers {
		required[name] = true
	}
	changed := false
	setupPending := false
	setupPhase := "installing"
	settled := rec.Armed && !rec.Stopped && !r.jobAlive(rec)
	if rec.Job != nil && rec.Job.Operation != "observe" {
		settled = false
	}
	if rec.Job != nil && rec.Job.Operation == "observe" {
		var status upstreamStatus
		if err := readUpstreamJSON(filepath.Join(rec.Job.Dir, "status.json"), &status); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		settled = settled && status.Done && status.Error == "" && status.ExitCode == 0 && status.Phase == "observing"
		for name, id := range status.Observed {
			if rec.Attached == nil {
				rec.Attached = make(map[string]bool)
			}
			if rec.Observed[name] != id || !rec.Attached[name] {
				rec.Observed[name], rec.Attached[name], changed = id, true, true
			}
		}
		if !rec.Stopped {
			info.Error, info.ExitCode = status.Error, status.ExitCode
			setupPending = !status.Done
			if status.Phase != "" {
				setupPhase = status.Phase
			}
			if setupPending && !r.jobAlive(rec) {
				info.Error = "upstream.supervisor_lost: observer installation outcome unknown"
			}
		}
	}
	// Attachment must predate this inspection's engine discovery. An armed
	// observer still waiting for any required coordinator container retains its
	// reservation, even when setup is complete and all names are absent.
	for _, name := range rec.Spec.Upstream.Containers {
		if !rec.Attached[name] || rec.Observed[name] == "" {
			settled = false
		}
	}
	primarySettled := settled
	for name := range upstreamContainerNames(rec.Spec.Upstream) {
		ci, err := r.Runtime.Inspect(ctx, name)
		if errdefs.IsNotFound(err) {
			if required[name] {
				running = false
				if rec.Attached[name] && !rec.Stopped {
					info.State, info.Error = "exited", "upstream.observed_container_missing: "+name
				}
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		// Required names are visited before auxiliary names. Once an attached
		// primary cycle has ended, a newly appearing auxiliary is not evidence
		// of ownership; keep it foreign across subsequent inspections as well.
		if required[name] && !stoppedUpstreamContainer(ci.State) && r.currentOwner(name, ci.ID) == rec {
			primarySettled = false
		}
		// Include auxiliary names and check identity before discovery below can
		// attach anything new. Neither an active nor a foreign container proves
		// that the previous observation cycle has ended.
		if !stoppedUpstreamContainer(ci.State) || r.currentOwner(name, ci.ID) != rec {
			settled = false
		}
		// Before contains only IDs accepted by preflight and durably acquired
		// while stopped. Arming authorizes their transition to running too.
		if rec.Armed && !rec.Attached[name] && (required[name] || !primarySettled) && (ci.ID != rec.Before[name] || ci.State == "running") {
			if rec.Attached == nil {
				rec.Attached = make(map[string]bool)
			}
			rec.Observed[name], rec.Attached[name], changed = ci.ID, true, true
		}
		if !rec.Attached[name] {
			if required[name] {
				running = false
			}
			continue
		}
		if r.currentOwner(name, ci.ID) != rec {
			if !rec.Stopped {
				info.Error = "upstream.container_identity_changed: " + name
			}
			running = false
			continue
		}
		if required[name] {
			info.Ports = append(info.Ports, ci.Ports...)
			info.OOMKilled = info.OOMKilled || ci.OOMKilled
			if ci.State != "running" {
				running = false
				info.ExitCode = ci.ExitCode
				info.State = ci.State
			}
			if ci.Error != "" {
				info.Error = ci.Error
			}
		}
	}
	if settled {
		rec.Armed, rec.Stopped = false, true
		changed = true
	}
	if changed {
		if err := r.save(rec); err != nil {
			if settled {
				rec.Armed, rec.Stopped = true, false
			}
			return nil, err
		}
	}
	if rec.Stopped {
		// The terminal observation is durable, but IDs, attachment and job
		// evidence remain available for a guarded retry or successor handoff.
		info.State, info.Error = "exited", ""
	}
	if running && info.Error == "" {
		info.State = "running"
	}
	if setupPending && !rec.Stopped {
		info.State = setupPhase
	}
	if info.Error != "" {
		info.State = "dead"
	}
	info.Status = info.State
	return info, nil
}

func (r *upstreamRuntime) stopObserver(ctx context.Context, rec *upstreamRecord, seconds int) error {
	if rec.Superseded {
		return nil
	}
	if err := r.cancel(ctx, rec, seconds); err != nil {
		return err
	}
	if seconds <= 0 {
		seconds = 15
	}
	deadline := time.NewTimer(time.Duration(seconds) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := r.inspectObserver(ctx, rec); err != nil {
			return err
		}
		stopped := true
		for name := range upstreamContainerNames(rec.Spec.Upstream) {
			ci, err := r.Runtime.Inspect(ctx, name)
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil {
				return err
			}
			if r.currentOwner(name, ci.ID) != rec {
				if ci.ID == rec.Before[name] && stoppedUpstreamContainer(ci.State) {
					continue
				}
				return fmt.Errorf("upstream.foreign_container: %s", name)
			}
			if !stoppedUpstreamContainer(ci.State) {
				stopped = false
			}
		}
		if stopped {
			rec.Armed, rec.Stopped = false, true
			return r.save(rec)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("upstream.coordinator_stop_incomplete: observed containers remain active")
		case <-ticker.C:
		}
	}
}
