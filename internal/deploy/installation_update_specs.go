package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
)

// installationUpdateSpecs does not plan deployments, acquire GPUs, or consult
// current node addresses. Only saved parameters and frozen installation evidence
// may determine the successor source configuration.
func (s *Service) installationUpdateSpecs(ctx context.Context, nodeID, targetDigest string) ([]recipe.InstallationUpdateSpec, error) {
	targetRow, err := s.q.GetRecipe(ctx, targetDigest)
	if err != nil {
		return nil, err
	}
	target, err := recipe.Parse([]byte(targetRow.Manifest))
	if err != nil {
		return nil, err
	}
	result := []recipe.InstallationUpdateSpec{}
	if !slices.ContainsFunc(target.Workloads, func(w recipe.Workload) bool { return w.Upstream != nil }) {
		return result, nil
	}
	if target.Metadata.Source == nil {
		return nil, fmt.Errorf("recipe.update_source_missing")
	}
	rows, err := s.q.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}
	// Current owners win over removed historical records, regardless of age.
	slices.SortStableFunc(rows, func(a, b db.ListDeploymentsRow) int {
		active := func(state string) int {
			if state == "removed" {
				return 1
			}
			return 0
		}
		return active(a.DesiredState) - active(b.DesiredState)
	})
	seen := map[string]bool{}
	for _, row := range rows {
		persisted := ParsePlacementSet(row.Placement)
		if len(persisted.RanksOnNode(nodeID)) == 0 {
			continue
		}
		oldRow, err := s.q.GetRecipe(ctx, row.RecipeDigest)
		if err != nil {
			return nil, fmt.Errorf("recipe.update_history_missing: deployment %s: %w", row.ID, err)
		}
		old, err := recipe.Parse([]byte(oldRow.Manifest))
		if err != nil {
			return nil, err
		}
		if old.Metadata.Source == nil || old.Metadata.Source.URL != target.Metadata.Source.URL {
			continue
		}
		if old.Metadata.Source.Path != target.Metadata.Source.Path {
			return nil, fmt.Errorf("recipe.update_source_path_changed: deployment %s retains %q, target uses %q", row.ID, old.Metadata.Source.Path, target.Metadata.Source.Path)
		}
		if !slices.ContainsFunc(old.Workloads, func(w recipe.Workload) bool { return w.Upstream != nil }) {
			continue
		}
		if persisted.Workload == nil || *persisted.Workload < 0 || *persisted.Workload >= len(old.Workloads) {
			return nil, fmt.Errorf("recipe.update_history_invalid: deployment %s workload", row.ID)
		}
		workloadIndex := *persisted.Workload
		oldWorkload := &old.Workloads[workloadIndex]
		if oldWorkload.Upstream == nil {
			continue
		}
		if workloadIndex >= len(target.Workloads) || target.Workloads[workloadIndex].Upstream == nil {
			return nil, fmt.Errorf("recipe.update_workload_changed: deployment %s", row.ID)
		}
		for _, rank := range persisted.RanksOnNode(nodeID) {
			var preview *UpstreamPreview
			for i := range persisted.Upstream {
				if persisted.Upstream[i].NodeID == nodeID && persisted.Upstream[i].Rank == rank {
					preview = &persisted.Upstream[i]
					break
				}
			}
			if preview == nil {
				return nil, fmt.Errorf("recipe.update_history_missing: deployment %s node %s rank %d has no frozen upstream inputs", row.ID, nodeID, rank)
			}
			names := append(append([]string{}, preview.Execution.Containers...), preview.Execution.AuxiliaryContainers...)
			slices.Sort(names)
			key := strconv.Itoa(int(rank)) + "\x00" + strings.Join(names, "\x00")
			if seen[key] {
				continue
			}
			input := recipe.InstallationUpdateSpec{Workload: workloadIndex, Rank: int(rank), Parameters: map[string]any{}, Bindings: map[string]string{recipe.TemplNodeID: nodeID}}
			saved := parametersForValue(row.Parameters)
			for _, parameter := range target.Parameters {
				if value, ok := saved[parameter.Name]; ok {
					input.Parameters[parameter.Name] = value
				} else if parameter.Default != nil {
					input.Parameters[parameter.Name] = parameter.Default
				}
			}
			// A compiler may expose a previously frozen environment input as a
			// setting. Preserve its exact value instead of asking for SSH/home again.
			for key, template := range target.Workloads[input.Workload].Env {
				if !strings.HasPrefix(template, recipe.TemplSetting) || !strings.HasSuffix(template, "}") || len(recipe.TemplateVars(template)) != 1 {
					continue
				}
				name := strings.TrimSuffix(strings.TrimPrefix(template, recipe.TemplSetting), "}")
				if _, savedValue := saved[name]; savedValue {
					continue
				}
				if value, ok := preview.Environment[key]; ok {
					input.Parameters[name] = value
				}
			}
			if placement := persisted.EntryFor(rank); placement != nil && placement.acceleratorBinding() != "" {
				input.Bindings[recipe.TemplNodeAccelerators] = placement.acceleratorBinding()
			}
			for _, peer := range persisted.Upstream {
				input.Bindings[fmt.Sprintf("${cluster.node.%d.id}", peer.Rank)] = peer.NodeID
			}
			if err := recoverInstallationBindings(old, oldWorkload, preview, saved, input.Bindings); err != nil {
				return nil, fmt.Errorf("recipe.update_history_invalid: deployment %s: %w", row.ID, err)
			}
			execution, environment, configuration, err := recipe.RenderInstallationUpstream(target, input)
			if err != nil {
				return nil, fmt.Errorf("recipe.update_reconstruction_failed: deployment %s node %s rank %d: %w", row.ID, nodeID, rank, err)
			}
			spec := runtime.ContainerSpec{
				Name:     containerName(row.ID, row.RunID.String, rank),
				Labels:   map[string]string{runtime.LabelManaged: "true", runtime.LabelDeployment: row.ID, runtime.LabelRun: row.RunID.String, runtime.LabelRecipe: targetDigest, runtime.LabelRecipeVersion: recipeVersion(targetDigest), runtime.LabelRank: strconv.Itoa(int(rank)), runtime.LabelModule: "serving"},
				Upstream: &runtime.UpstreamSpec{SourceURL: target.Metadata.Source.URL, Revision: target.Metadata.Source.Revision, SourcePath: target.Metadata.Source.Path, Install: execution.Install, Start: execution.Start, Stop: execution.Stop, Containers: execution.Containers, AuxiliaryContainers: execution.AuxiliaryContainers, EnvFile: execution.EnvFile, EnvTemplate: execution.EnvTemplate, EnvFormat: execution.EnvFormat, LogFile: execution.LogFile, Approved: true, Configuration: configuration, ObserveOnly: execution.CoordinatorRank != nil && *execution.CoordinatorRank != int(rank)},
			}
			keys := make([]string, 0, len(environment))
			for key := range environment {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				spec.Env = append(spec.Env, key+"="+environment[key])
			}
			input.Spec, err = json.Marshal(spec)
			if err != nil {
				return nil, err
			}
			result = append(result, input)
			seen[key] = true
		}
	}
	return result, nil
}

// Recover only unambiguous substitutions from the frozen authored contract.
// Multiple adjacent unknown substitutions are not guessed; rendering the target
// will fail precisely if it needs evidence the old installation did not retain.
func recoverInstallationBindings(manifest *recipe.Manifest, workload *recipe.Workload, preview *UpstreamPreview, parameters map[string]any, bindings map[string]string) error {
	type observation struct{ template, value string }
	var observations []observation
	for key, template := range workload.Env {
		if value, ok := preview.Environment[key]; ok {
			observations = append(observations, observation{template, value})
		}
	}
	pair := func(templates, values []string) {
		if len(templates) == len(values) {
			for i := range templates {
				observations = append(observations, observation{templates[i], values[i]})
			}
		}
	}
	execution := workload.Upstream
	pair(execution.Start, preview.Execution.Start)
	pair(execution.Stop, preview.Execution.Stop)
	names := execution.Containers
	if byRank, ok := execution.ContainersByRank[int(preview.Rank)]; ok {
		names = byRank
	}
	pair(names, preview.Execution.Containers)
	pair(execution.AuxiliaryContainers, preview.Execution.AuxiliaryContainers)
	install := execution.Install
	if byRank, ok := execution.InstallByRank[int(preview.Rank)]; ok {
		install = byRank
	}
	if len(install) == len(preview.Execution.Install) {
		for i := range install {
			pair(install[i], preview.Execution.Install[i])
		}
	}
	for changed := true; changed; {
		changed = false
		for _, observation := range observations {
			template := observation.template
			for _, token := range recipe.TemplateVars(template) {
				if value, ok := bindings[token]; ok {
					template = strings.ReplaceAll(template, token, value)
					continue
				}
				if token == recipe.TemplNodeRank || strings.HasPrefix(token, recipe.TemplSetting) {
					if value, ok := (recipe.RenderContext{NodeRank: int(preview.Rank), Settings: parameters}).Resolve(token); ok {
						template = strings.ReplaceAll(template, token, value)
					}
				}
			}
			unknown := recipe.TemplateVars(template)
			if len(unknown) == 0 && template != observation.value {
				return fmt.Errorf("frozen template has inconsistent saved values")
			}
			if len(unknown) != 1 || strings.Count(template, unknown[0]) != 1 {
				continue
			}
			prefix, suffix, _ := strings.Cut(template, unknown[0])
			if !strings.HasPrefix(observation.value, prefix) || !strings.HasSuffix(observation.value, suffix) || len(observation.value) < len(prefix)+len(suffix) {
				return fmt.Errorf("frozen template does not match %s", unknown[0])
			}
			value := observation.value[len(prefix) : len(observation.value)-len(suffix)]
			if previous, ok := bindings[unknown[0]]; ok {
				if previous != value {
					return fmt.Errorf("frozen template has inconsistent binding %s", unknown[0])
				}
				continue
			}
			bindings[unknown[0]] = value
			changed = true
		}
	}
	return nil
}

func installationSpecDigests(specs map[string][]recipe.InstallationUpdateSpec) map[string]string {
	if specs == nil {
		return nil
	}
	digests := make(map[string]string, len(specs))
	for node, inputs := range specs {
		raw, _ := json.Marshal(inputs)
		var canonical any
		_ = json.Unmarshal(raw, &canonical)
		raw, _ = json.Marshal(canonical)
		digests[node] = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
	}
	return digests
}

// Package placements prove acquisition, not source preparation. A durable
// per-device success under this explicit protocol is the latter's receipt.
func (s *Service) installationUpdatePrepared(ctx context.Context, repositoryID, digest, nodeID string, specs []recipe.InstallationUpdateSpec) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input, progress FROM runs
		WHERE kind = 'recipe-update' AND json_extract(input, '$.package_install_only') = 1
		AND json_extract(input, '$.repository_id') = ? AND json_extract(input, '$.target_digest') = ?`, repositoryID, digest)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	want := installationSpecDigests(map[string][]recipe.InstallationUpdateSpec{nodeID: specs})[nodeID]
	for rows.Next() {
		var inputJSON, progressJSON string
		if err := rows.Scan(&inputJSON, &progressJSON); err != nil {
			return false, err
		}
		var input repositoryUpdateRunInput
		var progress repositoryUpdateProgress
		if json.Unmarshal([]byte(inputJSON), &input) != nil || json.Unmarshal([]byte(progressJSON), &progress) != nil {
			continue
		}
		if installationSpecDigests(input.InstallationSpecs)[nodeID] != want {
			continue
		}
		for _, device := range progress.InstalledDevices {
			if device.NodeID == nodeID && device.Status == "succeeded" {
				return true, nil
			}
		}
	}
	return false, rows.Err()
}
