package repositorycompiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/syntax"
)

// Discover consumed external defaults, not every variable mentioned in a script.
// Local/computed assignments disqualify a name unless its assignment explicitly
// preserves that same environment input. Traversal includes authored functions.
func (c *configurationCollector) externalInputs(file *syntax.File, source string) {
	assigned := make(map[string]bool)
	self := make(map[string]bool)
	syntax.Walk(file, func(node syntax.Node) bool {
		if a, ok := node.(*syntax.Assign); ok && a.Name != nil {
			assigned[a.Name.Value] = true
			if selfDefault(a.Value, a.Name.Value) != nil {
				self[a.Name.Value] = true
			}
		}
		return true
	})
	syntax.Walk(file, func(node syntax.Node) bool {
		p, ok := node.(*syntax.ParamExp)
		if !ok || p.Exp == nil || p.Exp.Op != syntax.DefaultUnset && p.Exp.Op != syntax.DefaultUnsetOrNull {
			return true
		}
		name := p.Param.Value
		if _, _, valid := assignmentText(name + "="); !valid || assigned[name] && !self[name] {
			return true
		}
		value, _ := constantWord(p.Exp.Word)
		c.bindEnvironment(name, value, "Consumed by the original launch procedure.", source, true, c.workload.Upstream.EnvFormat == "literal")
		return true
	})
}

func (c *configurationCollector) configurationFile(content []byte, source string) *sourceconfig.File {
	for i := range c.workload.Upstream.Configuration {
		if c.workload.Upstream.Configuration[i].Path == source {
			return &c.workload.Upstream.Configuration[i]
		}
	}
	digest := sha256.Sum256(content)
	c.workload.Upstream.Configuration = append(c.workload.Upstream.Configuration, sourceconfig.File{Path: source, SHA256: hex.EncodeToString(digest[:])})
	return &c.workload.Upstream.Configuration[len(c.workload.Upstream.Configuration)-1]
}

func mappedPosition(positions []int, pos syntax.Pos) int {
	if positions == nil {
		return int(pos.Offset())
	}
	return positions[pos.Offset()]
}

func (c *configurationCollector) scalarEdit(configuration *sourceconfig.File, key, value, format, group string, start, end int) {
	if c.parameter(key) == nil {
		p := inferredParameter(key, value)
		p.Label, p.Group, p.Optional = humanLabel(key), group, true
		p.Default = nil
		p.Description = configuration.Path + ": original " + key + " input. Unset preserves the authored value and branch; explicit values reach the original invocation. Upstream validation still applies."
		c.manifest.Parameters = append(c.manifest.Parameters, p)
	}
	configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: start, End: end, Parameter: key, Format: format})
}

func engineFlagManaged(flag string) bool {
	for _, protected := range protectedServerArguments("vllm") {
		if flag == protected {
			return true
		}
	}
	return flag == "--enable-expert-parallel"
}

// engineWords operates on original argv arrays or the actual Compose exec,
// never on dead helper arrays or an unrestricted extra-argument surrogate.
func (c *configurationCollector) engineWords(configuration *sourceconfig.File, words []*syntax.Word, positions []int, format string) {
	for i := 0; i < len(words); i++ {
		flag, known := constantWord(words[i])
		if !known || !strings.HasPrefix(flag, "--") {
			continue
		}
		valueIndex := i + 1
		hasValue := valueIndex < len(words)
		if hasValue {
			next, constant := constantWord(words[valueIndex])
			hasValue = !constant || !strings.HasPrefix(next, "--")
			if arrayExpansion(words[valueIndex]) {
				hasValue = false
			}
		}
		if strings.HasPrefix(flag, "--enable-") || strings.HasPrefix(flag, "--no-enable-") || flag == "--trust-remote-code" || flag == "--enforce-eager" || flag == "--language-model-only" || flag == "--skip-mm-profiling" {
			hasValue = false
		}
		if engineFlagManaged(flag) {
			if hasValue {
				i++
			}
			continue
		}
		key := strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_")
		if !hasValue {
			if _, controlled := c.workload.Env[strings.ToUpper(key)]; controlled {
				continue
			}
			if c.parameter(key) == nil {
				c.manifest.Parameters = append(c.manifest.Parameters, recipe.Parameter{Name: key, Label: humanLabel(key), Group: "vLLM runtime", Type: "bool", Optional: true, Description: configuration.Path + ": false omits the authored " + flag + " flag; engine defaults still apply. True retains the flag in its original branch. Unset preserves the original command."})
			}
			flagFormat := "flag"
			if format == "shell-compose" {
				flagFormat = "flag-compose"
			}
			configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, words[i].Pos()), End: mappedPosition(positions, words[i].End()), Parameter: key, Format: flagFormat, Flag: flag})
			continue
		}
		i++
		word := words[valueIndex]
		value, constant := constantWord(word)
		if !constant {
			value, constant = constantPrintf(word)
		}
		if constant {
			c.scalarEdit(configuration, key, value, format, "vLLM runtime", mappedPosition(positions, word.Pos()), mappedPosition(positions, word.End()))
		} else if format == "shell-reparse" {
			// Qwen deliberately flattens these array words for a later shell.
			// Serialize their *selected* runtime value, leaving the original
			// outer double quotes to keep printf's result one array element.
			if q, ok := soleDoubleQuote(word); ok {
				parts := q.Parts
				if len(parts) == 3 {
					left, l := parts[0].(*syntax.Lit)
					right, r := parts[2].(*syntax.Lit)
					if l && r && left.Value == "'" && right.Value == "'" {
						parts = parts[1:2]
					}
				}
				if len(parts) == 1 {
					if p, ok := parts[0].(*syntax.ParamExp); ok && simpleVariable(&syntax.Word{Parts: []syntax.WordPart{p}}) != "" {
						configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, q.Pos()) + 1, End: mappedPosition(positions, q.End()) - 1, Variable: p.Param.Value, Format: "shell-word"})
					}
				}
			}
		}
	}
}

func soleDoubleQuote(word *syntax.Word) (*syntax.DblQuoted, bool) {
	if len(word.Parts) != 1 {
		return nil, false
	}
	q, ok := word.Parts[0].(*syntax.DblQuoted)
	return q, ok
}

// The reviewed dual-Qwen compilation JSON uses a constant printf command.
// No evaluation occurs here: only a one-argument printf with no directives.
func constantPrintf(word *syntax.Word) (string, bool) {
	q, ok := soleDoubleQuote(word)
	if !ok || len(q.Parts) != 1 {
		return "", false
	}
	sub, ok := q.Parts[0].(*syntax.CmdSubst)
	if !ok || len(sub.Stmts) != 1 {
		return "", false
	}
	call, ok := sub.Stmts[0].Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) != 0 || len(call.Args) != 2 || call.Args[0].Lit() != "printf" {
		return "", false
	}
	value, ok := constantWord(call.Args[1])
	if !ok || strings.ContainsAny(value, "%\\") {
		return "", false
	}
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		value = value[1 : len(value)-1]
	}
	return value, true
}

func (c *configurationCollector) dockerWords(configuration *sourceconfig.File, words []*syntax.Word, positions []int, format string) {
	for i := range len(words) - 1 {
		flag, ok := constantWord(words[i])
		if !ok {
			continue
		}
		value, constant := constantWord(words[i+1])
		if !constant {
			continue
		}
		word := words[i+1]
		switch flag {
		case "--shm-size", "--stop-timeout", "--cap-add", "--ulimit":
			key := "docker_" + strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_")
			if flag == "--ulimit" {
				name, _, _ := strings.Cut(value, "=")
				key += "_" + name
			}
			if flag == "--cap-add" {
				key += "_" + strings.ToLower(value)
			}
			c.scalarEdit(configuration, key, value, format, "Docker runtime", mappedPosition(positions, word.Pos()), mappedPosition(positions, word.End()))
		case "-e", "--env":
			name, selected, found := strings.Cut(value, "=")
			if !found || managedEnvironment(name) || name == "HF_HOME" || name == "TRITON_CACHE_DIR" || name == "VLLM_CACHE_ROOT" || name == "VLLM_PLE_PACKED_TABLE_DIR" || name == "VLLM_PLE_CPU_OFFLOAD" {
				continue
			}
			// These reviewed live constants are unquoted KEY=value words. Edit
			// only the value: its environment name remains source-owned.
			if len(word.Parts) != 1 {
				continue
			}
			lit, ok := word.Parts[0].(*syntax.Lit)
			if !ok || lit.Value != value {
				continue
			}
			start := mappedPosition(positions, word.Pos()) + len(name) + 1
			c.scalarEdit(configuration, strings.ToLower(name), selected, format, parameterGroup(name), start, mappedPosition(positions, word.End()))
		}
	}
}

func (c *configurationCollector) runtimeConfiguration(file *syntax.File, content []byte, source string) error {
	configuration := c.configurationFile(content, source)
	var failure error
	syntax.Walk(file, func(node syntax.Node) bool {
		if failure != nil {
			return false
		}
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name == nil {
				return true
			}
			if n.Name.Value == "IMAGE" && n.Array == nil {
				if value, ok := constantWord(n.Value); ok && value != "" {
					c.scalarEdit(configuration, "image", value, "shell", "Upstream dependencies", int(n.Value.Pos().Offset()), int(n.Value.End().Offset()))
				}
			}
			if n.Array != nil && n.Name.Value == "VLLM_ARGS" {
				var words []*syntax.Word
				for _, element := range n.Array.Elems {
					words = append(words, element.Value)
				}
				c.engineWords(configuration, words, nil, "shell-reparse")
			}
			if n.Array != nil && n.Name.Value == "nccl_common" {
				var words []*syntax.Word
				for _, element := range n.Array.Elems {
					words = append(words, element.Value)
				}
				c.dockerWords(configuration, words, nil, "shell")
			}
			if n.Value != nil && (n.Name.Value == "serve_env" || n.Name.Value == "worker_nccl" || n.Name.Value == "WORKER_HF_COMPOSE_ENV") {
				c.serializedEnvironment(configuration, n, content)
			}
			if n.Name.Value == "worker_preload" && n.Value != nil {
				if q, ok := soleDoubleQuote(n.Value); ok {
					start, end := int(q.Pos().Offset())+1, int(q.End().Offset())-1
					if bytes.HasPrefix(content[start:end], []byte("-v ")) {
						decoded, positions := heredocShell(content[start:end], start, false)
						inner, err := parseShell(decoded, source+" (worker preload)")
						if err != nil {
							failure = err
							return false
						}
						if len(inner.Stmts) == 1 {
							if call, ok := inner.Stmts[0].Cmd.(*syntax.CallExpr); ok {
								for _, word := range call.Args {
									c.generatedWord(configuration, word, positions)
								}
							}
						}
					}
				}
			}
			// Preserve the original worker shared/local cache selection; quote
			// the mount argument when its original string is assembled.
			if n.Name.Value == "WORKER_HF_MOUNT" && n.Value != nil {
				if q, ok := soleDoubleQuote(n.Value); ok {
					for _, part := range q.Parts {
						if p, ok := part.(*syntax.ParamExp); ok && (p.Param.Value == "REMOTE_HF" || p.Param.Value == "NFS_VOLUME") {
							configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: int(p.Pos().Offset()), End: int(p.End().Offset()), Variable: p.Param.Value, Format: "shell-word"})
						}
					}
				}
			}
		case *syntax.CallExpr:
			if len(n.Args) >= 2 && n.Args[0].Lit() == "docker" && n.Args[1].Lit() == "run" {
				serving := false
				for _, word := range n.Args {
					if word.Lit() == "sglang.launch_server" || word.Lit() == "/start.sh" {
						serving = true
					}
				}
				if serving {
					c.dockerWords(configuration, n.Args, nil, "shell")
				}
			}
			if len(n.Args) == 2 && n.Args[0].Lit() == "worker_ssh" {
				q, ok := soleDoubleQuote(n.Args[1])
				if ok {
					start, end := int(q.Pos().Offset())+1, int(q.End().Offset())-1
					body := content[start:end]
					if bytes.HasPrefix(body, []byte("docker run ")) {
						decoded, positions := heredocShell(body, start, false)
						inner, err := parseShell(decoded, source+" (worker command)")
						if err != nil {
							failure = err
							return false
						}
						c.generatedRuntime(configuration, inner, positions, "shell-double-quoted", true)
					}
				}
			}
		}
		return true
	})
	if failure != nil {
		return fmt.Errorf("reviewed runtime: %w", failure)
	}
	fragments, err := generatedBashHeredocs(file, content)
	if err != nil {
		return fmt.Errorf("reviewed runtime: %w", err)
	}
	for _, fragment := range fragments {
		decoded, positions := heredocShell(fragment.body, fragment.offset, fragment.quoted)
		inner, err := parseShell(decoded, source+" (generated runtime)")
		if err != nil {
			return fmt.Errorf("reviewed runtime: %w", err)
		}
		format := "shell-heredoc"
		if fragment.quoted {
			format = "shell-quoted-heredoc"
		}
		c.generatedRuntime(configuration, inner, positions, format, !fragment.quoted)
	}
	return nil
}

func (c *configurationCollector) generatedRuntime(configuration *sourceconfig.File, file *syntax.File, positions []int, format string, serialize bool) {
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Assign:
			if n.Name != nil && n.Name.Value == "ARGS" && n.Array != nil && !n.Append {
				var words []*syntax.Word
				for _, element := range n.Array.Elems {
					words = append(words, element.Value)
				}
				c.engineWords(configuration, words, positions, format)
			}
		case *syntax.CallExpr:
			if len(n.Args) < 2 || n.Args[0].Lit() != "docker" || n.Args[1].Lit() != "run" {
				return true
			}
			c.dockerWords(configuration, n.Args, positions, format)
			if serialize {
				for _, word := range n.Args {
					c.generatedWord(configuration, word, positions)
				}
			}
		}
		return true
	})
}

var generatedCommandName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,255}$`)

// Only scalar expansions are serialized. Inherited argv strings and mounts
// assembled as flag+value pairs retain their authored expansion boundaries.
func (c *configurationCollector) generatedWord(configuration *sourceconfig.File, word *syntax.Word, positions []int) {
	if p := simpleExpansion(word); p != nil && p.Exp != nil && (p.Exp.Op == syntax.AlternateUnsetOrNull || p.Exp.Op == syntax.AlternateUnset) {
		syntax.Walk(p.Exp.Word, func(node syntax.Node) bool {
			if inner, ok := node.(*syntax.ParamExp); ok && simpleVariable(&syntax.Word{Parts: []syntax.WordPart{inner}}) != "" {
				configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, inner.Pos()), End: mappedPosition(positions, inner.End()), Variable: inner.Param.Value, Format: "shell-word"})
			}
			return true
		})
		return
	}
	// Adjacent unquoted scalar expansions form one generated word. Quote each
	// selected component before their authored literal separators are joined.
	var scalars []*syntax.ParamExp
	plain := true
	for _, part := range word.Parts {
		switch p := part.(type) {
		case *syntax.Lit:
		case *syntax.ParamExp:
			if simpleVariable(&syntax.Word{Parts: []syntax.WordPart{p}}) == "" {
				plain = false
			}
			scalars = append(scalars, p)
		default:
			plain = false
		}
	}
	if plain && len(scalars) > 1 {
		for _, p := range scalars {
			configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, p.Pos()), End: mappedPosition(positions, p.End()), Variable: p.Param.Value, Format: "shell-word"})
		}
		return
	}
	parts := word.Parts
	if q, ok := soleDoubleQuote(word); ok {
		parts = q.Parts
	}
	// In a worker_ssh string the generated word may itself be single quoted;
	// its $ is expanded by the outer shell, before the worker parses it.
	var expanded []syntax.WordPart
	for _, part := range parts {
		if q, ok := part.(*syntax.SglQuoted); ok && strings.Contains(q.Value, "$") {
			parsed, err := parseShell([]byte("x="+strconv.Quote(q.Value)), "worker scalar")
			if err != nil || len(parsed.Stmts) != 1 {
				return
			}
			call, ok := parsed.Stmts[0].Cmd.(*syntax.CallExpr)
			if !ok || len(call.Assigns) != 1 {
				return
			}
			quoted, ok := soleDoubleQuote(call.Assigns[0].Value)
			if !ok {
				return
			}
			expanded = append(expanded, quoted.Parts...)
		} else {
			expanded = append(expanded, part)
		}
	}
	parts = expanded
	// A sole authored command substitution is one scalar too. Retain only
	// zero-argument identifier calls, never reconstruct arbitrary shell code.
	if len(parts) == 1 {
		if sub, ok := parts[0].(*syntax.CmdSubst); ok && len(sub.Stmts) == 1 {
			stmt := sub.Stmts[0]
			call, ok := stmt.Cmd.(*syntax.CallExpr)
			if ok && !stmt.Negated && !stmt.Background && !stmt.Coprocess && len(stmt.Redirs) == 0 && len(call.Assigns) == 0 && len(call.Args) == 1 {
				command := call.Args[0].Lit()
				if generatedCommandName.MatchString(command) {
					configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, word.Pos()), End: mappedPosition(positions, word.End()), Command: command, Format: "shell-word"})
				}
			}
			return
		}
	}
	var variable, prefix, suffix string
	for _, part := range parts {
		switch p := part.(type) {
		case *syntax.Lit:
			if variable == "" {
				prefix += p.Value
			} else {
				suffix += p.Value
			}
		case *syntax.ParamExp:
			if variable != "" || simpleVariable(&syntax.Word{Parts: []syntax.WordPart{p}}) == "" {
				return
			}
			variable = p.Param.Value
		default:
			return
		}
	}
	if variable == "" {
		return
	}
	// These are already serialized multiword argv, not scalar values.
	if strings.Contains(variable, "ARGS") || strings.Contains(variable, "MOUNT") || strings.HasSuffix(variable, "_STR") || variable == "PLE_OFFLOAD_ENV" || variable == "worker_preload" || variable == "worker_nccl" || variable == "serve_env" {
		return
	}
	configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: mappedPosition(positions, word.Pos()), End: mappedPosition(positions, word.End()), Variable: variable, Prefix: prefix, Suffix: suffix, Format: "shell-word"})
}

func (c *configurationCollector) serializedEnvironment(configuration *sourceconfig.File, assignment *syntax.Assign, content []byte) {
	q, ok := soleDoubleQuote(assignment.Value)
	if !ok {
		return
	}
	for _, part := range q.Parts {
		p, ok := part.(*syntax.ParamExp)
		if !ok {
			continue
		}
		indirect := assignment.Name.Value == "serve_env" && p.Excl && p.Param.Value == "v"
		workerCache := assignment.Name.Value == "WORKER_HF_COMPOSE_ENV" && simpleVariable(&syntax.Word{Parts: []syntax.WordPart{p}}) != ""
		if !indirect && !workerCache && !(assignment.Name.Value == "worker_nccl" && p.Param.Value == "e") && p.Param.Value != "VLLM_API_KEY" {
			continue
		}
		start, end := int(p.Pos().Offset()), int(p.End().Offset())
		if content[start-1] == '\'' && content[end] == '\'' {
			start--
			end++
		}
		configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: start, End: end, Variable: p.Param.Value, Indirect: indirect, Format: "shell-word"})
	}
}
