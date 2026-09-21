package deploy

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// deviceSettingDefaults fills only source-owned worker bindings whose declared
// settings have no value. Automatic placement retains its existing behavior:
// device facts are defaults only after the operator explicitly selects ranks.
func (s *Service) deviceSettingDefaults(ctx context.Context, m *recipe.Manifest, req PlanRequest, parameters map[string]any) (map[string]any, error) {
	if len(req.Placements) == 0 || m.Metadata.Source == nil {
		return parameters, nil
	}
	needsDefaults := false
	for _, parameter := range m.Parameters {
		if _, supplied := parameters[parameter.Name]; !supplied && parameter.Type == "string" && !parameter.Optional && !parameter.Sensitive && parameter.Default == nil {
			needsDefaults = true
			break
		}
	}
	if !needsDefaults {
		return parameters, nil
	}

	// Use the same workload predicate as planning, not bindings from inactive
	// alternatives. This does not assign nodes or relax placement validation.
	var workload *recipe.Workload
	if req.WorkloadIndex != nil {
		if *req.WorkloadIndex < 0 || *req.WorkloadIndex >= len(m.Workloads) {
			return nil, fmt.Errorf("workload index is outside the recipe")
		}
		workload = &m.Workloads[*req.WorkloadIndex]
	} else {
		nodes, err := s.q.ListNodes(ctx)
		if err != nil {
			return nil, err
		}
		for i := range m.Workloads {
			candidate := &m.Workloads[i]
			matches, err := variantMatchesNodes(candidate, s.workloadNodeCount(m, candidate), nodes, req.Placements)
			if err != nil {
				return nil, err
			}
			if matches {
				workload = candidate
				break
			}
		}
	}
	if workload == nil || workload.Upstream == nil || !slices.Contains(workload.Permissions, "host.upstream-exec") {
		return parameters, nil
	}

	resolved := parameters
	copied := false
	reports := map[string]*inventory.Inventory{}
	sshTargets := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(workload.Env)) {
		rank, fact, ok := workerSettingBinding(key)
		if !ok {
			continue
		}
		parameter := workerSettingParameter(m, workload.Env[key])
		if parameter == nil {
			continue
		}
		name := parameter.Name
		if _, supplied := parameters[name]; supplied {
			continue
		}
		nodeID := ""
		for _, placement := range req.Placements {
			if placement.Rank != rank {
				continue
			}
			if nodeID != "" {
				return nil, fmt.Errorf("setting %q (%s) requires exactly one explicitly selected device for rank %d", name, key, rank)
			}
			nodeID = placement.NodeID
		}
		if nodeID == "" {
			return nil, fmt.Errorf("setting %q (%s) requires an explicitly selected device for rank %d", name, key, rank)
		}
		inv := reports[nodeID]
		if inv == nil {
			node, err := s.q.GetNode(ctx, nodeID)
			if err != nil {
				return nil, fmt.Errorf("setting %q (%s): selected node %q is unavailable: %w", name, key, nodeID, err)
			}
			if !node.Inventory.Valid || node.Inventory.String == "" || node.Inventory.String == "null" {
				return nil, fmt.Errorf("setting %q (%s): selected node %q has no agent inventory", name, key, nodeID)
			}
			inv, err = inventory.Parse(node.Inventory.String)
			if err != nil {
				return nil, fmt.Errorf("setting %q (%s): selected node %q has invalid agent inventory: %w", name, key, nodeID, err)
			}
			reports[nodeID] = inv
		}
		var value string
		var err error
		if fact == "SSH" || fact == "HOST" {
			value, err = s.defaultWorkerSSH(ctx, workload, req.Placements, nodeID, inv, sshTargets)
		} else {
			value, err = workerSettingFact(fact, inv, *m.Metadata.Source)
		}
		if err != nil {
			return nil, fmt.Errorf("setting %q (%s) for selected node %q: %w", name, key, nodeID, err)
		}
		if previous, exists := resolved[name]; exists {
			if previous != value {
				return nil, fmt.Errorf("setting %q has conflicting selected-device bindings; provide an explicit setting", name)
			}
			continue
		}
		if !copied {
			// Never mutate caller/profile overrides while adding implicit values.
			resolved = maps.Clone(parameters)
			if resolved == nil {
				resolved = map[string]any{}
			}
			copied = true
		}
		resolved[name] = value
	}
	return resolved, nil
}

// Profiles may omit context-dependent settings, but still validate every
// supplied value and unrelated requirement. The manifest is private to profile
// validation; planning always uses the original required declarations.
func deferProfileDeviceSettings(m *recipe.Manifest) {
	if m.Metadata.Source == nil {
		return
	}
	for _, workload := range m.Workloads {
		if workload.Upstream == nil || !slices.Contains(workload.Permissions, "host.upstream-exec") {
			continue
		}
		for key, binding := range workload.Env {
			if _, _, ok := workerSettingBinding(key); !ok {
				continue
			}
			if parameter := workerSettingParameter(m, binding); parameter != nil {
				parameter.Optional = true
			}
		}
	}
}

func workerSettingParameter(m *recipe.Manifest, binding string) *recipe.Parameter {
	if !strings.HasPrefix(binding, "${setting.") || !strings.HasSuffix(binding, "}") {
		return nil
	}
	name := strings.TrimSuffix(strings.TrimPrefix(binding, "${setting."), "}")
	parameter := m.ParameterByName(name)
	if parameter == nil || parameter.Type != "string" || parameter.Optional || parameter.Sensitive || parameter.Default != nil {
		return nil
	}
	return parameter
}

// Authored launchers use WORKER_* for rank 1 and WORKER2_* or WORKER_2_* for rank 2.
func workerSettingBinding(key string) (int32, string, bool) {
	prefix, fact, ok := strings.Cut(key, "_")
	if !ok || !strings.HasPrefix(prefix, "WORKER") {
		return 0, "", false
	}
	if prefix == "WORKER" && len(fact) > 0 && fact[0] >= '0' && fact[0] <= '9' {
		number, remainder, found := strings.Cut(fact, "_")
		if !found {
			return 0, "", false
		}
		prefix, fact = prefix+number, remainder
	}
	rank := int64(1)
	if suffix := strings.TrimPrefix(prefix, "WORKER"); suffix != "" {
		parsed, err := strconv.ParseInt(suffix, 10, 32)
		if err != nil || parsed < 1 || strconv.FormatInt(parsed, 10) != suffix {
			return 0, "", false
		}
		rank = parsed
	}
	switch fact {
	case "SSH", "HOST", "USER", "HOME", "SCRIPT_DIR", "HF_CACHE":
		return int32(rank), fact, true
	default:
		return 0, "", false
	}
}

func workerSettingFact(fact string, inv *inventory.Inventory, source recipe.Source) (string, error) {
	switch fact {
	case "USER":
		if inv.AgentUsername == "" {
			return "", fmt.Errorf("agent inventory does not report the process account username")
		}
		return inv.AgentUsername, nil
	case "HOME":
		if !path.IsAbs(inv.AgentHome) {
			return "", fmt.Errorf("agent inventory does not report an absolute process home directory")
		}
		return path.Clean(inv.AgentHome), nil
	case "SCRIPT_DIR":
		if !path.IsAbs(inv.AgentWorkspace) {
			return "", fmt.Errorf("agent inventory does not report an absolute workspace directory")
		}
		identity, _, _, err := recipe.RepositoryIdentity(source)
		if err != nil || source.Revision == "" {
			return "", fmt.Errorf("recipe has no pinned source identity for a worker deployment directory")
		}
		digest := sha256.Sum256([]byte(identity + "\x00" + source.Revision))
		return path.Join(inv.AgentWorkspace, "worker-deployments", fmt.Sprintf("%x", digest)), nil
	case "HF_CACHE":
		for _, root := range inv.CacheRoots {
			if root.Backend == "huggingface" && root.Writable && path.IsAbs(root.Path) && path.Base(path.Clean(root.Path)) != "hub" {
				return path.Clean(root.Path), nil
			}
		}
		// HF_CACHE is the HF home, not the hub subdirectory. A configured hub
		// alone proves neither its parent's layout nor its parent's writability.
		if !path.IsAbs(inv.AgentHome) {
			return "", fmt.Errorf("agent inventory reports neither a writable Hugging Face root nor an absolute home directory")
		}
		fallback := path.Join(inv.AgentHome, ".cache", "huggingface")
		for _, root := range inv.CacheRoots {
			if path.Clean(root.Path) == fallback && !root.Writable {
				return "", fmt.Errorf("agent inventory reports the default Hugging Face root as not writable")
			}
		}
		return fallback, nil
	default:
		return "", fmt.Errorf("unsupported worker fact %q", fact)
	}
}
