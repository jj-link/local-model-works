package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/hardware"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// tailer mirrors one workload's container output into local log files
// (byte-cursor source of truth on the node) and streams the chunks to the
// controller.
type tailer struct {
	a    *Agent
	key  tailKey
	id   string // container ID
	done chan struct{}
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
	key := tailKey{runID: runID, deploymentID: deploymentID, rank: rank}
	w.mu.Lock()
	if t, ok := w.tailers[key]; ok {
		if t.id == containerID {
			w.mu.Unlock()
			return
		}
		// Container was recreated (new ID): restart the tail from the
		// existing local files' offsets.
		t.done <- struct{}{}
		close(t.done)
		delete(w.tailers, key)
	}
	t := &tailer{a: w.a, key: key, id: containerID, done: make(chan struct{})}
	w.tailers[key] = t
	w.mu.Unlock()
	go t.run(ctx)
}

// stopTailer stops the tailer for a run/deployment/rank, if present.
func (w *workloads) stopTailer(runID, deploymentID string, rank int32) {
	key := tailKey{runID: runID, deploymentID: deploymentID, rank: rank}
	w.mu.Lock()
	t, ok := w.tailers[key]
	if ok {
		delete(w.tailers, key)
	}
	w.mu.Unlock()
	if ok {
		close(t.done)
	}
}

// run mirrors stdout and stderr until the container stream ends or the
// tailer is stopped.
func (t *tailer) run(ctx context.Context) {
	stdout, stderr, err := t.a.rt.LogsStreams(ctx, t.id)
	if err != nil {
		t.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_StateUpdate{
			StateUpdate: &agentv1.StateUpdate{
				DeploymentId:      t.key.deploymentID,
				ContainerId:       t.id,
				DiagnosticCode:    "logs.unavailable",
				DiagnosticMessage: err.Error(),
			},
		}})
		return
	}
	defer stdout.Close()
	defer stderr.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); t.stream(ctx, stdout, "stdout") }()
	go func() { defer wg.Done(); t.stream(ctx, stderr, "stderr") }()

	// Exit when the container is gone or the tailer is replaced.
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			wg.Wait()
			return
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			info, err := t.a.rt.Inspect(ctx, t.id)
			if err != nil || info.State != "running" {
				wg.Wait()
				return
			}
		}
	}
}

// stream reads one output stream line-by-line, appends it to the local log
// file, and sends the chunk with its file offset.
func (t *tailer) stream(ctx context.Context, r io.Reader, name string) {
	path := t.a.logPath(t.key, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	offset, _ := f.Seek(0, io.SeekEnd)

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := append(sc.Bytes(), '\n')
		if _, err := f.Write(line); err != nil {
			return
		}
		chunk := make([]byte, len(line))
		copy(chunk, line)
		t.a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_LogChunk{
			LogChunk: &agentv1.LogChunk{
				RunId:        t.key.runID,
				DeploymentId: t.key.deploymentID,
				Rank:         t.key.rank,
				Stream:       name,
				Offset:       uint64(offset),
				Data:         chunk,
			},
		}})
		offset += int64(len(line))
	}
}

// handleLogRequest replays a byte range of one local log stream to the
// controller (resume from Last-Event-ID / from_offset). Live output is
// already streamed by the tailer; this covers history and gaps.
func (a *Agent) handleLogRequest(ctx context.Context, req *agentv1.LogRequest) {
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
	from := req.GetFromOffset()
	if from > 0 {
		if _, err := f.Seek(int64(from), io.SeekStart); err != nil {
			return
		}
	}
	buf := make([]byte, 256*1024)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_LogChunk{
				LogChunk: &agentv1.LogChunk{
					RunId:        key.runID,
					DeploymentId: key.deploymentID,
					Rank:         key.rank,
					Stream:       stream,
					Offset:       from,
					Data:         chunk,
				},
			}})
			from += uint64(n)
		}
		if rerr != nil {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// Cache placements
// ---------------------------------------------------------------------------

// scanCacheRoot inspects one configured cache root: backend detection,
// total size, and repository listing.
func scanCacheRoot(ctx context.Context, root string) hardware.CacheRoot {
	out := hardware.CacheRoot{Path: root, Backend: detectBackend(root)}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return out
	}
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

// placementCandidate is one concrete model tree the node holds.
type placementCandidate struct {
	Identity string
	Path     string
	Size     int64
}

// placementCandidates lists the reportable model trees under a cache root.
// HuggingFace hub layout (models--<org>--<repo>) normalizes to the
// canonical identity "huggingface://<org>/<repo>" and reports the concrete
// snapshot directory; plain roots report each direct subdirectory as
// "local://<name>".
func placementCandidates(ctx context.Context, root string) []placementCandidate {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []placementCandidate
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(root, e.Name())
		if strings.HasPrefix(e.Name(), "models--") {
			modelDir := full
			sha, ok := resolveHFSnapshot(ctx, modelDir)
			if !ok {
				continue
			}
			rest := strings.TrimPrefix(e.Name(), "models--")
			org, repo, found := strings.Cut(rest, "--")
			if !found || org == "" || repo == "" {
				continue
			}
			snap := filepath.Join(modelDir, "snapshots", sha)
			out = append(out, placementCandidate{
				Identity: "huggingface://" + org + "/" + repo,
				Path:     snap,
				Size:     dirSize(ctx, snap, hfContainDir(snap)),
			})
			continue
		}
		out = append(out, placementCandidate{
			Identity: "local://" + e.Name(),
			Path:     full,
			Size:     dirSize(ctx, full, full),
		})
	}
	return out
}

// resolveHFSnapshot resolves a HF refs file to a commit snapshot, falling
// back to the newest snapshot directory.
func resolveHFSnapshot(ctx context.Context, modelDir string) (string, bool) {
	refs := filepath.Join(modelDir, "refs")
	if entries, err := os.ReadDir(refs); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(refs, e.Name()))
			sha := strings.TrimSpace(string(data))
			if rerr != nil || !isSha40(sha) {
				continue
			}
			if _, serr := os.Stat(filepath.Join(modelDir, "snapshots", sha)); serr == nil {
				return sha, true
			}
		}
	}
	snaps := filepath.Join(modelDir, "snapshots")
	entries, err := os.ReadDir(snaps)
	if err != nil {
		return "", false
	}
	var best string
	var bestM time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if fi, ierr := e.Info(); ierr == nil && fi.ModTime().After(bestM) {
			best, bestM = e.Name(), fi.ModTime()
		}
	}
	return best, best != ""
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
		if fi, serr := os.Stat(target); serr == nil && fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return size
}

// reportPlacements sends one placement report per discovered model tree.
// State is "pending": the controller validates before a placement becomes
// schedulable.
func (a *Agent) reportPlacements(ctx context.Context) {
	for _, root := range a.cfg.CacheRoots {
		cands := placementCandidates(ctx, root)
		if len(cands) == 0 {
			// No model directories: report the raw root itself.
			a.sendPlacement("cache://"+root, root, dirSize(ctx, root, root))
			continue
		}
		for _, c := range cands {
			a.sendPlacement(c.Identity, c.Path, c.Size)
		}
	}
}

func (a *Agent) sendPlacement(identity, path string, size int64) {
	a.send(&agentv1.AgentMessage{Body: &agentv1.AgentMessage_PlacementReport{
		PlacementReport: &agentv1.PlacementReport{
			ArtifactId: identity,
			Path:       path,
			State:      "pending",
			SizeBytes:  uint64(size),
		},
	}})
}
