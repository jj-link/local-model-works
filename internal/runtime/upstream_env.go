package runtime

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

type upstreamEnvAssignment struct {
	Original []string `json:"original"`
	Applied  []string `json:"applied"`
}

type upstreamEnvState struct {
	Path        string                           `json:"path"`
	Assignments map[string]upstreamEnvAssignment `json:"assignments"`
}

func upstreamAssignmentKey(line string) string {
	candidate := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
	name, _, ok := strings.Cut(candidate, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(name)
}

// Restore only app-owned assignments before applying this run's overrides. This
// makes omission meaningful in a retained .env without replacing unrelated lines.
func renderUpstreamEnv(spec *ContainerSpec, lines []string, statePath string) ([]string, upstreamEnvState, error) {
	previous := upstreamEnvState{}
	next := upstreamEnvState{Path: spec.Upstream.EnvFile, Assignments: make(map[string]upstreamEnvAssignment)}
	if err := readUpstreamJSON(statePath, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, next, err
	}
	if previous.Path != "" && previous.Path != next.Path {
		return nil, next, fmt.Errorf("upstream.env_configuration_path_changed")
	}
	keys := make([]string, 0, len(previous.Assignments))
	for key := range previous.Assignments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		assignment := previous.Assignments[key]
		var actual []string
		for _, line := range lines {
			if upstreamAssignmentKey(line) == key {
				actual = append(actual, line)
			}
		}
		if !slices.Equal(actual, assignment.Applied) {
			return nil, next, fmt.Errorf("upstream.env_configuration_drift: %s; refusing to overwrite upstream changes", key)
		}
		var restored []string
		inserted := false
		for _, line := range lines {
			if upstreamAssignmentKey(line) == key {
				if !inserted {
					restored = append(restored, assignment.Original...)
					inserted = true
				}
				continue
			}
			restored = append(restored, line)
		}
		if !inserted {
			restored = append(restored, assignment.Original...)
		}
		lines = restored
	}
	for _, override := range spec.Env {
		key, value, _ := strings.Cut(override, "=")
		assignment := key + "='" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		if spec.Upstream.EnvFormat == "literal" {
			assignment = key + "=" + value
		}
		state := upstreamEnvAssignment{}
		for i, line := range lines {
			if upstreamAssignmentKey(line) == key {
				state.Original = append(state.Original, line)
				state.Applied = append(state.Applied, assignment)
				lines[i] = assignment
			}
		}
		if len(state.Applied) == 0 {
			lines = append(lines, assignment)
			state.Applied = []string{assignment}
		}
		next.Assignments[key] = state
	}
	return lines, next, nil
}

// Approved lifecycle commands may normalize an assignment's quoting or value.
// Record their result, retaining the pre-override default for later omission.
func recordUpstreamEnv(spec *ContainerSpec, working, statePath string) error {
	if spec.Upstream.EnvFile == "" {
		return nil
	}
	var state upstreamEnvState
	if err := readUpstreamJSON(statePath, &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(state.Assignments) == 0 {
		return nil
	}
	if state.Path != spec.Upstream.EnvFile {
		return fmt.Errorf("upstream.env_configuration_path_changed")
	}
	path, err := upstreamPath(working, state.Path, false)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	actual := make(map[string][]string, len(state.Assignments))
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		key := upstreamAssignmentKey(line)
		if _, tracked := state.Assignments[key]; tracked {
			actual[key] = append(actual[key], line)
		}
	}
	changed := false
	for key, assignment := range state.Assignments {
		if !slices.Equal(assignment.Applied, actual[key]) {
			assignment.Applied = actual[key]
			state.Assignments[key] = assignment
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return writeUpstreamJSON(statePath, state)
}
