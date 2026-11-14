package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// upstreamStatusTimeout bounds one authored status query, which may itself
	// reach every rank (DSGB10's service.sh status SSHes to each worker).
	upstreamStatusTimeout = 60 * time.Second
	// upstreamStatusInterval separates background queries of one installation.
	upstreamStatusInterval = 10 * time.Second
	// upstreamStatusGrace is how long a started service may report not running
	// (an authored supervisor restarting its engine) before it is terminal.
	upstreamStatusGrace = 3 * time.Minute
)

// upstreamStatusResult is one authored status query: exit 0 means serving,
// anything else (including a timeout or a launch failure) means not running.
type upstreamStatusResult struct {
	ExitCode int
	Detail   string
	Err      error
}

func (s upstreamStatusResult) running() bool { return s.Err == nil && s.ExitCode == 0 }

func (s upstreamStatusResult) describe() string {
	text := fmt.Sprintf("status exited %d", s.ExitCode)
	if s.Err != nil {
		text = s.Err.Error()
	}
	if s.Detail != "" {
		text += ": " + s.Detail
	}
	return text
}

// upstreamProbe is the latest background status result for the service one
// start job launched. A new job discards it.
type upstreamProbe struct {
	job     string
	pending bool
	result  *upstreamStatusResult
	at      time.Time
	failing time.Time
}

// placementHolder is a non-coordinator rank without authored containers: its
// coordinator drives the host remotely and LMW runs or observes nothing there.
func placementHolder(u *UpstreamSpec) bool {
	return u.ObserveOnly && len(u.Containers) == 0
}

// startedService reports a completed, successful start of a status-observed
// service. Its liveness is the status query's; starting it again would only
// collide with the service it launched.
func (r *upstreamRuntime) startedService(rec *upstreamRecord) bool {
	if len(rec.Spec.Upstream.Status) == 0 || rec.Stopped || rec.Job == nil || rec.Job.Operation != "start" {
		return false
	}
	var status upstreamStatus
	if readUpstreamJSON(filepath.Join(rec.Job.Dir, "status.json"), &status) != nil {
		return false
	}
	return status.Done && status.Error == ""
}

// statusObservation never blocks inspection (which holds mu) on the authored
// query: results refresh in the background, one query per installation at a
// time. It returns the latest result, nil before the first one completes, and
// how long the service has continuously reported not running.
func (r *upstreamRuntime) statusObservation(rec *upstreamRecord) (*upstreamStatusResult, time.Duration) {
	if r.probes == nil {
		r.probes = make(map[string]*upstreamProbe)
	}
	probe := r.probes[rec.ID]
	if probe == nil || probe.job != rec.Job.Dir {
		probe = &upstreamProbe{job: rec.Job.Dir}
		r.probes[rec.ID] = probe
	}
	if !probe.pending && (probe.result == nil || time.Since(probe.at) >= r.statusInterval) {
		probe.pending = true
		spec, workspace, timeout := rec.Spec, rec.Workspace, r.statusTimeout
		go func() {
			result := r.queryStatus(&spec, workspace, timeout)
			r.mu.Lock()
			defer r.mu.Unlock()
			probe.pending = false
			probe.result, probe.at = &result, time.Now()
			if result.running() {
				probe.failing = time.Time{}
			} else if probe.failing.IsZero() {
				probe.failing = probe.at
			}
		}()
	}
	var failing time.Duration
	if !probe.failing.IsZero() {
		failing = time.Since(probe.failing)
	}
	return probe.result, failing
}

// queryStatus runs the status argv like an authored lifecycle command: in the
// retained checkout's source directory with the reviewed environment, bound
// to the Docker daemon this runtime observes.
func (r *upstreamRuntime) queryStatus(spec *ContainerSpec, workspace string, timeout time.Duration) upstreamStatusResult {
	working, err := upstreamPath(filepath.Join(workspace, "repository"), spec.Upstream.SourcePath, false)
	if err != nil {
		return upstreamStatusResult{ExitCode: -1, Err: err}
	}
	socket := ""
	if docker, ok := r.Runtime.(*dockerRuntime); ok {
		socket = strings.TrimPrefix(docker.cli.DaemonHost(), "unix://")
	}
	environment, err := upstreamCommandEnvironment(os.Environ(), spec.Env, socket)
	if err != nil {
		return upstreamStatusResult{ExitCode: -1, Err: err}
	}
	if timeout <= 0 {
		timeout = upstreamStatusTimeout
	}
	return runUpstreamStatus(context.Background(), spec.Upstream.Status, working, environment, timeout)
}

// runUpstreamStatus executes the authored status argv in its own process
// group, killing the whole group at the deadline so helpers (ssh, curl) cannot
// outlive it. Output is retained only as a short diagnostic.
func runUpstreamStatus(ctx context.Context, argv []string, working string, environment []string, timeout time.Duration) upstreamStatusResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd, err := upstreamAuthoredCommand(ctx, argv, working, environment)
	if err != nil {
		return upstreamStatusResult{ExitCode: -1, Err: err}
	}
	if err := configureUpstreamProcess(cmd); err != nil {
		return upstreamStatusResult{ExitCode: -1, Err: err}
	}
	output := &statusOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Cancel = func() error { return killUpstreamProcessGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	result := upstreamStatusResult{Detail: output.lastLine()}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		result.ExitCode, result.Err = -1, fmt.Errorf("upstream.status_timeout: %s did not answer within %s", argv[0], timeout)
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
	case err != nil:
		result.ExitCode, result.Err = -1, fmt.Errorf("upstream.status_failed: %w", err)
	}
	return result
}

// statusOutput keeps only the tail of the authored status output.
type statusOutput struct {
	mu   sync.Mutex
	tail []byte
}

const statusOutputLimit = 4096

func (o *statusOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.tail = append(o.tail, p...)
	if excess := len(o.tail) - statusOutputLimit; excess > 0 {
		o.tail = append(o.tail[:0], o.tail[excess:]...)
	}
	return len(p), nil
}

func (o *statusOutput) lastLine() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	lines := bytes.Split(bytes.TrimSpace(o.tail), []byte("\n"))
	line := strings.TrimSpace(string(lines[len(lines)-1]))
	if len(line) > 512 {
		line = line[:512]
	}
	return strings.ToValidUTF8(line, "")
}
