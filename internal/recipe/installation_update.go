package recipe

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

const MaxInstallationUpdateBytes = 4 << 20
const MaxInstallationUpdateSpecs = 128
const InstallationUpdateProtocolFeature = "recipe-installation-update-v1"

// InstallationUpdateSpec carries reconstruction evidence separately from labels:
// installation settings may contain secrets and must not reach engine telemetry.
// Spec is a runtime.ContainerSpec, decoded strictly by the receiving agent.
type InstallationUpdateSpec struct {
	Spec       json.RawMessage   `json:"spec"`
	Workload   int               `json:"workload"`
	Rank       int               `json:"rank"`
	Parameters map[string]any    `json:"parameters,omitempty"`
	Bindings   map[string]string `json:"bindings,omitempty"`
}

// RenderInstallationUpstream renders only the verified package's authored
// contract. Bindings contain frozen placement/fabric values, never source edits.
func RenderInstallationUpstream(m *Manifest, input InstallationUpdateSpec) (UpstreamExecution, map[string]string, []sourceconfig.ResolvedFile, error) {
	var execution UpstreamExecution
	if input.Workload < 0 || input.Workload >= len(m.Workloads) {
		return execution, nil, nil, fmt.Errorf("recipe.update_workload_invalid")
	}
	workload := &m.Workloads[input.Workload]
	if workload.Upstream == nil || m.Metadata.Source == nil || !slices.Contains(workload.Permissions, "host.upstream-exec") {
		return execution, nil, nil, fmt.Errorf("recipe.update_upstream_invalid")
	}
	if input.Rank < 0 || len(workload.Ranks) > 0 && !slices.Contains(workload.Ranks, input.Rank) || len(workload.Ranks) == 0 && input.Rank >= max(1, m.Compatibility.NodeCount) {
		return execution, nil, nil, fmt.Errorf("recipe.update_rank_invalid")
	}
	render := func(value string) (string, error) {
		var renderErr error
		result := templateVar.ReplaceAllStringFunc(value, func(token string) string {
			if token == TemplNodeRank {
				return strconv.Itoa(input.Rank)
			}
			if strings.HasPrefix(token, TemplSetting) {
				resolved, ok := (RenderContext{Settings: input.Parameters}).Resolve(token)
				if ok {
					return resolved
				}
			} else if resolved, ok := input.Bindings[token]; ok {
				return resolved
			}
			renderErr = fmt.Errorf("recipe.update_binding_missing: %s", token)
			return token
		})
		return result, renderErr
	}
	renderList := func(values []string) ([]string, error) {
		if values == nil {
			return nil, nil
		}
		out := make([]string, len(values))
		for i, value := range values {
			var err error
			out[i], err = render(value)
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	execution = *workload.Upstream
	if names, ok := execution.ContainersByRank[input.Rank]; ok {
		execution.Containers = names
	}
	if commands, ok := execution.InstallByRank[input.Rank]; ok {
		execution.Install = commands
	}
	execution.ContainersByRank, execution.InstallByRank = nil, nil
	var err error
	for _, values := range []*[]string{&execution.Start, &execution.Stop, &execution.Containers, &execution.AuxiliaryContainers} {
		*values, err = renderList(*values)
		if err != nil {
			return execution, nil, nil, err
		}
	}
	if execution.Install != nil {
		commands := make([][]string, len(execution.Install))
		for i, command := range execution.Install {
			commands[i], err = renderList(command)
			if err != nil {
				return execution, nil, nil, err
			}
		}
		execution.Install = commands
	}
	environment := make(map[string]string, len(workload.Env))
	for key, value := range workload.Env {
		if strings.HasPrefix(value, TemplSetting) && strings.HasSuffix(value, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(value, TemplSetting), "}")
			if parameter := m.ParameterByName(name); parameter != nil && parameter.Optional {
				if _, exists := input.Parameters[name]; !exists {
					continue
				}
			}
		}
		environment[key], err = render(value)
		if err != nil {
			return execution, nil, nil, err
		}
	}
	configuration, err := sourceconfig.Resolve(execution.Configuration, input.Parameters, render)
	return execution, environment, configuration, err
}
