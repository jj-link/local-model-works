package repositorycompiler

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

// lifecycleEvidence recognizes the maintained authored shell interfaces, not
// arbitrary shell behavior. It parses commands, function calls and the authored
// generated Bash/SSH command bodies; it never evaluates source or substitutions.
// Unknown expressions cannot establish a container identity or source path.
type lifecycleEvidence struct {
	launch           map[string]bool
	stop             map[string]bool
	inputs           map[string]bool
	clobbered        map[string]bool
	shellEnv         map[string]bool
	literalEnv       map[string]bool
	readSource       func(string) ([]byte, error)
	active           map[string]bool
	startPath        string
	vllmArray        bool
	vllm             bool
	functions        map[string]lifecycleFunction
	tensorfoldArrays map[string][]*syntax.Word
	commandStrings   map[string]quotedCommand
	remoteLaunch     map[string]bool
	remoteStop       map[string]bool
	localLaunch      map[string]bool
	localStop        map[string]bool
	literalParser    map[string]bool
}

func verifyLifecycle(manifest *recipe.Manifest, readSource func(string) ([]byte, error)) error {
	for _, workload := range manifest.Workloads {
		u := workload.Upstream
		if u == nil || len(u.Start) == 0 || len(u.Stop) == 0 {
			return layoutError("", fmt.Errorf("source-owned workload requires authored start and stop entrypoints"))
		}
		install := append([][]string(nil), u.Install...)
		ranks := make([]int, 0, len(u.InstallByRank))
		for rank := range u.InstallByRank {
			ranks = append(ranks, rank)
		}
		sort.Ints(ranks)
		for _, rank := range ranks {
			install = append(install, u.InstallByRank[rank]...)
		}
		for _, command := range install {
			if len(command) == 0 || !strings.HasPrefix(command[0], "./") {
				continue
			}
			content, err := readSource(strings.TrimPrefix(command[0], "./"))
			if err != nil {
				return layoutError(command[0], err)
			}
			file, err := parseShell(content, command[0])
			if err != nil {
				return layoutError(command[0], err)
			}
			if err := verifyScriptArguments(file, command); err != nil {
				return err
			}
		}
		if u.EnvTemplate != "" {
			content, err := readSource(u.EnvTemplate)
			if err != nil {
				return layoutError(u.EnvTemplate, err)
			}
			declared := false
			for _, line := range strings.Split(string(content), "\n") {
				if _, _, ok := assignmentText(strings.TrimSpace(line)); ok {
					declared = true
					break
				}
			}
			if !declared {
				return layoutError(u.EnvTemplate, fmt.Errorf("environment template has no authored assignments"))
			}
		}
		startArgv := u.Start
		singleTensorFold := manifest.Metadata.Engine == "tensorfold" && manifest.Compatibility.NodeCount == 1
		if singleTensorFold {
			// Serving options after the entrypoint are forwarded to tensorfold
			// serve, not dispatched by the script; verifySingleTensorFoldLaunch
			// proves that forwarding.
			if err := verifyTensorFoldServeArguments(u.Start[0], u.Start[1:]); err != nil {
				return err
			}
			startArgv = u.Start[:1]
		}
		start := newLifecycleEvidence(readSource, u.Start[0])
		if err := start.script(startArgv, 0); err != nil {
			return err
		}
		if manifest.Metadata.Engine == "vllm" && !start.vllm {
			return layoutError(u.Start[0], fmt.Errorf("authored lifecycle no longer reaches a supported vLLM serving command"))
		}
		if singleTensorFold {
			if err := verifySingleTensorFoldLaunch(u.Start[0], len(u.Start) > 1, readSource); err != nil {
				return err
			}
		}
		stop := newLifecycleEvidence(readSource, u.Start[0])
		if err := stop.script(u.Stop, 0); err != nil {
			return err
		}
		// A single service may be renamed if both authored endpoints agree,
		// unless its identity is fixed by the maintained environment binding.
		// Multi-rank identities remain explicit; guessing rank placement is unsafe.
		if len(u.Containers) == 1 && len(u.ContainersByRank) == 0 && workload.Env["CONTAINER_NAME"] == "" && !start.launch[u.Containers[0]] {
			var candidates []string
			for name := range start.launch {
				if stop.stop[name] {
					candidates = append(candidates, name)
				}
			}
			if len(candidates) == 1 {
				u.Containers[0] = candidates[0]
			}
		}
		for _, names := range append([][]string{u.Containers}, rankContainers(u.ContainersByRank)...) {
			for _, name := range names {
				if !start.launch[name] {
					return layoutError(u.Start[0], fmt.Errorf("authored launch no longer establishes named container %q", name))
				}
				if !stop.stop[name] {
					return layoutError(u.Stop[0], fmt.Errorf("authored stop no longer tears down named container %q", name))
				}
				if manifest.Metadata.Engine == "tensorfold" && len(u.ContainersByRank) != 0 {
					if !start.localLaunch[name] || !stop.localStop[name] || !start.remoteLaunch[name] || !stop.remoteStop[name] {
						return layoutError(u.Start[0], fmt.Errorf("authored local and SSH lifecycles no longer launch and stop named container %q", name))
					}
				}
			}
		}
		if u.EnvFile != "" {
			read := start.shellEnv[u.EnvFile]
			if u.EnvFormat == "literal" {
				read = start.literalEnv[u.EnvFile]
				if manifest.Metadata.Engine == "tensorfold" {
					read = read && start.literalParser[u.EnvFile]
				}
			}
			if !read {
				return layoutError(u.Start[0], fmt.Errorf("authored launch no longer reads %q with %s environment semantics", u.EnvFile, u.EnvFormat))
			}
		}
		bound := make([]string, 0, len(workload.Env))
		for name := range workload.Env {
			bound = append(bound, name)
		}
		sort.Strings(bound)
		for _, name := range bound {
			if start.clobbered[name] {
				return layoutError(u.Start[0], fmt.Errorf("authored launch unconditionally overwrites bound environment input %s", name))
			}
			if !start.inputs[name] {
				return layoutError(u.Start[0], fmt.Errorf("authored launch no longer consumes bound environment input %s", name))
			}
		}
	}
	return nil
}

func rankContainers(ranks map[int][]string) [][]string {
	var names [][]string
	keys := make([]int, 0, len(ranks))
	for rank := range ranks {
		keys = append(keys, rank)
	}
	sort.Ints(keys)
	for _, rank := range keys {
		names = append(names, ranks[rank])
	}
	return names
}

func newLifecycleEvidence(readSource func(string) ([]byte, error), startPath string) *lifecycleEvidence {
	return &lifecycleEvidence{
		launch: map[string]bool{}, stop: map[string]bool{}, inputs: map[string]bool{},
		clobbered: map[string]bool{},
		shellEnv:  map[string]bool{}, literalEnv: map[string]bool{},
		readSource: readSource, active: map[string]bool{}, startPath: path.Clean(startPath),
		functions: map[string]lifecycleFunction{}, tensorfoldArrays: map[string][]*syntax.Word{},
		commandStrings: map[string]quotedCommand{},
		remoteLaunch:   map[string]bool{}, remoteStop: map[string]bool{},
		localLaunch: map[string]bool{}, localStop: map[string]bool{}, literalParser: map[string]bool{},
	}
}

func (e *lifecycleEvidence) script(argv []string, depth int) error {
	name := path.Clean(strings.TrimPrefix(argv[0], "./"))
	if depth > 8 || e.active[name] {
		return layoutError(name, fmt.Errorf("recursive authored lifecycle is unsupported"))
	}
	content, err := e.readSource(name)
	if err != nil {
		return layoutError(name, err)
	}
	file, err := parseShell(content, name)
	if err != nil {
		return layoutError(name, err)
	}
	if err := verifyScriptArguments(file, argv); err != nil {
		return err
	}
	e.active[name] = true
	defer delete(e.active, name)
	values := map[string]string{"SCRIPT_DIR": ".", "0": "./" + name, "_LMW_SOURCE_DIR": path.Dir(name)}
	for i, arg := range argv[1:] {
		values[strconv.Itoa(i+1)] = arg
	}
	if err := e.shell(file, content, values, depth); err != nil {
		return layoutError(name, err)
	}
	return nil
}

// sourceWord resolves only literals, known scalar assignments and default
// expansions. It deliberately does not invoke expand.Fields or a shell.
func sourceWord(word *syntax.Word, values map[string]string) (string, bool) {
	if word == nil {
		return "", false
	}
	var parts func([]syntax.WordPart) (string, bool)
	parts = func(items []syntax.WordPart) (string, bool) {
		var result strings.Builder
		for _, item := range items {
			switch p := item.(type) {
			case *syntax.Lit:
				result.WriteString(p.Value)
			case *syntax.SglQuoted:
				result.WriteString(p.Value)
			case *syntax.DblQuoted:
				value, ok := parts(p.Parts)
				if !ok {
					return "", false
				}
				result.WriteString(value)
			case *syntax.ParamExp:
				if p.Index != nil || p.Slice != nil || p.Repl != nil || p.Length || p.Excl {
					return "", false
				}
				value, ok := values[p.Param.Value]
				if p.Exp != nil {
					switch p.Exp.Op {
					case syntax.DefaultUnset, syntax.DefaultUnsetOrNull:
						if !ok || (value == "" && p.Exp.Op == syntax.DefaultUnsetOrNull) {
							value, ok = sourceWord(p.Exp.Word, values)
						}
					default:
						return "", false
					}
				}
				if !ok {
					return "", false
				}
				result.WriteString(value)
			default:
				return "", false
			}
		}
		return result.String(), true
	}
	return parts(word.Parts)
}

func (e *lifecycleEvidence) shell(file *syntax.File, content []byte, values map[string]string, depth int) error {
	functions := e.functions
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil || assignment.Array != nil {
				continue
			}
			if _, literal := constantWord(assignment.Value); literal {
				e.clobbered[assignment.Name.Value] = true
			}
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		if fn, ok := node.(*syntax.FuncDecl); ok {
			functions[fn.Name.Value] = lifecycleFunction{fn, content}
			return false
		}
		if p, ok := node.(*syntax.ParamExp); ok {
			e.inputs[p.Param.Value] = true
		}
		return true
	})
	var failure error
	calling := map[string]bool{}
	var walk func(syntax.Node, map[string]string, []byte)
	walk = func(root syntax.Node, scope map[string]string, sourceContent []byte) {
		syntax.Walk(root, func(node syntax.Node) bool {
			if failure != nil {
				return false
			}
			switch n := node.(type) {
			case *syntax.FuncDecl:
				return false
			case *syntax.CaseClause:
				if value, known := sourceWord(n.Word, scope); known {
					for _, item := range n.Items {
						for _, pattern := range item.Patterns {
							text, ok := sourceWord(pattern, scope)
							if !ok {
								continue
							}
							if matches, _ := path.Match(text, value); matches {
								for _, stmt := range item.Stmts {
									walk(stmt, scope, sourceContent)
								}
								return false
							}
						}
					}
					return false
				}
			case *syntax.Stmt:
				if loop, ok := n.Cmd.(*syntax.WhileClause); ok && literalEnvironmentLoop(loop) {
					for _, redirect := range n.Redirs {
						if redirect.Op == syntax.RdrIn {
							if value, known := sourceWord(redirect.Word, scope); known {
								e.literalParser[path.Clean(value)] = true
							}
						}
					}
				}
				// Track the source of a normalized temporary environment file,
				// not an unrelated mention of the original path in a comment.
				if call, ok := n.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 1 && call.Args[0].Lit() == "sed" {
					input, known := sourceWord(call.Args[len(call.Args)-1], scope)
					if known {
						for _, redirect := range n.Redirs {
							if redirect.Op == syntax.RdrOut {
								if variable := simpleVariable(redirect.Word); variable != "" {
									scope[variable] = input
								}
							}
						}
					}
				}
			case *syntax.ParamExp:
				e.inputs[n.Param.Value] = true
			case *syntax.Assign:
				if n.Name != nil && n.Array != nil {
					var words []*syntax.Word
					for _, element := range n.Array.Elems {
						words = append(words, element.Value)
					}
					if n.Append {
						words = append(append([]*syntax.Word(nil), e.tensorfoldArrays[n.Name.Value]...), words...)
					}
					e.tensorfoldArrays[n.Name.Value] = words
				}
				if n.Name != nil && n.Name.Value == "VLLM_ARGS" && n.Array != nil {
					e.vllmArray = true
				}
				// A command built as one double-quoted string (remote="docker run ...")
				// is kept unevaluated so an SSH helper call can parse its source text.
				if n.Name != nil && n.Array == nil && !n.Append {
					if n.Value == nil {
						delete(e.commandStrings, n.Name.Value)
					} else if q, ok := soleDoubleQuote(n.Value); ok {
						e.commandStrings[n.Name.Value] = quotedCommand{q, sourceContent}
					} else {
						delete(e.commandStrings, n.Name.Value)
					}
				}
				if n.Name != nil && n.Array == nil && !n.Append && n.Name.Value != "SCRIPT_DIR" {
					if repositoryRootWord(n.Value) {
						scope[n.Name.Value] = path.Clean(scope["_LMW_SOURCE_DIR"] + "/..")
						return true
					}
					if value, ok := sourceWord(n.Value, scope); ok {
						scope[n.Name.Value] = value
					} else {
						delete(scope, n.Name.Value)
					}
				}
			case *syntax.Redirect:
				if n.Op == syntax.RdrIn {
					if value, ok := sourceWord(n.Word, scope); ok {
						e.literalEnv[path.Clean(value)] = true
					}
				}
			case *syntax.CallExpr:
				if len(n.Args) == 0 {
					return true
				}
				command, _ := sourceWord(n.Args[0], scope)
				if command == "shift" && len(n.Args) == 1 {
					// Ranked SSH helpers consume the worker selector before
					// forwarding "$@"; keep unknown positional slots aligned.
					maximum := 0
					for key := range scope {
						if position, err := strconv.Atoi(key); err == nil && position > maximum {
							maximum = position
						}
					}
					for position := 1; position <= maximum; position++ {
						key := strconv.Itoa(position)
						if value, known := scope[strconv.Itoa(position+1)]; known {
							scope[key] = value
						} else {
							delete(scope, key)
						}
					}
					return false
				}
				if len(n.Args) == 1 && arrayExpansion(n.Args[0]) {
					if p := simpleExpansion(n.Args[0]); p != nil {
						words := e.tensorfoldArrays[p.Param.Value]
						if len(words) >= 2 && words[0].Lit() == "docker" {
							failure = e.docker(words[1:], scope)
						}
					}
				}
				if command == "exec" && len(n.Args) >= 3 && path.Base(n.Args[1].Lit()) == "vllm" && n.Args[2].Lit() == "serve" {
					e.vllm = true
				}
				if fn, exists := functions[command]; exists && !calling[command] {
					local := make(map[string]string, len(scope))
					for key, value := range scope {
						if _, err := strconv.Atoi(key); err != nil {
							local[key] = value
						}
					}
					position := 1
					for _, arg := range n.Args[1:] {
						if offset := forwardedPosition(arg); offset > 0 {
							for i := offset; ; i++ {
								value, exists := scope[strconv.Itoa(i)]
								if !exists {
									break
								}
								local[strconv.Itoa(position)] = value
								position++
							}
						} else {
							if value, ok := sourceWord(arg, scope); ok {
								local[strconv.Itoa(position)] = value
							}
							position++
						}
					}
					calling[command] = true
					walk(fn.declaration.Body, local, fn.content)
					delete(calling, command)
				}
				if (command == "source" || command == ".") && len(n.Args) == 2 {
					if value, ok := sourceWord(n.Args[1], scope); ok {
						e.shellEnv[path.Clean(value)] = true
					}
					if name, literal := literalSource(n, scope["_LMW_SOURCE_DIR"]); literal {
						e.shellEnv[name] = true
						failure = e.sourcedScript(name, scope, depth+1)
					}
				}
				if command == "docker" {
					failure = e.docker(n.Args[1:], scope)
				} else if command == "env" {
					for i, arg := range n.Args[1:] {
						if arg.Lit() == "docker" {
							failure = e.docker(n.Args[i+2:], scope)
							break
						}
					}
				}
				if command == "ssh" {
					failure = e.forwardedSSH(n.Args[1:], scope, depth+1)
				}
				if fn, exists := functions[command]; exists && forwardsSSH(fn.declaration) {
					failure = e.serializedDocker(file, n.Args[1:], scope)
				}
				if fn, exists := functions[command]; exists && failure == nil && serializesPositionalSSH(fn.declaration) {
					remote := make(map[string]string, len(scope)+1)
					for key, value := range scope {
						remote[key] = value
					}
					remote["_LMW_REMOTE"] = "1"
					args := n.Args[1:]
					if len(args) == 1 && arrayExpansion(args[0]) {
						if p := simpleExpansion(args[0]); p != nil {
							args = e.tensorfoldArrays[p.Param.Value]
						}
					}
					if len(args) >= 2 && args[0].Lit() == "docker" {
						failure = e.docker(args[1:], remote)
					}
				}
				if path.Clean(command) == e.startPath && len(n.Args) == 2 && n.Args[1].Lit() == "stop" {
					failure = e.script([]string{command, "stop"}, depth+1)
				}
				// SSH helpers pass their authored command as one quoted argument,
				// either inline or as a variable holding a quoted command string.
				fn, helper := functions[command]
				helper = helper && forwardsSSH(fn.declaration)
				if command == "ssh" || strings.Contains(command, "ssh") || helper {
					for _, arg := range n.Args[1:] {
						q, ok := soleDoubleQuote(arg)
						if !ok {
							continue
						}
						sourceContent := sourceContent
						if stored, ok := e.commandStrings[soleQuotedVariable(q)]; ok && helper {
							q, sourceContent = stored.quote, stored.content
						} else if !(command == "ssh" || strings.Contains(command, "ssh")) {
							continue
						}
						start, end := int(q.Pos().Offset())+1, int(q.End().Offset())-1
						var body strings.Builder
						at := start
						for _, part := range q.Parts {
							if p, ok := part.(*syntax.ParamExp); ok {
								if value, known := sourceWord(&syntax.Word{Parts: []syntax.WordPart{p}}, scope); known {
									body.Write(sourceContent[at:int(p.Pos().Offset())])
									body.WriteString(value)
									at = int(p.End().Offset())
								}
							}
						}
						body.Write(sourceContent[at:end])
						decoded, _ := heredocShell([]byte(body.String()), 0, false)
						inner, err := parseShell(decoded, "SSH lifecycle")
						if err == nil {
							local := make(map[string]string, len(scope))
							for key, value := range scope {
								local[key] = value
							}
							local["_LMW_REMOTE"] = "1"
							failure = e.shell(inner, decoded, local, depth+1)
						}
					}
				}
			}
			return true
		})
	}
	if depth > 8 {
		return fmt.Errorf("authored lifecycle nesting exceeds supported depth")
	}
	walk(file, values, content)
	if failure != nil {
		return failure
	}
	fragments, err := generatedBashHeredocs(file, content)
	if err != nil {
		return err
	}
	for _, fragment := range fragments {
		decoded, _ := heredocShell(fragment.body, 0, fragment.quoted)
		inner, err := parseShell(decoded, "generated lifecycle")
		if err != nil {
			return err
		}
		if err := e.shell(inner, decoded, values, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (e *lifecycleEvidence) docker(args []*syntax.Word, values map[string]string) error {
	if len(args) == 0 {
		return nil
	}
	switch args[0].Lit() {
	case "run":
		name, serving := "", false
		for i := 1; i < len(args); i++ {
			if args[i].Lit() == "--name" && i+1 < len(args) {
				name, _ = sourceWord(args[i+1], values)
			}
			if args[i].Lit() == "sglang.launch_server" || args[i].Lit() == "/start.sh" {
				serving = true
			}
			if simpleVariable(args[i]) == "VLLM_ARGS_STR" && e.vllmArray {
				serving, e.vllm = true, true
			}
			if tensorfoldServe(args[i:]) {
				serving = true
			}
			// --entrypoint "$TF_BIN" "$IMAGE" serve ...
			if args[i].Lit() == "--entrypoint" && i+3 < len(args) && args[i+3].Lit() == "serve" {
				if entry, ok := sourceWord(args[i+1], values); ok && path.Base(entry) == "tensorfold-native" {
					serving = true
				}
			}
			if p := simpleExpansion(args[i]); p != nil && arrayExpansion(args[i]) {
				if tensorfoldServe(e.tensorfoldArrays[p.Param.Value]) {
					serving = true
				}
			}
			// The DeepSeek follower rank runs from a quoted command string.
			if stored, ok := e.commandStrings[simpleVariable(args[i])]; ok && len(stored.quote.Parts) > 0 {
				if lit, ok := stored.quote.Parts[0].(*syntax.Lit); ok && strings.HasPrefix(lit.Value, "tf-dsv41-lanes ") {
					serving = true
				}
			}
		}
		if name != "" && serving {
			e.launch[name] = true
			if values["_LMW_REMOTE"] == "1" {
				e.remoteLaunch[name] = true
			} else {
				e.localLaunch[name] = true
			}
		}
	case "stop", "rm":
		for _, arg := range args[1:] {
			if name, ok := sourceWord(arg, values); ok && !strings.HasPrefix(name, "-") {
				e.stop[name] = true
				if values["_LMW_REMOTE"] == "1" {
					e.remoteStop[name] = true
				} else {
					e.localStop[name] = true
				}
			}
		}
	case "compose":
		project, filename, operation := "", "", ""
		for i := 1; i < len(args); i++ {
			flag, _ := sourceWord(args[i], values)
			// Compose wrappers forward "${@:3}": only the first forwarded
			// argument identifies the operation; never join/evaluate argv.
			if offset := forwardedPosition(args[i]); offset > 0 {
				flag = values[strconv.Itoa(offset)]
			}
			if (flag == "-p" || flag == "-f") && i+1 < len(args) {
				value, _ := sourceWord(args[i+1], values)
				if flag == "-p" {
					project = value
				} else {
					filename = path.Clean(value)
				}
				i++
			} else if flag == "up" || flag == "down" {
				operation = flag
			}
		}
		if project == "" || filename == "" || operation == "" {
			return nil
		}
		if path.IsAbs(filename) || filename == ".." || strings.HasPrefix(filename, "../") {
			return layoutError(filename, fmt.Errorf("Compose dependency escapes the source directory"))
		}
		content, err := e.readSource(filename)
		if err != nil {
			return layoutError(filename, err)
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return layoutError(filename, err)
		}
		if len(doc.Content) != 1 {
			return layoutError(filename, fmt.Errorf("expected Compose document"))
		}
		services := yamlField(doc.Content[0], "services")
		if services == nil {
			return layoutError(filename, fmt.Errorf("missing Compose services"))
		}
		for i := 0; i+1 < len(services.Content); i += 2 {
			name := project + "-" + services.Content[i].Value + "-1"
			if explicit := yamlField(services.Content[i+1], "container_name"); explicit != nil {
				name = explicit.Value
			}
			if operation == "up" {
				e.launch[name] = true
				// composeConfiguration separately verifies this authored
				// service's parsed exec and configuration scalars.
				if services.Content[i].Value == "vllm-dspark" {
					e.vllm = true
				}
			} else {
				e.stop[name] = true
			}
		}
	}
	return nil
}

func forwardedPosition(word *syntax.Word) int {
	p := simpleExpansion(word)
	if p == nil || p.Param.Value != "@" || p.Index != nil || p.Exp != nil {
		return 0
	}
	if p.Slice == nil {
		return 1
	}
	offset, ok := p.Slice.Offset.(*syntax.Word)
	if !ok || p.Slice.Length != nil {
		return 0
	}
	value, err := strconv.Atoi(offset.Lit())
	if err != nil || value < 1 {
		return 0
	}
	return value
}

func verifyScriptArguments(file *syntax.File, argv []string) error {
	if len(file.Stmts) == 0 {
		return layoutError(argv[0], fmt.Errorf("authored lifecycle script is empty"))
	}
	for _, arg := range argv[1:] {
		accepted := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if item, ok := node.(*syntax.CaseItem); ok {
				for _, pattern := range item.Patterns {
					if value, literal := constantWord(pattern); literal && value == arg {
						accepted = true
					}
				}
			}
			return true
		})
		if !accepted {
			return layoutError(argv[0], fmt.Errorf("authored argument %q has no supported dispatch evidence", arg))
		}
	}
	return nil
}

type quotedCommand struct {
	quote   *syntax.DblQuoted
	content []byte
}

// soleQuotedVariable names X in "$X", the whole quoted word being one expansion.
func soleQuotedVariable(q *syntax.DblQuoted) string {
	if len(q.Parts) != 1 {
		return ""
	}
	p, ok := q.Parts[0].(*syntax.ParamExp)
	if !ok || p.Index != nil || p.Exp != nil || p.Slice != nil || p.Repl != nil || p.Length || p.Excl {
		return ""
	}
	return p.Param.Value
}

// tensorfoldServe reports a TensorFold serving entrypoint at the start of words.
func tensorfoldServe(words []*syntax.Word) bool {
	if len(words) < 2 || words[1].Lit() != "serve" {
		return false
	}
	return words[0].Lit() == "tensorfold" || words[0].Lit() == "tensorfold-native"
}

// serializesPositionalSSH recognizes a helper that quotes each of its own
// arguments with printf %q into one string and passes that string to ssh.
func serializesPositionalSSH(fn *syntax.FuncDecl) bool {
	serialized := map[string]bool{}
	syntax.Walk(fn.Body, func(node syntax.Node) bool {
		loop, ok := node.(*syntax.ForClause)
		if !ok {
			return true
		}
		iter, ok := loop.Loop.(*syntax.WordIter)
		if !ok || len(iter.Items) != 1 || forwardedPosition(iter.Items[0]) != 1 {
			return true
		}
		syntax.Walk(loop, func(node syntax.Node) bool {
			a, ok := node.(*syntax.Assign)
			if !ok || a.Name == nil || !a.Append || a.Value == nil {
				return true
			}
			syntax.Walk(a.Value, func(node syntax.Node) bool {
				call, ok := node.(*syntax.CallExpr)
				if ok && len(call.Args) == 3 && call.Args[0].Lit() == "printf" {
					format, constant := constantWord(call.Args[1])
					if constant && format == "%q" && simpleVariable(call.Args[2]) == iter.Name.Value {
						serialized[a.Name.Value] = true
					}
				}
				return true
			})
			return true
		})
		return true
	})
	sent := false
	syntax.Walk(fn.Body, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 || call.Args[0].Lit() != "ssh" {
			return true
		}
		for _, word := range call.Args[1:] {
			sent = sent || serialized[simpleVariable(word)]
		}
		return true
	})
	return sent
}
