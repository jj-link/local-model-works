package deploy

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"

	"github.com/jj-link/local-model-works/internal/inventory"
	"github.com/jj-link/local-model-works/internal/recipe"
)

// Host cache settings must resolve before approval, not inside a launcher where
// an unset value can silently select a fresh cache under each source checkout.
func (s *Service) hostCacheDefaults(ctx context.Context, m *recipe.Manifest, workload *recipe.Workload, placements []PlacementOverride, parameters map[string]any) (map[string]any, error) {
	resolved := parameters
	copied := false
	cacheRoot := ""
	for _, parameter := range m.Parameters {
		if _, supplied := parameters[parameter.Name]; supplied || parameter.Type != "string" || parameter.Sensitive || parameter.Default != nil || !hostCacheBinding(workload, parameter.Name) {
			continue
		}
		if cacheRoot == "" {
			for _, placement := range placements {
				if len(workload.Ranks) > 0 && !slices.Contains(workload.Ranks, int(placement.Rank)) {
					continue
				}
				if coordinator := workload.Upstream.CoordinatorRank; coordinator != nil && *coordinator != int(placement.Rank) {
					continue
				}
				node, err := s.q.GetNode(ctx, placement.NodeID)
				if err != nil {
					return nil, fmt.Errorf("host cache setting %q: selected node %q is unavailable: %w", parameter.Name, placement.NodeID, err)
				}
				inv, err := inventory.Parse(node.Inventory.String)
				if !node.Inventory.Valid || err != nil || inv == nil {
					return nil, fmt.Errorf("host cache setting %q: selected node %q has no usable agent inventory", parameter.Name, placement.NodeID)
				}
				root := ""
				for _, candidate := range inv.CacheRoots {
					if candidate.Backend == "huggingface" && candidate.Writable && path.IsAbs(candidate.Path) && path.Base(path.Clean(candidate.Path)) != "hub" {
						root = path.Clean(candidate.Path)
						break
					}
				}
				if root == "" {
					return nil, fmt.Errorf("host cache setting %q: node %q has no configured writable Hugging Face home; configure a shared cache root or supply an explicit setting", parameter.Name, placement.NodeID)
				}
				if cacheRoot != "" && cacheRoot != root {
					return nil, fmt.Errorf("host cache setting %q: selected devices have different cache roots; supply an explicit path valid on every executing device", parameter.Name)
				}
				cacheRoot = root
			}
			if cacheRoot == "" {
				return nil, fmt.Errorf("host cache setting %q requires explicitly selected execution devices or an explicit cache path", parameter.Name)
			}
		}
		if !copied {
			resolved = maps.Clone(parameters)
			if resolved == nil {
				resolved = make(map[string]any)
			}
			copied = true
		}
		resolved[parameter.Name] = cacheRoot
	}
	return resolved, nil
}

func hostCacheBinding(workload *recipe.Workload, parameter string) bool {
	binding := "${setting." + parameter + "}"
	if workload.Env["HF_HOME"] == binding || workload.Env["HF_CACHE"] == binding {
		return true
	}
	// The compiler's hardcoded-cache adapter binds HF_HOME's assignment by
	// parameter, since exporting HF_HOME alone cannot override that assignment.
	if parameter == "hf_home" {
		for _, file := range workload.Upstream.Configuration {
			for _, edit := range file.Edits {
				if edit.Parameter == parameter && edit.Format == "shell" {
					return true
				}
			}
		}
	}
	return false
}
