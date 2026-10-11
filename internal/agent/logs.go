package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
	"github.com/jj-link/local-model-works/internal/hardware"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// tailer mirrors one workload's container output into local log files
// (byte-cursor source of truth on the node) and streams the chunks to the
// controller.
type tailer struct {
	a      *Agent
	key    tailKey
	id     string // container or logical upstream installation ID
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // closed after capture producers and local files close
}

// logPath is <LogDir>/<run>/<deployment>/rank<N>-<stream>.log; an empty
// run falls back to the deployment id, an empty deployment to "adhoc".
func (a *Agent) logPath(key tailKey, stream string) string {
	run := key.runID
	if run == "" {
		run = key.deploymentID
	}
	if run == "" {
		run = "adhoc"
	}
	dep := key.deploymentID
	if dep == "" {
		dep = "adhoc"
	}
	return filepath.Join(a.cfg.LogDir(), shortID(run), shortID(dep),
		fmt.Sprintf("rank%d-%s.log", key.rank, stream))
}

// startTailer begins mirroring a running container's output if no tailer is
// already live for the run/deployment/rank.
func (w *workloads) startTailer(ctx context.Context, runID, deploymentID string, rank int32, containerID string) {
	if validateWorkloadIdentity(deploymentID, runID, rank) != nil {
		return
	}

	key := tailKey{runID: runID, deploymentID: deploymentID, rank: rank}
	w.mu.Lock()
	previous := w.tailers[key]
	if previous != nil {
		if previous.id == containerID && previous.ctx.Err() == nil {
			w.mu.Unlock()
			return
		}
		previous.cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	t := &tailer{a: w.a, key: key, id: containerID, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	w.tailers[key] = t
	w.mu.Unlock()
	go func() {
		defer func() {
			t.cancel()
			w.mu.Lock()
			if w.tailers[key] == t {
				delete(w.tailers, key)
			}
			w.mu.Unlock()
			close(t.done)
		}()
		// Reconnect may beat the old readers' shutdown. Never let two
		// generations append to the same byte-cursor files concurrently.
		if previous != nil {
			<-previous.done
		}
		if ctx.Err() == nil {
			t.run(ctx)
		}
	}()
}

// stopTailer stops the tailer for a run/deployment/rank, if present.
func (w *workloads) stopTailer(runID, deploymentID string, rank int32) {
	key := tailKey{runID: runID, deploymentID: deploymentID, rank: rank}
	w.mu.Lock()
	if t := w.tailers[key]; t != nil {
		t.cancel()
	}
	w.mu.Unlock()
}

// run retains one capture generation for the session. Transport EOF/errors
// reopen from committed physical-source cursors, never from merged byte counts.
func (t *tailer) run(ctx context.Context) {
	for ctx.Err() == nil {
		complete, err := t.capture(ctx)
		if complete || ctx.Err() != nil {
			return
		}
		t.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_StateUpdate{
			StateUpdate: &agentv1.StateUpdate{
				DeploymentId: t.key.deploymentID, RunId: t.key.runID, ContainerId: t.id,
				DiagnosticCode: "logs.unavailable", DiagnosticMessage: err.Error(),
			},
		}})
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (t *tailer) capture(ctx context.Context) (bool, error) {
	c, err := openLogCapture(t)
	if err != nil {
		return false, err
	}
	defer c.close()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- t.a.rt.CaptureLogs(child, t.id, c.cursor, c.emit) }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var captureErr, flushErr error
captureLoop:
	for {
		select {
		case captureErr = <-finished:
			break captureLoop
		case <-ctx.Done():
			cancel()
			captureErr = <-finished
			break captureLoop
		case <-ticker.C:
			if flushErr = c.flush(ctx, false); flushErr != nil {
				cancel()
				captureErr = <-finished
				break captureLoop
			}
		}
	}
	// Source EOF is independent of controller availability. In particular, a
	// successful result already racing with cancellation must survive on disk.
	complete := captureErr == nil
	if err := c.flush(ctx, complete); err != nil {
		return false, err
	}
	if flushErr != nil && ctx.Err() == nil {
		return false, flushErr
	}
	if complete {
		for _, name := range []string{"stdout", "stderr"} {
			_ = t.a.sendLog(ctx, &agentv1.LogChunk{
				RunId: t.key.runID, DeploymentId: t.key.deploymentID, Rank: t.key.rank,
				Stream: name, Final: true, EndOffset: uint64(c.state.Offsets[name]),
			})
		}
	}
	return complete, captureErr
}

// handleLogRequest replays a byte range of one local log stream to the
// controller (resume from Last-Event-ID / from_offset). Live output is
// already streamed by the tailer; this covers history and gaps.
func (a *Agent) handleLogRequest(ctx context.Context, req *agentv1.LogRequest) {
	if validateWorkloadIdentity(req.GetDeploymentId(), req.GetRunId(), req.GetRank()) != nil {
		return
	}
	name := containerName(req.GetDeploymentId(), req.GetRunId(), req.GetRank())
	if _, err := a.resolve(name, req.GetDeploymentId(), req.GetRunId(), req.GetRank()); err != nil {
		return
	}
	key := tailKey{runID: req.GetRunId(), deploymentID: req.GetDeploymentId(), rank: req.GetRank()}
	stream := req.GetStream()
	if stream != "stderr" {
		stream = "stdout"
	}
	path := a.logPath(key, stream)
	f, err := os.Open(path)
	if err != nil {
		return // nothing on disk for this stream yet
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return
	}
	end := uint64(stat.Size())
	complete := false
	data, err := os.ReadFile(a.logCheckpointPath(key))
	if err == nil {
		var checkpoint logCheckpoint
		if json.Unmarshal(data, &checkpoint) != nil || checkpoint.Offsets[stream] < 0 {
			return
		}
		end = uint64(checkpoint.Offsets[stream])
		complete = checkpoint.Completed
	} else if !errors.Is(err, os.ErrNotExist) {
		return
	}
	if err := a.replayLogFile(ctx, key, stream, f, req.GetFromOffset(), end); err != nil {
		return
	}
	if complete {
		_ = a.sendLog(ctx, &agentv1.LogChunk{
			RunId: key.runID, DeploymentId: key.deploymentID, Rank: key.rank,
			Stream: stream, Final: true, EndOffset: end,
		})
	}
}

// ---------------------------------------------------------------------------
// Cache placements
// ---------------------------------------------------------------------------

// scanCacheRoot inspects one configured cache root: backend detection,
// total size, repository listing, and an actual writability probe. The
// probe creates and removes a unique marker file through the agent's own
// mount namespace, so systemd ProtectSystem/ProtectHome restrictions are
// reflected exactly as artifact fetches will experience them.
func scanCacheRoot(ctx context.Context, root string) hardware.CacheRoot {
	out := hardware.CacheRoot{Path: root, Backend: detectBackend(root)}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return out
	}
	out.Writable = probeWritable(root)
	var size int64
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				size += fi.Size()
			}
		}
		return nil
	})
	out.SizeBytes = size
	out.Repositories = listRepos(root)
	return out
}

// probeWritable reports whether a marker file can be created, written,
// and removed inside root. Any failure leaves the root reported as not
// writable; diagnostics are intentionally coarse because the fix lives in
// node configuration, not in the file contents.
func probeWritable(root string) bool {
	file, err := os.CreateTemp(root, ".lmw-write-probe-*")
	if err != nil {
		return false
	}
	_, writeErr := file.WriteString("lmw")
	closeErr := file.Close()
	removeErr := os.Remove(file.Name())
	return writeErr == nil && closeErr == nil && removeErr == nil
}

// detectBackend classifies a cache root by its layout.
func detectBackend(root string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(root)))
	if base == "huggingface" {
		return "huggingface"
	}
	if strings.HasSuffix(strings.ToLower(filepath.Clean(root)), "/models/hub") ||
		strings.HasSuffix(strings.ToLower(filepath.Clean(root)), "/.cache/huggingface") {
		return "huggingface"
	}
	return "local"
}

// listRepos lists model repositories under a cache root (two levels for
// huggingface hub layouts, one for plain local roots).
func listRepos(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var repos []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(root, e.Name())
		subEntries, err := os.ReadDir(sub)
		if err == nil && len(subEntries) > 0 {
			for _, se := range subEntries {
				if se.IsDir() {
					repos = append(repos, filepath.Join(e.Name(), se.Name()))
				}
			}
		} else {
			repos = append(repos, e.Name())
		}
	}
	return repos
}

// placementCandidate is one validated immutable model snapshot.
type placementCandidate struct {
	Identity    string
	Path        string
	Size        int64
	State       string
	Diagnostics []*agentv1.Diagnostic
}

// placementCandidates recognizes both <cache>/models--owner--repo and
// <cache>/hub/models--owner--repo, and reports every immutable revision.
func placementCandidates(ctx context.Context, root string) []placementCandidate {
	searchRoots := []string{root}
	if info, err := os.Stat(filepath.Join(root, "hub")); err == nil && info.IsDir() {
		searchRoots = append(searchRoots, filepath.Join(root, "hub"))
	}
	seenModels := map[string]bool{}
	var out []placementCandidate
	for _, searchRoot := range searchRoots {
		entries, err := os.ReadDir(searchRoot)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "models--") {
				continue
			}
			modelDir := filepath.Join(searchRoot, entry.Name())
			if seenModels[modelDir] {
				continue
			}
			seenModels[modelDir] = true
			rest := strings.TrimPrefix(entry.Name(), "models--")
			owner, repo, ok := strings.Cut(rest, "--")
			if !ok || owner == "" || repo == "" {
				continue
			}
			snapshots, err := os.ReadDir(filepath.Join(modelDir, "snapshots"))
			if err != nil {
				continue
			}
			for _, snapshot := range snapshots {
				if !snapshot.IsDir() || !isSha40(snapshot.Name()) {
					continue
				}
				identity, err := artifactidentity.Canonical(
					"huggingface", "hf://"+owner+"/"+repo, snapshot.Name(), "",
				)
				if err != nil {
					continue
				}
				snapshotDir := filepath.Join(modelDir, "snapshots", snapshot.Name())
				candidate := placementCandidate{
					Identity:    identity,
					Path:        modelDir,
					Size:        regularTreeSize(ctx, modelDir),
					State:       "valid",
					Diagnostics: hfSnapshotDiagnostics(ctx, identity, modelDir, snapshotDir),
				}
				if len(candidate.Diagnostics) > 0 {
					candidate.State = "invalid"
				}
				out = append(out, candidate)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identity < out[j].Identity })
	return out
}

func isSha40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// dirSize totals regular-file sizes under root, dereferencing symlinks that
// stay inside contain (HF snapshots symlink into sibling blobs/).
func dirSize(ctx context.Context, root, contain string) int64 {
	contain = filepath.Clean(contain)
	var size int64
	seen := map[string]bool{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipDir
		}
		target := p
		if d.Type()&fs.ModeSymlink != 0 {
			resolved, rerr := filepath.EvalSymlinks(p)
			if rerr != nil {
				return nil
			}
			target = resolved
		}
		rl := filepath.Clean(target)
		if rl != contain && !strings.HasPrefix(rl, contain+string(filepath.Separator)) {
			return nil
		}
		if seen[rl] {
			return nil
		}
		seen[rl] = true
		if fi, serr := os.Stat(target); serr == nil && fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return size
}

func regularTreeSize(ctx context.Context, root string) int64 {
	var size int64
	_ = filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

// reportPlacements validates every discovered immutable snapshot before it
// becomes schedulable.
func (a *Agent) reportPlacements(ctx context.Context) {
	a.rescanPlacements(ctx)
}

func (a *Agent) sendPlacement(candidate placementCandidate) {
	a.rememberPlacement(candidate)
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_PlacementReport{
		PlacementReport: &agentv1.PlacementReport{
			ArtifactId:  candidate.Identity,
			Path:        candidate.Path,
			State:       candidate.State,
			VerifiedAt:  timestamppb.Now(),
			Diagnostics: candidate.Diagnostics,
			SizeBytes:   uint64(candidate.Size),
		},
	}})
}
