//go:build linux

package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func upstreamStatusTestSpec() *ContainerSpec {
	spec := upstreamTestSpec()
	spec.Upstream.Containers = []string{}
	spec.Upstream.Status = []string{"./status.sh"}
	return spec
}

func TestUpstreamStatusValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*UpstreamSpec)
		want string
	}{
		"status without containers":     {func(u *UpstreamSpec) {}, ""},
		"neither containers nor status": {func(u *UpstreamSpec) { u.Status = nil }, "upstream.observed_containers_required"},
		"auxiliary without primary":     {func(u *UpstreamSpec) { u.AuxiliaryContainers = []string{"builder"} }, "upstream.observed_containers_required"},
		"empty status argv":             {func(u *UpstreamSpec) { u.Status = []string{} }, "upstream.lifecycle_incomplete"},
		"blank status entrypoint":       {func(u *UpstreamSpec) { u.Status = []string{""} }, "upstream.lifecycle_incomplete"},
		"observer querying status":      {func(u *UpstreamSpec) { u.ObserveOnly = true }, "upstream.observer_status_invalid"},
		"placement holder":              {func(u *UpstreamSpec) { u.ObserveOnly, u.Status = true, nil }, ""},
		"placement holder with setup": {func(u *UpstreamSpec) {
			u.ObserveOnly, u.Status, u.Install = true, nil, [][]string{{"./install.sh"}}
		}, "upstream.placement_holder_executes"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := upstreamStatusTestSpec()
			tc.edit(spec.Upstream)
			err := validateUpstream(spec)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("validateUpstream = %v, want %q", err, tc.want)
			}
		})
	}
}

// statusWorkspace materializes the retained checkout an inspection queries.
func statusWorkspace(t *testing.T, rt *upstreamRuntime, id, script string) string {
	t.Helper()
	working := filepath.Join(rt.records[id].Workspace, "repository", "recipe")
	if err := os.MkdirAll(working, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(working, "status.sh"), []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return working
}

func waitUpstreamState(t *testing.T, rt *upstreamRuntime, id, want string) *ContainerInfo {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := rt.Inspect(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if info.State == want {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("state %q (%s), want %q", info.State, info.Error, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUpstreamStatusDrivesCoordinatorState(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	rt.statusInterval, rt.statusTimeout = 0, 2*time.Second
	spec := upstreamStatusTestSpec()
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing of this installation's runs before its start: never query or
	// adopt whatever the status command might report.
	working := statusWorkspace(t, rt, id, `printf 'probe\n' >> probes; test -f serving`)
	if err := os.WriteFile(filepath.Join(working, "serving"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "created" {
		t.Fatalf("unstarted status service claimed: %+v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(working, "probes")); !os.IsNotExist(err) {
		t.Fatal("status queried before this installation started its service")
	}

	upstreamTestCompletedJob(t, rt, rt.records[id], "start")
	info := waitUpstreamState(t, rt, id, "running")
	if info.Error != "" {
		t.Fatalf("serving status reported an error: %+v", info)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatalf("started status service relaunched: %v", err)
	}

	// Not running: a recovery window first, then terminal.
	if err := os.Remove(filepath.Join(working, "serving")); err != nil {
		t.Fatal(err)
	}
	info = waitUpstreamState(t, rt, id, "restarting")
	if info.ExitCode != 1 || info.Error != "" {
		t.Fatalf("recovering service misreported: %+v", info)
	}
	rt.mu.Lock()
	rt.statusGrace = 0
	rt.mu.Unlock()
	info = waitUpstreamState(t, rt, id, "dead")
	if !strings.Contains(info.Error, "upstream.status_not_running: status exited 1") {
		t.Fatalf("terminal status lacks evidence: %+v", info)
	}
	if err := os.WriteFile(filepath.Join(working, "serving"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitUpstreamState(t, rt, id, "running")

	// A hung query is not running; its helpers die with it.
	statusWorkspace(t, rt, id, `sleep 30 & wait`)
	info = waitUpstreamState(t, rt, id, "dead")
	if !strings.Contains(info.Error, "upstream.status_timeout") {
		t.Fatalf("hung status lacks timeout evidence: %+v", info)
	}

	// After an authored stop nothing is queried and the service is exited.
	rt.mu.Lock()
	rt.records[id].Stopped = true
	rt.mu.Unlock()
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "exited" {
		t.Fatalf("stopped status service: %+v, %v", info, err)
	}
	if len(base.started) != 0 || len(base.stopped) != 0 {
		t.Fatal("status service touched Docker containers")
	}
}

func TestUpstreamStatusOwnsStartAndProvesStop(t *testing.T) {
	ctx := context.Background()
	repo, revision := upstreamFixture(t, "true")
	base := &upstreamTestRuntime{}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamStatusTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repo, revision
	spec.Upstream.Start = []string{"./start.sh", "argument"}
	spec.Env = []string{"VALUE=reviewed"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	workspace := rt.records[id].Workspace
	working := filepath.Join(workspace, "repository", "recipe")
	run := func(operation string) error {
		return executeUpstreamJob(ctx, base, t.TempDir(), &upstreamRequest{Spec: *spec, Installation: workspace, Operation: operation}, io.Discard, io.Discard)
	}
	if err := run("start"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(working, "started")); err != nil {
		t.Fatal("authored start did not run", err)
	}
	if err := os.WriteFile(filepath.Join(working, "argv.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("start"); err == nil || !strings.Contains(err.Error(), "upstream.foreign_service") {
		t.Fatalf("start adopted a running service: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(working, "argv.txt")); len(data) != 0 {
		t.Fatal("start ran over a running service")
	}
	if err := os.WriteFile(filepath.Join(working, "stuck"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run("stop"); err == nil || !strings.Contains(err.Error(), "upstream.stop_incomplete") {
		t.Fatalf("stop accepted while status still serves: %v", err)
	}
	if err := os.Remove(filepath.Join(working, "stuck")); err != nil {
		t.Fatal(err)
	}
	if err := run("stop"); err != nil {
		t.Fatal(err)
	}
	if err := run("start"); err != nil {
		t.Fatalf("restart after a proven stop: %v", err)
	}
}

func TestUpstreamPlacementHolderRunsNothing(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	spec := upstreamTestSpec()
	spec.Labels[LabelRank] = "2"
	spec.Upstream.ObserveOnly, spec.Upstream.Containers = true, []string{}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "created" {
		t.Fatalf("unarmed holder: %+v, %v", info, err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	if rt.records[id].Job != nil {
		t.Fatal("placement holder launched a lifecycle job")
	}
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "running" {
		t.Fatalf("armed holder must not block the coordinator's health: %+v, %v", info, err)
	}
	// Restarted agents keep the reservation without settling it.
	rt = upstreamTestWrapper(t, base, root)
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "running" {
		t.Fatalf("holder lost its reservation across restart: %+v, %v", info, err)
	}
	if err := PrepareUpstreamSource(ctx, rt, spec); err != nil {
		t.Fatalf("holder source preparation: %v", err)
	}
	if err := rt.Stop(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if info, err := rt.Inspect(ctx, id); err != nil || info.State != "exited" {
		t.Fatalf("stopped holder: %+v, %v", info, err)
	}
	if err := rt.Remove(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(rt.records[id].Workspace, "repository")); !os.IsNotExist(err) {
		t.Fatalf("placement holder materialized source: %v", err)
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("placement holder manipulated Docker")
	}
}

func TestUpstreamStatusOutputKeepsLastLine(t *testing.T) {
	output := &statusOutput{}
	_, _ = output.Write(bytes.Repeat([]byte("x"), 2*statusOutputLimit))
	_, _ = output.Write([]byte("\nservice: not running\n  last: failed\n"))
	if got := output.lastLine(); got != "last: failed" {
		t.Fatalf("lastLine = %q", got)
	}
	if len(output.tail) > statusOutputLimit {
		t.Fatal("status output is unbounded")
	}
}
