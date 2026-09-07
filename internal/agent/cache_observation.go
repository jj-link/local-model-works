package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func placementKey(candidate placementCandidate) string {
	return candidate.Identity + "\x00" + candidate.Path
}
func (a *Agent) loadPlacements() {
	if a.observedPlacements != nil {
		return
	}
	a.observedPlacements = map[string]placementCandidate{}
	data, err := os.ReadFile(filepath.Join(a.cfg.StateRoot, "observed-placements.json"))
	if err == nil && len(data) <= 16<<20 {
		_ = json.Unmarshal(data, &a.observedPlacements)
	}
	if a.observedPlacements == nil {
		a.observedPlacements = map[string]placementCandidate{}
	}
}
func (a *Agent) rememberPlacement(candidate placementCandidate) {
	a.placementMu.Lock()
	defer a.placementMu.Unlock()
	a.loadPlacements()
	a.observedPlacements[placementKey(candidate)] = candidate
	data, err := json.Marshal(a.observedPlacements)
	if err != nil {
		return
	}
	path := filepath.Join(a.cfg.StateRoot, "observed-placements.json")
	if os.WriteFile(path+".part", data, 0600) == nil {
		_ = os.Rename(path+".part", path)
	}
}
func (a *Agent) rescanPlacements(ctx context.Context) {
	a.placementMu.Lock()
	a.loadPlacements()
	previous := make([]placementCandidate, 0, len(a.observedPlacements))
	for _, candidate := range a.observedPlacements {
		previous = append(previous, candidate)
	}
	a.placementMu.Unlock()
	for _, root := range a.cfg.CacheRoots {
		if ctx.Err() != nil {
			return
		}
		// A disconnected/unreadable filesystem is not evidence that its files vanished.
		if _, err := os.ReadDir(root); err != nil {
			continue
		}
		candidates := placementCandidates(ctx, root)
		if ctx.Err() != nil {
			return
		}
		found := make(map[string]bool, len(candidates))
		for _, candidate := range candidates {
			found[placementKey(candidate)] = true
			a.sendPlacement(candidate)
		}
		for _, old := range previous {
			if !strings.HasPrefix(old.Identity, "hf://") || !pathWithin(old.Path, root) || found[placementKey(old)] {
				continue
			}
			// Only invalidate a positively absent snapshot; permission/network errors
			// keep the last observation rather than falsely claiming deletion.
			_, revision, ok := strings.Cut(old.Identity, "@")
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(old.Path, "snapshots", revision)); !os.IsNotExist(err) {
				continue
			}
			old.State = "invalid"
			old.Size = 0
			old.Diagnostics = []*agentv1.Diagnostic{{Code: "artifact.path_removed", Severity: "error", Message: "Previously observed snapshot no longer exists", Resource: old.Path}}
			a.sendPlacement(old)
		}
	}
}
