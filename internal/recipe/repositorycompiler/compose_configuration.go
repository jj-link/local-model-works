package repositorycompiler

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

func yamlField(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// Compose is an original reviewed dependency of the launcher. Its original
// command, image entrypoint, hotfix ordering and lifecycle are not replaced.
func (c *configurationCollector) composeConfiguration(content []byte, source string) error {
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 {
		return fmt.Errorf("expected Compose document")
	}
	service := yamlField(yamlField(document.Content[0], "services"), "vllm-dspark")
	if service == nil {
		return fmt.Errorf("missing reviewed vllm-dspark service")
	}
	configuration := c.configurationFile(content, source)
	lineOffsets := []int{0}
	for i, ch := range content {
		if ch == '\n' {
			lineOffsets = append(lineOffsets, i+1)
		}
	}
	// The original environment map and volume/image fields contain Compose
	// inputs, including ones never assigned by the parent shell. Parse only
	// these actual YAML scalars; comments and container-side $$ are not inputs.
	var inputs func(*yaml.Node) error
	inputs = func(node *yaml.Node) error {
		if node == nil {
			return nil
		}
		if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "${") {
			text := strings.ReplaceAll(node.Value, "$$", "\\$")
			file, err := parseShell([]byte("value="+strconv.Quote(text)), source+" (Compose input)")
			if err != nil {
				return err
			}
			c.externalInputs(file, source)
		}
		for _, child := range node.Content {
			if err := inputs(child); err != nil {
				return err
			}
		}
		return nil
	}
	for _, field := range []string{"environment", "volumes", "image"} {
		if err := inputs(yamlField(service, field)); err != nil {
			return err
		}
	}
	// Literal YAML scalars require JSON-compatible quoting, not shell syntax.
	for _, item := range []struct {
		node *yaml.Node
		key  string
	}{
		{yamlField(service, "shm_size"), "docker_shm_size"},
		{yamlField(yamlField(service, "environment"), "VLLM_CACHE_ROOT"), "vllm_cache_root"},
	} {
		if item.node == nil || item.node.Kind != yaml.ScalarNode {
			return fmt.Errorf("missing reviewed scalar %s", item.key)
		}
		start := lineOffsets[item.node.Line-1] + item.node.Column - 1
		end := start
		for end < len(content) && content[end] != '\n' && content[end] != '\r' {
			end++
		}
		c.scalarEdit(configuration, item.key, item.node.Value, "json-compose", "Docker runtime", start, end)
	}
	command := yamlField(service, "command")
	if command == nil || command.Kind != yaml.SequenceNode || len(command.Content) != 3 || command.Content[0].Value != "bash" || command.Content[1].Value != "-lc" || command.Content[2].Style != yaml.FoldedStyle {
		return fmt.Errorf("expected reviewed bash -lc folded Compose command")
	}
	// Parse a mapped view of the authored folded shell. Every original line
	// ends in a shell separator except the final exec's continued argv. Folding
	// physical newlines to spaces therefore preserves this reviewed grammar.
	start := lineOffsets[command.Content[2].Line]
	decoded, positions := composeShell(content[start:], start)
	file, err := parseShell(decoded, source+" (Compose command)")
	if err != nil {
		return err
	}
	found := false
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) < 4 || call.Args[0].Lit() != "exec" || call.Args[1].Lit() != "/usr/local/bin/vllm" || call.Args[2].Lit() != "serve" {
			return true
		}
		found = true
		c.engineWords(configuration, call.Args[4:], positions, "shell-compose")
		return true
	})
	if !found {
		return fmt.Errorf("missing reviewed Compose vLLM exec")
	}
	return nil
}

func composeShell(content []byte, base int) ([]byte, []int) {
	var decoded []byte
	positions := make([]int, 0, len(content)+1)
	lineStart := true
	for i := 0; i < len(content); i++ {
		if lineStart && content[i] == ' ' {
			continue
		}
		lineStart = false
		value := content[i]
		at := i
		if value == '$' && i+1 < len(content) && content[i+1] == '$' {
			i++
		}
		if value == '\n' {
			value, lineStart = ' ', true
		}
		positions = append(positions, base+at)
		decoded = append(decoded, value)
	}
	positions = append(positions, base+len(content))
	return bytes.TrimRight(decoded, " "), positions
}
