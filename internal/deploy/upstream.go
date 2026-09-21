package deploy

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/diag"
	fabriccfg "github.com/jj-link/local-model-works/internal/fabric"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"strings"
)

// UpstreamPreview is the exact source-owned operation approved for one node.
// Persist it with the placement: dispatch must not resolve changed peer addresses
// or configuration after the operator has approved the plan digest.
type UpstreamPreview struct {
	NodeID        string                      `json:"node_id"`
	NodeName      string                      `json:"node_name,omitempty"`
	Rank          int32                       `json:"rank"`
	Source        recipe.Source               `json:"source"`
	Execution     recipe.UpstreamExecution    `json:"execution"`
	Environment   map[string]string           `json:"environment,omitempty"`
	Configuration []sourceconfig.ResolvedFile `json:"configuration,omitempty"`
}

func (s *Service) previewUpstream(ctx context.Context, plan *Plan, manifest *recipe.Manifest, workload *recipe.Workload, nodes map[int]recipe.RenderNode) error {
	if manifest.Metadata.Source == nil || !slices.Contains(workload.Permissions, "host.upstream-exec") {
		return fmt.Errorf("upstream execution requires a pinned source and explicit host.upstream-exec permission")
	}
	if plan.Fabric != nil {
		fabric, err := s.q.GetFabric(ctx, *plan.Fabric)
		if err != nil {
			return fmt.Errorf("upstream fabric: %w", err)
		}
		bindings := fabriccfg.ParseBindings(fabric.Bindings)
		for rank, node := range nodes {
			for _, binding := range bindings {
				if binding.NodeID != node.NodeID {
					continue
				}
				node.FabricNodeAddr = binding.Address
				node.FabricInterface = binding.InterfaceName
				node.FabricRDMADevice = binding.RDMADevice
				if binding.GIDIndex != nil {
					node.FabricGIDIndex = strconv.Itoa(int(*binding.GIDIndex))
				}
			}
			if node.FabricNodeAddr == "" || node.FabricInterface == "" || (fabric.Transport == fabriccfg.TransportRoCE && (node.FabricRDMADevice == "" || node.FabricGIDIndex == "")) {
				return fmt.Errorf("upstream fabric binding is incomplete for rank %d", rank)
			}
			nodes[rank] = node
		}
	}
	for _, placement := range plan.Placements {
		node := nodes[int(placement.Rank)]
		renderContext := recipe.RenderContext{
			NodeID: node.NodeID, NodeRank: int(placement.Rank), NodeAddress: node.NodeAddress,
			NodeAccelerators: placement.acceleratorBinding(),
			FabricAddr:       nodes[0].FabricNodeAddr, FabricNodeAddr: node.FabricNodeAddr,
			FabricInterface: node.FabricInterface, FabricRDMADevice: node.FabricRDMADevice,
			FabricGIDIndex: node.FabricGIDIndex, Settings: plan.Parameters, Nodes: nodes,
		}
		execution, environment, err := renderUpstream(manifest, workload, renderContext)
		if err != nil {
			return fmt.Errorf("upstream rank %d: %w", placement.Rank, err)
		}
		configuration, err := sourceconfig.Resolve(execution.Configuration, plan.Parameters, func(value string) (string, error) {
			return manifest.Render(value, renderContext)
		})
		if err != nil {
			return fmt.Errorf("upstream rank %d: %w", placement.Rank, err)
		}
		plan.Upstream = append(plan.Upstream, UpstreamPreview{
			NodeID: placement.NodeID, NodeName: placement.NodeName, Rank: placement.Rank,
			Source: *manifest.Metadata.Source, Execution: execution, Environment: environment,
			Configuration: configuration,
		})
	}
	plan.Diagnostics = append(plan.Diagnostics, diag.Warning("upstream.host_execution",
		"The original repository runs as the node agent account, with access to its Docker daemon and host resources. Upstream owns downloads, builds, caches and dependency selection; managed-resource inventory does not constrain those operations. A source commit does not pin upstream image tags or model downloads."))
	return nil
}

func renderUpstream(manifest *recipe.Manifest, workload *recipe.Workload, ctx recipe.RenderContext) (recipe.UpstreamExecution, map[string]string, error) {
	execution := *workload.Upstream
	render := func(values []string) ([]string, error) {
		if values == nil {
			return nil, nil
		}
		out := make([]string, len(values))
		for i, value := range values {
			var err error
			out[i], err = manifest.Render(value, ctx)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	var err error
	if execution.Start, err = render(execution.Start); err != nil {
		return execution, nil, err
	}
	if execution.Stop, err = render(execution.Stop); err != nil {
		return execution, nil, err
	}
	if names, exists := execution.ContainersByRank[ctx.NodeRank]; exists {
		execution.Containers = names
	}
	execution.ContainersByRank = nil
	if execution.Containers, err = render(execution.Containers); err != nil {
		return execution, nil, err
	}
	if execution.AuxiliaryContainers, err = render(execution.AuxiliaryContainers); err != nil {
		return execution, nil, err
	}
	install := execution.Install
	if commands, exists := execution.InstallByRank[ctx.NodeRank]; exists {
		install = commands
	}
	execution.InstallByRank = nil
	execution.Install = nil
	if install != nil {
		execution.Install = make([][]string, len(install))
		for i, command := range install {
			if execution.Install[i], err = render(command); err != nil {
				return execution, nil, err
			}
		}
	}
	environment := make(map[string]string, len(workload.Env))
	for key, value := range workload.Env {
		if strings.HasPrefix(value, recipe.TemplSetting) && strings.HasSuffix(value, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(value, recipe.TemplSetting), "}")
			if parameter := manifest.ParameterByName(name); parameter != nil && parameter.Optional {
				if _, exists := ctx.Settings[name]; !exists {
					continue
				}
			}
		}
		environment[key], err = manifest.Render(value, ctx)
		if err != nil {
			return execution, nil, err
		}
	}
	return execution, environment, nil
}

func frozenUpstreamSpec(row db.GetDeploymentRow, placement *Placement, runID string) (*runtime.ContainerSpec, error) {
	persisted := ParsePlacementSet(row.Placement)
	var reviewed *UpstreamPreview
	for i := range persisted.Upstream {
		candidate := &persisted.Upstream[i]
		if candidate.Rank == placement.Rank && candidate.NodeID == placement.NodeID {
			reviewed = candidate
			break
		}
	}
	if reviewed == nil {
		return nil, fmt.Errorf("%w: the exact upstream lifecycle and target must be reviewed before execution", ErrPlanStale)
	}
	execution := reviewed.Execution
	spec := &runtime.ContainerSpec{
		Name: containerName(row.ID, runID, placement.Rank), AcquisitionPolicy: AcquisitionRequireExisting,
		Upstream: &runtime.UpstreamSpec{
			SourceURL: reviewed.Source.URL, Revision: reviewed.Source.Revision, SourcePath: reviewed.Source.Path,
			Install: execution.Install, Start: execution.Start, Stop: execution.Stop,
			Containers: execution.Containers, AuxiliaryContainers: execution.AuxiliaryContainers,
			EnvFile: execution.EnvFile, EnvTemplate: execution.EnvTemplate, EnvFormat: execution.EnvFormat,
			LogFile: execution.LogFile, Approved: true,
			Configuration: reviewed.Configuration,
			ObserveOnly:   execution.CoordinatorRank != nil && *execution.CoordinatorRank != int(placement.Rank),
		},
		Labels: map[string]string{
			runtime.LabelManaged: "true", runtime.LabelDeployment: row.ID, runtime.LabelRun: runID,
			runtime.LabelRecipe: row.RecipeDigest, runtime.LabelRecipeVersion: recipeVersion(row.RecipeDigest),
			runtime.LabelRank: strconv.Itoa(int(placement.Rank)), runtime.LabelModule: "serving",
		},
	}
	keys := make([]string, 0, len(reviewed.Environment))
	for key := range reviewed.Environment {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		spec.Env = append(spec.Env, key+"="+reviewed.Environment[key])
	}
	if err := applyAcquisitionSpec(spec, persisted, placement.NodeID); err != nil {
		return nil, err
	}
	return spec, nil
}
