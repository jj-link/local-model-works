//go:build linux

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/errdefs"
)

type upstreamTestRuntime struct {
	Runtime
	mu         sync.Mutex
	containers map[string]ContainerInfo
	inspect    func(string) (*ContainerInfo, error)
	removed    []string
	started    []string
	stopped    []string
}

func (f *upstreamTestRuntime) Inspect(ctx context.Context, name string) (*ContainerInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inspect != nil {
		return f.inspect(name)
	}
	if info, ok := f.containers[name]; ok {
		return &info, nil
	}
	for _, info := range f.containers {
		if info.ID == name {
			return &info, nil
		}
	}
	return nil, errdefs.NotFound(fmt.Errorf("missing %s", name))
}
func (f *upstreamTestRuntime) ListByLabel(context.Context, string, string) ([]ContainerInfo, error) {
	return nil, nil
}
func (f *upstreamTestRuntime) LogsFollow(context.Context, string, bool, bool) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}
func (f *upstreamTestRuntime) Remove(ctx context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	for name, info := range f.containers {
		if info.ID == id {
			delete(f.containers, name)
		}
	}
	return nil
}
func (f *upstreamTestRuntime) Start(ctx context.Context, id string) error {
	f.started = append(f.started, id)
	return nil
}
func (f *upstreamTestRuntime) Stop(ctx context.Context, id string, seconds int) error {
	f.stopped = append(f.stopped, id)
	return nil
}

func upstreamTestSpec() *ContainerSpec {
	return &ContainerSpec{
		Name: "reviewed-run-one", Labels: ManagedLabels("deployment", "run-one", "sha256:"+strings.Repeat("a", 64), "1.0.0", 0, "serving"),
		Upstream: &UpstreamSpec{Approved: true, SourceURL: "https://example.invalid/authored.git", Revision: strings.Repeat("a", 40), SourcePath: "recipe", Start: []string{"./start.sh"}, Stop: []string{"./stop.sh"}, Containers: []string{"authored"}},
	}
}
func upstreamTestWrapper(t *testing.T, base Runtime, root string) *upstreamRuntime {
	t.Helper()
	rt, err := WithUpstream(base, root, true)
	if err != nil {
		t.Fatal(err)
	}
	return rt.(*upstreamRuntime)
}

func TestUpstreamAuthorityRevocationRetainsOwnership(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	enabled := upstreamTestWrapper(t, base, root)
	id, err := enabled.Create(ctx, upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	rec := enabled.records[id]
	rec.Observed["authored"] = "owned-engine-id"
	if err := enabled.save(rec); err != nil {
		t.Fatal(err)
	}
	base.containers["authored"] = ContainerInfo{ID: "owned-engine-id", Name: "authored", State: "running"}
	logText := "retained lifecycle output\n"
	if err := os.WriteFile(filepath.Join(root, "upstream", id, "stdout.log"), []byte(logText), 0600); err != nil {
		t.Fatal(err)
	}
	disabled, err := WithUpstream(base, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if UpstreamEnabled(disabled) || UpstreamEnabled(base) || !UpstreamEnabled(enabled) {
		t.Fatal("capability does not reflect node authority")
	}
	info, err := disabled.Inspect(ctx, id)
	if err != nil || info.State != "running" {
		t.Fatalf("retained service hidden after revocation: %+v %v", info, err)
	}
	list, err := disabled.ListByLabel(ctx, LabelRun, "run-one")
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("retained ownership missing from inventory: %+v %v", list, err)
	}
	logs, err := disabled.LogsFollow(ctx, id, true, false)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(logText))
	_, err = io.ReadFull(logs, got)
	_ = logs.Close()
	if err != nil || string(got) != logText {
		t.Fatalf("retained logs unavailable: %q %v", got, err)
	}
	if err := disabled.Start(ctx, id); err != nil {
		t.Fatalf("already-running approved service was disrupted: %v", err)
	}
	if err := disabled.Stop(ctx, id, 1); err == nil || !strings.Contains(err.Error(), "execution_disabled") {
		t.Fatalf("disabled Stop pretended success: %v", err)
	}
	if err := disabled.Remove(ctx, id, true); err == nil {
		t.Fatal("active retained installation removed while execution disabled")
	}
	info, err = disabled.Inspect(ctx, id)
	if err != nil || info.State != "running" {
		t.Fatalf("revocation cancelled retained service: %+v %v", info, err)
	}
	base.containers["authored"] = ContainerInfo{ID: "owned-engine-id", Name: "authored", State: "exited"}
	if err := disabled.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "execution_disabled") {
		t.Fatalf("disabled host launch accepted: %v", err)
	}
	next := upstreamTestSpec()
	next.Name = "another-review"
	if _, err := disabled.Create(ctx, next); err == nil || !strings.Contains(err.Error(), "execution_disabled") {
		t.Fatalf("disabled Create accepted: %v", err)
	}
	if err := disabled.PrepareHost(ctx, next); err == nil {
		t.Fatal("disabled host preparation accepted")
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("logical upstream IDs fell through to native runtime")
	}
	if err := disabled.Start(ctx, "ordinary"); err != nil {
		t.Fatal(err)
	}
	if err := disabled.Stop(ctx, "ordinary", 1); err != nil {
		t.Fatal(err)
	}
	if len(base.started) != 1 || base.started[0] != "ordinary" || len(base.stopped) != 1 || base.stopped[0] != "ordinary" {
		t.Fatal("revocation disabled native container lifecycle")
	}
	if _, err := disabled.LogsFollow(ctx, "~lmw-upstream-missing", true, true); !errdefs.IsNotFound(err) {
		t.Fatalf("missing upstream log ownership fell through: %v", err)
	}
	if _, _, err := disabled.LogsStreams(ctx, "~lmw-upstream-missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("missing upstream stream ownership fell through: %v", err)
	}
}

func TestUpstreamApprovalAndCollisionBoundaries(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamTestSpec()
	spec.Upstream.Approved = false
	if _, err := rt.Create(ctx, spec); err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("unapproved host execution accepted: %v", err)
	}
	spec.Upstream.Approved = true
	spec.Labels[LabelManaged] = "false"
	if _, err := rt.Create(ctx, spec); err == nil {
		t.Fatal("unmanaged host execution accepted")
	}
	spec.Labels[LabelManaged] = "true"
	spec.Upstream.AuxiliaryContainers = []string{"authored-builder"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	base.containers["authored-builder"] = ContainerInfo{ID: "foreign", Name: "authored-builder", State: "exited"}
	if err := rt.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "foreign_container") {
		t.Fatalf("foreign auxiliary container accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.records[id].Workspace, "repository")); !os.IsNotExist(err) {
		t.Fatalf("collision executed checkout: %v", err)
	}
	if err := rt.Start(ctx, "ordinary-container"); err != nil {
		t.Fatal(err)
	}
	if len(base.started) != 1 || base.started[0] != "ordinary-container" {
		t.Fatalf("non-upstream Start was not preserved: %v", base.started)
	}
}

func TestUpstreamStoppedOwnershipAndRollback(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	oldSpec := upstreamTestSpec()
	oldID, err := rt.Create(ctx, oldSpec)
	if err != nil {
		t.Fatal(err)
	}
	old := rt.records[oldID]
	old.Observed["authored"], old.Stopped = "old-id", true
	if err := rt.save(old); err != nil {
		t.Fatal(err)
	}
	base.containers["authored"] = ContainerInfo{ID: "old-id", Name: "authored", State: "exited"}
	nextSpec := upstreamTestSpec()
	nextSpec.Name, nextSpec.Labels[LabelRun] = "reviewed-run-two", "run-two"
	nextID, err := rt.Create(ctx, nextSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.preflight(ctx, rt.records[nextID], true); err != nil {
		t.Fatalf("owned stopped predecessor rejected: %v", err)
	}
	base.containers["authored"] = ContainerInfo{ID: "old-id", Name: "authored", State: "running"}
	if _, err := rt.preflight(ctx, rt.records[nextID], true); err == nil {
		t.Fatal("running predecessor accepted")
	}
	base.containers["authored"] = ContainerInfo{ID: "new-id", Name: "authored", State: "exited"}
	next := rt.records[nextID]
	next.Observed["authored"], next.Stopped = "new-id", true
	if err := rt.save(next); err != nil {
		t.Fatal(err)
	}
	restarted := upstreamTestWrapper(t, base, root)
	if _, err := restarted.preflight(ctx, restarted.records[oldID], true); err != nil {
		t.Fatalf("original stopped installation cannot restore: %v", err)
	}
	if err := restarted.Remove(ctx, oldID, false); err != nil {
		t.Fatal(err)
	}
	if len(base.removed) != 0 {
		t.Fatalf("removing retained original touched successor: %v", base.removed)
	}
}

func TestUpstreamCreatedReplacementDoesNotClaimPredecessorFailure(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	previous := upstreamTestSpec()
	previous.Upstream.AuxiliaryContainers = []string{"builder"}
	previousID, err := rt.Create(ctx, previous)
	if err != nil {
		t.Fatal(err)
	}
	old := rt.records[previousID]
	old.Stopped = true
	for _, name := range []string{"authored", "builder"} {
		old.Observed[name] = name + "-previous"
		base.containers[name] = ContainerInfo{ID: name + "-previous", Name: name, State: "exited", ExitCode: 1, Error: "previous run failed", OOMKilled: true}
	}
	if err := rt.save(old); err != nil {
		t.Fatal(err)
	}
	next := upstreamTestSpec()
	next.Name, next.Labels[LabelRun] = "reviewed-run-two", "run-two"
	next.Upstream.AuxiliaryContainers = []string{"builder"}
	nextID, err := rt.Create(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	info, err := rt.Inspect(ctx, nextID)
	if err != nil || info.State != "created" || info.Error != "" || info.OOMKilled {
		t.Fatalf("unstarted replacement inherited predecessor state: %+v, %v", info, err)
	}
}

func TestUpstreamObserverArmingPersistenceAndChangedIDs(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	spec := upstreamTestSpec()
	spec.Upstream.ObserveOnly = true
	spec.Upstream.AuxiliaryContainers = []string{"optional-builder"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, id); err != nil {
		t.Fatal(err)
	}
	info, err := rt.Inspect(ctx, id)
	if err != nil || info.State != "observing" {
		t.Fatalf("observer barrier was not armed without claiming health: %+v, %v", info, err)
	}
	base.containers["authored"] = ContainerInfo{ID: "coordinator-id", Name: "authored", State: "running"}
	info, err = rt.Inspect(ctx, id)
	if err != nil || info.State != "running" {
		t.Fatalf("coordinator container not observed: %+v, %v", info, err)
	}
	rt = upstreamTestWrapper(t, base, root)
	base.containers["authored"] = ContainerInfo{ID: "foreign-replacement", Name: "authored", State: "running"}
	info, err = rt.Inspect(ctx, id)
	if err != nil || info.State != "dead" || !strings.Contains(info.Error, "identity_changed") {
		t.Fatalf("replacement silently adopted: %+v, %v", info, err)
	}
	if err := rt.Stop(ctx, id, 1); err == nil {
		t.Fatal("observer stopped a foreign replacement")
	}
	base.containers["authored"] = ContainerInfo{ID: "coordinator-id", Name: "authored", State: "exited"}
	if err := rt.Stop(ctx, id, 1); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("observer manipulated coordinator-owned containers")
	}
	if _, err := os.Stat(filepath.Join(rt.records[id].Workspace, "repository")); !os.IsNotExist(err) {
		t.Fatalf("observer materialized executable source: %v", err)
	}
}

// Seed a completed durable supervisor, including the status that an agent
// restart would otherwise import again after authority has changed hands.
func upstreamTestCompletedJob(t *testing.T, rt *upstreamRuntime, rec *upstreamRecord, operation string) {
	t.Helper()
	dir := filepath.Join(rt.root, rec.ID, "job-"+randomUpstreamID())
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	status := upstreamStatus{Phase: "started", Done: true, Observed: rec.Observed}
	if err := writeUpstreamJSON(filepath.Join(dir, "status.json"), &status); err != nil {
		t.Fatal(err)
	}
	rec.Job = &upstreamJob{Dir: dir, Operation: operation}
	if err := rt.save(rec); err != nil {
		t.Fatal(err)
	}
}

func TestUpstreamRetainedIDHandoffRevokesPredecessor(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			ctx := context.Background()
			base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
			root := t.TempDir()
			rt := upstreamTestWrapper(t, base, root)
			oldID, err := rt.Create(ctx, upstreamTestSpec())
			if err != nil {
				t.Fatal(err)
			}
			old := rt.records[oldID]
			old.Observed["authored"], old.Stopped = "retained-id", true
			upstreamTestCompletedJob(t, rt, old, "start")
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
			nextSpec := upstreamTestSpec()
			nextSpec.Name, nextSpec.Labels[LabelRun] = "reviewed-run-two", "run-two"
			nextID, err := rt.Create(ctx, nextSpec)
			if err != nil {
				t.Fatal(err)
			}
			next := rt.records[nextID]
			// Exercise the same locked, durable handoff used before launching
			// an authored supervisor, without executing host code in this test.
			rt.mu.Lock()
			before, err := rt.preflight(ctx, next, true)
			if err == nil {
				err = rt.acquire(next, before)
			}
			rt.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			upstreamTestCompletedJob(t, rt, next, "start")
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "running"}
			for pass := range 2 {
				if pass == 1 {
					rt = upstreamTestWrapper(t, base, root)
				}
				info, err := rt.Inspect(ctx, oldID)
				if err != nil || info.State != "exited" {
					t.Fatalf("predecessor claimed successor after pass %d: %+v %v", pass, info, err)
				}
				info, err = rt.Inspect(ctx, nextID)
				if err != nil || info.State != "running" {
					t.Fatalf("successor lost reused engine ID: %+v %v", info, err)
				}
				if err := rt.Start(ctx, oldID); err == nil {
					t.Fatal("predecessor stole a live successor")
				}
				if err := rt.Stop(ctx, oldID, 1); err != nil {
					t.Fatalf("retained predecessor stop: %v", err)
				}
			}
			if err := rt.Remove(ctx, oldID, force); err != nil {
				t.Fatal(err)
			}
			info, err := rt.Inspect(ctx, nextID)
			if err != nil || info.State != "running" {
				t.Fatalf("predecessor cleanup affected successor: %+v %v", info, err)
			}
			if len(base.removed) != 0 || len(base.stopped) != 0 {
				t.Fatalf("cleanup touched reused engine: removed=%v stopped=%v", base.removed, base.stopped)
			}
			if _, err := os.Stat(old.Workspace); err != nil {
				t.Fatalf("retained installation lost: %v", err)
			}
			rt = upstreamTestWrapper(t, base, root)
			if rt.records[oldID].Observed["authored"] != "retained-id" {
				t.Fatal("historical installation evidence was erased")
			}
			info, err = rt.Inspect(ctx, nextID)
			if err != nil || info.State != "running" {
				t.Fatalf("cleanup did not survive restart: %+v %v", info, err)
			}
		})
	}
}

func TestUpstreamObserverRetainedIDRestarts(t *testing.T) {
	for _, successor := range []bool{false, true} {
		t.Run(fmt.Sprintf("successor=%t", successor), func(t *testing.T) {
			ctx := context.Background()
			base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
			root := t.TempDir()
			rt := upstreamTestWrapper(t, base, root)
			spec := upstreamTestSpec()
			spec.Upstream.ObserveOnly = true
			oldID, err := rt.Create(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			if err := rt.Start(ctx, oldID); err != nil {
				t.Fatal(err)
			}
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "running"}
			if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "running" {
				t.Fatalf("initial observation: %+v %v", info, err)
			}
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
			if err := rt.Stop(ctx, oldID, 1); err != nil {
				t.Fatal(err)
			}
			// A retained setup status must not attach a new observation cycle
			// before its accepted stopped container actually starts again.
			upstreamTestCompletedJob(t, rt, rt.records[oldID], "observe")
			rt = upstreamTestWrapper(t, base, root)
			id := oldID
			if successor {
				nextSpec := upstreamTestSpec()
				nextSpec.Name, nextSpec.Labels[LabelRun] = "observer-successor", "run-two"
				nextSpec.Upstream.ObserveOnly = true
				id, err = rt.Create(ctx, nextSpec)
				if err != nil {
					t.Fatal(err)
				}
			}
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "running"}
			if err := rt.Start(ctx, id); err == nil {
				t.Fatal("unarmed observer adopted an already-running retained ID")
			}
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
			if err := rt.Start(ctx, id); err != nil {
				t.Fatal(err)
			}
			rt = upstreamTestWrapper(t, base, root)
			if info, err := rt.Inspect(ctx, id); err != nil || info.State != "observing" {
				t.Fatalf("stopped retained ID claimed readiness: %+v %v", info, err)
			}
			competing := upstreamTestSpec()
			competing.Name = "competing-observer"
			competing.Upstream.ObserveOnly = true
			competingID, err := rt.Create(ctx, competing)
			if err != nil {
				t.Fatal(err)
			}
			if err := rt.Start(ctx, competingID); err == nil {
				t.Fatal("competing observer stole armed retained ID")
			}
			base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "running"}
			// Lose the agent before it sees the stopped-to-running transition.
			rt = upstreamTestWrapper(t, base, root)
			if info, err := rt.Inspect(ctx, id); err != nil || info.State != "running" {
				t.Fatalf("same-ID restart never attached: %+v %v", info, err)
			}
			if successor {
				if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "exited" {
					t.Fatalf("historical observer claimed successor: %+v %v", info, err)
				}
				if err := rt.Stop(ctx, oldID, 1); err != nil {
					t.Fatal(err)
				}
				if err := rt.Remove(ctx, oldID, true); err != nil {
					t.Fatal(err)
				}
			}
			rt = upstreamTestWrapper(t, base, root)
			if info, err := rt.Inspect(ctx, id); err != nil || info.State != "running" {
				t.Fatalf("attachment was not durable: %+v %v", info, err)
			}
			base.containers["authored"] = ContainerInfo{ID: "unrelated-live-id", Name: "authored", State: "running"}
			if info, err := rt.Inspect(ctx, id); err != nil || info.State != "dead" {
				t.Fatalf("unrelated live ID was adopted: %+v %v", info, err)
			}
			if err := rt.Stop(ctx, id, 1); err == nil {
				t.Fatal("foreign replacement accepted by observer stop")
			}
			if err := rt.Remove(ctx, id, true); err == nil {
				t.Fatal("foreign replacement accepted by observer remove")
			}
			if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
				t.Fatal("observer executed native lifecycle")
			}
		})
	}
}

func TestUpstreamNeverStartedStopIgnoresForeignContainers(t *testing.T) {
	for _, observer := range []bool{false, true} {
		t.Run(fmt.Sprintf("observer=%t", observer), func(t *testing.T) {
			ctx := context.Background()
			base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
			root := t.TempDir()
			rt := upstreamTestWrapper(t, base, root)
			spec := upstreamTestSpec()
			spec.Upstream.ObserveOnly = observer
			id, err := rt.Create(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			// A shared source workspace does not grant container ownership or
			// authorize this installation's authored stop command.
			if err := os.MkdirAll(filepath.Join(rt.records[id].Workspace, "repository", ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			base.containers["authored"] = ContainerInfo{ID: "foreign-id", Name: "authored", State: "running"}
			if err := rt.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "foreign_container") {
				t.Fatalf("foreign container did not reject start: %v", err)
			}
			for pass := range 2 {
				if pass == 1 {
					rt = upstreamTestWrapper(t, base, root)
				}
				if err := rt.Stop(ctx, id, 1); err != nil {
					t.Fatalf("never-started logical stop failed on pass %d: %v", pass, err)
				}
				info, err := rt.Inspect(ctx, id)
				if err != nil || info.State != "exited" || info.Error != "" {
					t.Fatalf("logical stop did not persist: %+v %v", info, err)
				}
				if rt.records[id].Job != nil {
					t.Fatal("logical stop launched an authored command")
				}
				if err := rt.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "foreign_container") {
					t.Fatalf("logical stop granted authority over a foreign container: %v", err)
				}
			}
			if base.containers["authored"].ID != "foreign-id" || base.containers["authored"].State != "running" ||
				len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
				t.Fatal("logical stop touched the foreign container")
			}
		})
	}
}

func TestUpstreamLogicalStopRequiresNoLifecycleOrOwnership(t *testing.T) {
	for _, evidence := range []string{"job", "observed", "before", "attached", "armed"} {
		t.Run(evidence, func(t *testing.T) {
			ctx := context.Background()
			base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
			root := t.TempDir()
			rt := upstreamTestWrapper(t, base, root)
			id, err := rt.Create(ctx, upstreamTestSpec())
			if err != nil {
				t.Fatal(err)
			}
			rec := rt.records[id]
			switch evidence {
			case "job":
				upstreamTestCompletedJob(t, rt, rec, "start")
			case "observed":
				rec.Observed["authored"] = "previous-id"
			case "before":
				rec.Before = map[string]string{"authored": "previous-id"}
			case "attached":
				rec.Attached = map[string]bool{"authored": true}
			case "armed":
				rec.Armed = true
			}
			if err := rt.save(rec); err != nil {
				t.Fatal(err)
			}
			base.containers["authored"] = ContainerInfo{ID: "foreign-id", Name: "authored", State: "running"}
			rt = upstreamTestWrapper(t, base, root)
			if err := rt.Stop(ctx, id, 1); err == nil || !strings.Contains(err.Error(), "foreign_container") {
				t.Fatalf("prior lifecycle or ownership bypassed stop preflight: %v", err)
			}
			if rt.records[id].Stopped || len(base.stopped) != 0 || len(base.removed) != 0 {
				t.Fatal("rejected stop reported completion or touched foreign containers")
			}
		})
	}
}

func TestUpstreamTerminalObserverReleasesReservation(t *testing.T) {
	for _, successor := range []bool{false, true} {
		for _, state := range []string{"missing", "exited", "created", "dead"} {
			t.Run(fmt.Sprintf("successor=%t/%s", successor, state), func(t *testing.T) {
				ctx := context.Background()
				base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
				root := t.TempDir()
				rt := upstreamTestWrapper(t, base, root)
				spec := upstreamTestSpec()
				spec.Upstream.ObserveOnly = true
				spec.Upstream.AuxiliaryContainers = []string{"builder"}
				oldID, err := rt.Create(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := rt.Start(ctx, oldID); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"authored", "builder"} {
					base.containers[name] = ContainerInfo{ID: name + "-id", Name: name, State: "running"}
				}
				if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "running" {
					t.Fatalf("initial attachment failed: %+v %v", info, err)
				}
				old := rt.records[oldID]
				upstreamTestCompletedJob(t, rt, old, "observe")
				status := upstreamStatus{Phase: "observing", Done: true, Observed: old.Observed}
				if err := writeUpstreamJSON(filepath.Join(old.Job.Dir, "status.json"), &status); err != nil {
					t.Fatal(err)
				}
				for name, ci := range base.containers {
					if state == "missing" {
						delete(base.containers, name)
					} else {
						ci.State = state
						base.containers[name] = ci
					}
				}
				rt = upstreamTestWrapper(t, base, root)
				if successor {
					// Exercise Start's predecessor reconciliation directly,
					// without requiring an inventory/Inspect call first.
					next := upstreamTestSpec()
					next.Name, next.Upstream.ObserveOnly = "observer-successor", true
					nextID, err := rt.Create(ctx, next)
					if err != nil {
						t.Fatal(err)
					}
					if err := rt.Start(ctx, nextID); err != nil {
						t.Fatalf("terminal predecessor still reserved names: %v", err)
					}
				} else {
					if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "exited" || info.Error != "" {
						t.Fatalf("terminal observer did not settle: %+v %v", info, err)
					}
				}
				rt = upstreamTestWrapper(t, base, root)
				old = rt.records[oldID]
				if old.Armed || !old.Stopped || old.Observed["authored"] != "authored-id" ||
					!old.Attached["authored"] || old.Job == nil {
					t.Fatalf("retirement lost durable terminal state or historical evidence: %+v", old)
				}
				if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "exited" {
					t.Fatalf("terminal state did not survive reload: %+v %v", info, err)
				}
				if !successor {
					if err := rt.Start(ctx, oldID); err != nil {
						t.Fatalf("terminal observer could not retry: %v", err)
					}
					if info, err := rt.Inspect(ctx, oldID); err != nil || info.State != "observing" {
						t.Fatalf("retry did not preserve a fresh waiting barrier: %+v %v", info, err)
					}
				}
				if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
					t.Fatal("observer reconciliation executed native lifecycle")
				}
			})
		}
	}
}

func TestUpstreamObserverUncertainReservationIsRetained(t *testing.T) {
	for _, boundary := range []string{"fresh", "partial-attachment", "missing-status", "unfinished", "live-job", "failed-setup", "unknown-phase", "unknown-operation", "active-primary", "active-auxiliary", "foreign-primary", "foreign-auxiliary", "unattached-auxiliary", "inspect-error"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
			root := t.TempDir()
			rt := upstreamTestWrapper(t, base, root)
			spec := upstreamTestSpec()
			spec.Upstream.ObserveOnly = true
			spec.Upstream.AuxiliaryContainers = []string{"builder"}
			if boundary == "partial-attachment" {
				spec.Upstream.Containers = append(spec.Upstream.Containers, "pending")
			}
			oldID, err := rt.Create(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			old := rt.records[oldID]
			old.Armed = true
			old.Observed = map[string]string{"authored": "authored-id", "builder": "builder-id"}
			old.Attached = map[string]bool{"authored": true, "builder": true}
			if boundary == "fresh" {
				old.Observed, old.Attached = make(map[string]string), make(map[string]bool)
			}
			upstreamTestCompletedJob(t, rt, old, "observe")
			status := upstreamStatus{Phase: "observing", Done: true, Observed: old.Observed}
			switch boundary {
			case "unfinished":
				status.Done = false
			case "live-job":
				old.Job.PID = os.Getpid()
				old.Job.ProcessIdentity, err = upstreamProcessIdentity(old.Job.PID)
				if err != nil {
					t.Fatal(err)
				}
			case "failed-setup":
				status.Phase, status.Error, status.ExitCode = "dead", "setup failed", 1
			case "unknown-phase":
				status.Phase = ""
			case "unknown-operation":
				old.Job.Operation = "start"
			case "active-primary":
				base.containers["authored"] = ContainerInfo{ID: "authored-id", Name: "authored", State: "running"}
			case "active-auxiliary":
				base.containers["builder"] = ContainerInfo{ID: "builder-id", Name: "builder", State: "running"}
			case "foreign-primary":
				base.containers["authored"] = ContainerInfo{ID: "foreign-id", Name: "authored", State: "exited"}
			case "unattached-auxiliary":
				delete(old.Observed, "builder")
				delete(old.Attached, "builder")
				base.containers["builder"] = ContainerInfo{ID: "foreign-id", Name: "builder", State: "exited"}
			case "foreign-auxiliary":
				base.containers["builder"] = ContainerInfo{ID: "foreign-id", Name: "builder", State: "exited"}
			}
			if err := writeUpstreamJSON(filepath.Join(old.Job.Dir, "status.json"), &status); err != nil {
				t.Fatal(err)
			}
			if boundary == "missing-status" {
				if err := os.Remove(filepath.Join(old.Job.Dir, "status.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err := rt.save(old); err != nil {
				t.Fatal(err)
			}
			next := upstreamTestSpec()
			next.Name, next.Upstream.ObserveOnly = "observer-successor", true
			nextID, err := rt.Create(ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			if boundary == "inspect-error" {
				base.inspect = func(name string) (*ContainerInfo, error) {
					return nil, fmt.Errorf("engine unavailable")
				}
			}
			for pass := range 2 {
				rt = upstreamTestWrapper(t, base, root)
				if err := rt.Start(ctx, nextID); err == nil {
					t.Fatal("uncertain observer released a successor")
				}
				old = rt.records[oldID]
				if !old.Armed || old.Stopped || old.Superseded {
					t.Fatalf("uncertain observer lost its reservation on pass %d: %+v", pass, old)
				}
				if boundary == "unattached-auxiliary" && (old.Attached["builder"] || old.Observed["builder"] != "") {
					t.Fatal("terminal observer adopted a foreign auxiliary container")
				}
			}
			if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
				t.Fatal("reservation reconciliation executed native lifecycle")
			}
		})
	}
}

func TestUpstreamUnsettledSupervisorBlocksHandoff(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	oldID, err := rt.Create(ctx, upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	old := rt.records[oldID]
	old.Observed["authored"] = "retained-id"
	upstreamTestCompletedJob(t, rt, old, "start")
	status := upstreamStatus{Phase: "starting", Observed: old.Observed}
	if err := writeUpstreamJSON(filepath.Join(old.Job.Dir, "status.json"), &status); err != nil {
		t.Fatal(err)
	}
	base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
	nextSpec := upstreamTestSpec()
	nextSpec.Name, nextSpec.Upstream.ObserveOnly = "successor", true
	nextID, err := rt.Create(ctx, nextSpec)
	if err != nil {
		t.Fatal(err)
	}
	rt = upstreamTestWrapper(t, base, root)
	for _, id := range []string{oldID, nextID} {
		if err := rt.Start(ctx, id); err == nil || !strings.Contains(err.Error(), "lifecycle_unsettled") {
			t.Fatalf("lost supervisor authorized another launch: %v", err)
		}
	}
	if info, err := rt.Inspect(ctx, nextID); err != nil || info.State == "observing" || info.State == "running" {
		t.Fatalf("unsettled predecessor armed successor: %+v %v", info, err)
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("unsettled predecessor reached native lifecycle")
	}
}

func TestUpstreamAmbiguousRetainedClaimsFailClosed(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	var ids []string
	for index := range 2 {
		spec := upstreamTestSpec()
		spec.Name = fmt.Sprintf("legacy-run-%d", index)
		id, err := rt.Create(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		rec := rt.records[id]
		rec.Observed["authored"], rec.Stopped = "shared-legacy-id", true
		upstreamTestCompletedJob(t, rt, rec, "start")
		ids = append(ids, id)
	}
	rt = upstreamTestWrapper(t, base, root)
	base.containers["authored"] = ContainerInfo{ID: "shared-legacy-id", Name: "authored", State: "running"}
	for _, id := range ids {
		if info, err := rt.Inspect(ctx, id); err != nil || info.State == "running" {
			t.Fatalf("ambiguous legacy record claimed running service: %+v %v", info, err)
		}
		if err := rt.Stop(ctx, id, 1); err == nil {
			t.Fatal("ambiguous history authorized authored stop")
		}
		if err := rt.Remove(ctx, id, true); err == nil {
			t.Fatal("ambiguous history authorized engine removal")
		}
	}
	base.containers["authored"] = ContainerInfo{ID: "shared-legacy-id", Name: "authored", State: "exited"}
	for _, id := range ids {
		if err := rt.Start(ctx, id); err == nil {
			t.Fatal("ambiguous history authorized restart")
		}
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("ambiguous history reached native lifecycle")
	}
}

func TestUpstreamIncompleteHandoffFailsClosed(t *testing.T) {
	ctx := context.Background()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	root := t.TempDir()
	rt := upstreamTestWrapper(t, base, root)
	oldID, err := rt.Create(ctx, upstreamTestSpec())
	if err != nil {
		t.Fatal(err)
	}
	old := rt.records[oldID]
	old.Observed["authored"], old.Stopped = "retained-id", true
	upstreamTestCompletedJob(t, rt, old, "start")
	base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
	nextSpec := upstreamTestSpec()
	nextSpec.Name, nextSpec.Upstream.ObserveOnly = "successor", true
	nextID, err := rt.Create(ctx, nextSpec)
	if err != nil {
		t.Fatal(err)
	}
	// Force the grant's atomic rename to fail after predecessor revocation.
	path := filepath.Join(rt.root, nextID, "record.json")
	if err := os.Rename(path, path+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := rt.Start(ctx, nextID); err == nil {
		t.Fatal("incomplete durable grant armed observer")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".retained", path); err != nil {
		t.Fatal(err)
	}
	rt = upstreamTestWrapper(t, base, root)
	base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "running"}
	for _, id := range []string{oldID, nextID} {
		if info, err := rt.Inspect(ctx, id); err != nil || info.State == "running" {
			t.Fatalf("incomplete handoff manufactured authority: %+v %v", info, err)
		}
		if err := rt.Start(ctx, id); err == nil {
			t.Fatal("incomplete handoff adopted a live container")
		}
	}
	if err := rt.Stop(ctx, oldID, 1); err != nil {
		t.Fatal(err)
	}
	if err := rt.Stop(ctx, nextID, 1); err != nil {
		t.Fatalf("ungranted successor logical stop failed: %v", err)
	}
	if !rt.records[nextID].Stopped {
		t.Fatal("ungranted successor logical stop was not persisted")
	}
	if err := rt.Remove(ctx, oldID, true); err != nil {
		t.Fatal(err)
	}
	if err := rt.Remove(ctx, nextID, true); err == nil {
		t.Fatal("ungranted successor accepted removal")
	}
	base.containers["authored"] = ContainerInfo{ID: "retained-id", Name: "authored", State: "exited"}
	if err := rt.Start(ctx, nextID); err == nil {
		t.Fatal("orphaned historical evidence silently granted new authority")
	}
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("incomplete handoff touched engine lifecycle")
	}
}

func upstreamFixture(t *testing.T, install string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git required for local source fixture")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_TEMPLATE_DIR", t.TempDir())
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "recipe"), 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"recipe/install.sh":    "#!/bin/sh\nset -eu\n" + install + "\n",
		"recipe/start.sh":      "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$1\" > argv.txt\nprintf '%s\\n' \"$VALUE\" > value.txt\nprintf 'started\\n' > started\nprintf '# authored patch\\n' >> tracked.py\nprintf 'start stdout\\n'\nprintf 'start stderr\\n' >&2\n",
		"recipe/stop.sh":       "#!/bin/sh\nset -eu\nrm -f started\n",
		"recipe/tracked.py":    "# exact pinned source\n",
		"recipe/env.template":  "VALUE=default\nUNCHANGED=upstream-default\n",
		"outside-source.asset": "complete repository asset\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("local fixture git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-m", "pinned fixture")
	revision := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "recipe/start.sh"), []byte("#!/bin/sh\nexit 99\n"), 0755); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "commit.gpgsign=false", "commit", "-m", "moving branch must not execute")
	return repo, revision
}

func TestUpstreamOriginalSourceAndInstallationReuse(t *testing.T) {
	ctx := context.Background()
	repo, revision := upstreamFixture(t, "printf 'install\\n' >> install.count")
	base := &upstreamTestRuntime{}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repo, revision
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	spec.Upstream.Start = []string{"./start.sh", "literal $(touch injected)"}
	spec.Upstream.EnvFile, spec.Upstream.EnvTemplate, spec.Upstream.EnvFormat = ".env", "env.template", "shell"
	spec.Env = []string{"VALUE=reviewed ' value"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	workspace := rt.records[id].Workspace
	working := filepath.Join(workspace, "repository", "recipe")
	base.inspect = func(name string) (*ContainerInfo, error) {
		if name == "authored" {
			if _, err := os.Stat(filepath.Join(working, "started")); err == nil {
				return &ContainerInfo{ID: "authored-id", Name: name, State: "running"}, nil
			}
		}
		return nil, errdefs.NotFound(fmt.Errorf("missing %s", name))
	}
	var stdout, stderr bytes.Buffer
	run := func(spec *ContainerSpec, operation string) error {
		before := map[string]string{}
		if operation == "stop" {
			before["authored"] = "authored-id"
		}
		return executeUpstreamJob(ctx, base, t.TempDir(), &upstreamRequest{Spec: *spec, Installation: workspace, Operation: operation, Before: before}, &stdout, &stderr)
	}
	if err := run(spec, "start"); err != nil {
		t.Fatal(err)
	}
	assertFile := func(path, want string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, want %q: %v", path, data, want, err)
		}
	}
	assertFile(filepath.Join(working, "argv.txt"), "literal $(touch injected)\n")
	assertFile(filepath.Join(working, "value.txt"), "reviewed ' value\n")
	assertFile(filepath.Join(workspace, "repository", "outside-source.asset"), "complete repository asset\n")
	if _, err := os.Stat(filepath.Join(workspace, "repository", ".git")); err != nil {
		t.Fatal("complete Git metadata missing", err)
	}
	if _, err := os.Stat(filepath.Join(working, "injected")); !os.IsNotExist(err) {
		t.Fatal("argv was shell-expanded")
	}
	mode, err := os.Stat(filepath.Join(working, "start.sh"))
	if err != nil || mode.Mode()&0111 == 0 {
		t.Fatal("authored executable mode lost")
	}
	if !strings.Contains(stdout.String(), "start stdout") || !strings.Contains(stderr.String(), "start stderr") {
		t.Fatalf("authored output lost: %s / %s", stdout.String(), stderr.String())
	}
	if err := run(spec, "stop"); err != nil {
		t.Fatalf("authored source mutation broke stop: %v", err)
	}
	if err := os.WriteFile(filepath.Join(working, "cache-kept"), []byte("cache"), 0600); err != nil {
		t.Fatal(err)
	}
	spec.Name, spec.Labels[LabelRun] = "reviewed-run-two", "run-two"
	nextID, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if rt.records[nextID].Workspace != workspace {
		t.Fatal("new run abandoned reviewed installation")
	}
	if err := run(spec, "start"); err != nil {
		t.Fatalf("authored source mutation broke restart: %v", err)
	}
	assertFile(filepath.Join(working, "install.count"), "install\n")
	assertFile(filepath.Join(working, "cache-kept"), "cache")
	if err := run(spec, "stop"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(working, "tracked.py"), []byte("# external modification\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := run(spec, "start"); err == nil || !strings.Contains(err.Error(), "source_modified") {
		t.Fatalf("external source modification accepted: %v", err)
	}
}

func TestUpstreamFailedInstallIsNotRunningOrInstalled(t *testing.T) {
	ctx := context.Background()
	repo, revision := upstreamFixture(t, "printf 'setup failure\\n' >&2; exit 7")
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repo, revision
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	rec := rt.records[id]
	dir := filepath.Join(rt.root, id, "job-failure")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rec.Job = &upstreamJob{Dir: dir, Operation: "start"}
	var stderr bytes.Buffer
	err = executeUpstreamJob(ctx, base, dir, &upstreamRequest{Spec: rec.Spec, Installation: rec.Workspace, Operation: "start"}, io.Discard, &stderr)
	if err == nil {
		t.Fatal("failed installation succeeded")
	}
	info, err := rt.Inspect(ctx, id)
	if err != nil || info.State != "dead" || info.ExitCode != 7 {
		t.Fatalf("failed setup state lost: %+v, %v", info, err)
	}
	if !strings.Contains(stderr.String(), "setup failure") {
		t.Fatalf("setup stderr lost: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rec.Workspace, "installed.json")); !os.IsNotExist(err) {
		t.Fatalf("failed install marked complete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rec.Workspace, "repository", "recipe", "started")); !os.IsNotExist(err) {
		t.Fatalf("start executed after failed install: %v", err)
	}
	if err := writeUpstreamJSON(filepath.Join(dir, "status.json"), &upstreamStatus{Phase: "starting"}); err != nil {
		t.Fatal(err)
	}
	info, err = rt.Inspect(ctx, id)
	if err != nil || info.State != "dead" || !strings.Contains(info.Error, "supervisor_lost") {
		t.Fatalf("lost supervisor became success: %+v, %v", info, err)
	}
}

func TestUpstreamLiteralEnvironmentAndPathEscape(t *testing.T) {
	working := t.TempDir()
	spec := upstreamTestSpec()
	spec.Upstream.EnvFile, spec.Upstream.EnvFormat = ".env", "literal"
	spec.Env = []string{"VALUE=$HOME with ' literal quotes"}
	if err := configureUpstreamEnv(spec, working, filepath.Join(working, "env-state.json")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(working, ".env"))
	if err != nil || string(data) != "VALUE=$HOME with ' literal quotes\n" {
		t.Fatalf("literal env was shell-quoted: %q, %v", data, err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(working, "escape")); err != nil {
		t.Skip("symlinks unavailable")
	}
	spec.Upstream.EnvFile = "escape/.env"
	if err := configureUpstreamEnv(spec, working, filepath.Join(working, "env-state.json")); err == nil {
		t.Fatal("configuration write escaped source")
	}
}

func TestUpstreamDockerEndpointBinding(t *testing.T) {
	env, err := upstreamCommandEnvironment([]string{"PATH=/bin", "VALUE=host"}, []string{"VALUE=reviewed"}, "/custom/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["DOCKER_HOST"] != "unix:///custom/docker.sock" || values["VALUE"] != "reviewed" {
		t.Fatalf("command daemon/config mismatch: %v", values)
	}
	if _, err := upstreamCommandEnvironment([]string{"DOCKER_HOST=unix:///other.sock"}, nil, "/custom/docker.sock"); err == nil {
		t.Fatal("different inherited daemon accepted")
	}
	if _, err := upstreamCommandEnvironment(nil, []string{"DOCKER_CONTEXT=remote"}, "/custom/docker.sock"); err == nil {
		t.Fatal("context overriding observed daemon accepted")
	}
}

func TestUpstreamSetupLogsBeforeSourceMaterialization(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamTestSpec()
	spec.Upstream.LogFile = "logs/engine.log"
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := rt.LogsFollow(ctx, id, true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := os.WriteFile(filepath.Join(rt.root, id, "stdout.log"), []byte("install in progress\n"), 0600); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, len("install in progress\n"))
	if _, err := io.ReadFull(stream, output); err != nil {
		t.Fatal(err)
	}
	if string(output) != "install in progress\n" {
		t.Fatalf("live setup output lost: %q", output)
	}
	info, err := rt.Inspect(ctx, id)
	if err != nil || info.State == "running" {
		t.Fatalf("logs fabricated running state: %+v, %v", info, err)
	}
}

func TestUpstreamObserverInstallsOnceWithoutCoordinatorLifecycle(t *testing.T) {
	ctx := context.Background()
	repo, revision := upstreamFixture(t, "printf 'install\\n' >> install.count; printf 'observer setup output\\n'")
	base := &upstreamTestRuntime{containers: make(map[string]ContainerInfo)}
	rt := upstreamTestWrapper(t, base, t.TempDir())
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repo, revision
	spec.Upstream.ObserveOnly = true
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	spec.Upstream.Stop = []string{"/bin/sh", "-c", "exit 91"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	rec := rt.records[id]
	rec.Armed, rec.Before, rec.Attached = true, make(map[string]string), make(map[string]bool)
	dir := filepath.Join(rt.root, id, "job-observe")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	rec.Job = &upstreamJob{Dir: dir, Operation: "observe"}
	request := &upstreamRequest{Spec: rec.Spec, Installation: rec.Workspace, Operation: "observe"}
	var stdout bytes.Buffer
	if err := executeUpstreamJob(ctx, base, dir, request, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	info, err := rt.Inspect(ctx, id)
	if err != nil || info.State != "observing" {
		t.Fatalf("completed local setup did not release observer barrier: %+v, %v", info, err)
	}
	if !strings.Contains(stdout.String(), "observer setup output") {
		t.Fatalf("observer setup logs lost: %s", stdout.String())
	}
	if err := executeUpstreamJob(ctx, base, dir, request, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	count, err := os.ReadFile(filepath.Join(rec.Workspace, "repository", "recipe", "install.count"))
	if err != nil || string(count) != "install\n" {
		t.Fatalf("observer repeated installation: %q, %v", count, err)
	}
	if _, err := os.Stat(filepath.Join(rec.Workspace, "repository", "recipe", "started")); !os.IsNotExist(err) {
		t.Fatalf("observer ran coordinator start: %v", err)
	}
	if err := rt.Stop(ctx, id, 1); err != nil {
		t.Fatalf("observer ran coordinator stop instead of observing teardown: %v", err)
	}
}
