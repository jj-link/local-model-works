package repositorycompiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// compileConfiguration reads only verified, pinned originals. It never executes
// source and never replaces an upstream installer, overlay or serving stack.
func compileConfiguration(manifest *recipe.Manifest, readSource func(string) ([]byte, error)) error {
	for wi := range manifest.Workloads {
		workload := &manifest.Workloads[wi]
		upstream := workload.Upstream
		name := strings.TrimPrefix(upstream.Start[0], "./")
		content, err := readSource(name)
		if err != nil {
			return layoutError(name, err)
		}
		file, err := parseShell(content, name)
		if err != nil {
			return layoutError(name, err)
		}
		if workload.Env == nil {
			workload.Env = make(map[string]string)
		}
		collector := configurationCollector{manifest: manifest, workload: workload}
		if upstream.EnvTemplate != "" {
			template, err := readSource(upstream.EnvTemplate)
			if err != nil {
				return layoutError(upstream.EnvTemplate, err)
			}
			if err := collector.environment(template, upstream.EnvTemplate, upstream.EnvFormat); err != nil {
				return layoutError(upstream.EnvTemplate, err)
			}
			collector.documentedInputs(file, template, upstream.EnvTemplate)
		}
		// A self-default assignment is an actual environment input, unlike an
		// unconditional constant. Keep these optional: the source can compute
		// a default from the selected model, host, or another setting at startup.
		collector.scriptInputs(file, name)
		if manifest.Metadata.Engine == "sglang" {
			configuration, err := collector.sglang(file, content, name)
			if err != nil {
				return layoutError(name, err)
			}
			upstream.Configuration = []sourceconfig.File{configuration}
			collector.hardcodedCaches(file, name)
			if err := collector.hardcodedPort(file); err != nil {
				return layoutError(name, err)
			}
		}
		if err := collector.runtimeConfiguration(file, content, name); err != nil {
			return layoutError(name, err)
		}
		if strings.HasPrefix(name, "start-deepseek-") {
			const dependency = "docker-compose.dspark.yml"
			compose, err := readSource(dependency)
			if err != nil {
				return layoutError(dependency, err)
			}
			if err := collector.composeConfiguration(compose, dependency); err != nil {
				return layoutError(dependency, err)
			}
		}
		if err := collector.extraArguments(file, content, name); err != nil {
			return layoutError(name, err)
		}
		kept := upstream.Configuration[:0]
		for _, configuration := range upstream.Configuration {
			if len(configuration.Edits) != 0 {
				kept = append(kept, configuration)
			}
		}
		upstream.Configuration = kept
		for i := range upstream.Configuration {
			edits := upstream.Configuration[i].Edits
			sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
		}
	}
	return nil
}

func layoutError(path string, err error) error {
	return &recipe.PackError{Code: "recipe.repository_layout_changed", Asset: path, Message: err.Error()}
}

func parseShell(content []byte, name string) (*syntax.File, error) {
	return syntax.NewParser(syntax.Variant(syntax.LangBash), syntax.KeepComments(true)).Parse(bytes.NewReader(content), name)
}

type configurationCollector struct {
	manifest *recipe.Manifest
	workload *recipe.Workload
}

// These values participate in LMW's coupled rank, endpoint, fabric, observation
// and teardown contracts. They are not independent serving options. Existing
// setting bindings (SSH accounts and cache paths) are retained before this check.
func managedEnvironment(name string) bool {
	switch name {
	case "PORT", "VLLM_PORT", "VLLM_HOST", "HOST", "MASTER_PORT", "MASTER_ADDR",
		"HEAD_IP", "WORKER_IP", "WORKER_USER", "WORKER_SSH", "WORKER_HOME", "WORKER_HOST",
		"NODE_RANK", "HEADLESS", "TP", "NNODES", "TENSOR_PARALLEL_SIZE", "DSPARK_TP3",
		"TP3_MAX_NUM_SEQS", "ENABLE_EXPERT_PARALLEL", "VLLM_HOST_IP", "WORKER_VLLM_HOST_IP",
		"IFACE", "WORKER_IFACE", "IB_HCA", "WORKER_IB_HCA", "IB_GID_INDEX",
		"NCCL_IB_HCA", "NCCL_SOCKET_IFNAME", "TP_SOCKET_IFNAME", "GLOO_SOCKET_IFNAME",
		"NCCL_IB_GID_INDEX", "NCCL_IB_GID_AUTO", "NCCL_IB_GID_MATCH_IP",
		"WORKER_NCCL_IB_HCA", "WORKER_NCCL_SOCKET_IFNAME", "WORKER_TP_SOCKET_IFNAME", "WORKER_GLOO_SOCKET_IFNAME",
		"WORKER_NCCL_IB_GID_INDEX", "WORKER_NCCL_IB_GID_MATCH_IP", "NFS_SERVER_IP",
		"HEAD_CX7_IF", "WORKER_CX7_IF", "HEAD_CX7_IB", "WORKER_CX7_IB", "HEAD_GID", "WORKER_GID",
		"CONTAINER_NAME", "CONTAINER_HEAD", "CONTAINER_WORKER", "TP1_CONTAINER_NAME", "PROJECT_NAME",
		"ENV_FILE", "COMPOSE_FILE", "NFS_VOLUME", "NFS_CONTAINER", "NFS_OVERRIDE_FILE",
		"API_HOST", "API_URL", "CHAT_URL", "SERVED_MODEL_NAME", "WORKER_DIR", "DSPARK_MODEL",
		"NATIVE_MAX_MODEL_LEN", "YARN_CEILING_MODEL_LEN", "EXPECTED_SHARDS", "DSPARK_PROPOSER_FILE":
		return true
	case "HOME", "PATH", "LD_LIBRARY_PATH", "CUDA_HOME", "CUDA_PATH", "CUDAToolkit_ROOT",
		"TP_SIZE", "TP3_PATCH_DIR", "DSPARK_PATCHES_DIR", "COMPOSE_ENV_FILE", "WORKER_SCRIPT_DIR":
		return true
	}
	return strings.HasPrefix(name, "WORKER2_") || strings.HasPrefix(name, "TP3_") || strings.HasPrefix(name, "_") ||
		strings.Contains(name, "PATCH_HOST") || strings.HasSuffix(name, "_HOTFIX") && !strings.HasPrefix(name, "DSPARK_ENABLE_") && !strings.HasPrefix(name, "DSPARK_SKIP_") ||
		name == "VLLM_GB10_PATCH_DIR" || name == "VIDEO_PATCH_HOST"
}

func (c *configurationCollector) parameter(name string) *recipe.Parameter {
	for i := range c.manifest.Parameters {
		if c.manifest.Parameters[i].Name == name {
			return &c.manifest.Parameters[i]
		}
	}
	return nil
}

func (c *configurationCollector) bindEnvironment(name, value, description, source string, optional, literal bool) {
	if name == "GPU_MEMORY_UTILIZATION" && strings.HasPrefix(c.workload.Upstream.Start[0], "./start-deepseek-") {
		return // Always overwritten by the live GPU_MEMORY_UTILIZATION_TEXT alias.
	}
	if extraEnvironment(name) {
		return // JSON argv is bound at the actual invocation, never through raw env text.
	}
	if binding, exists := c.workload.Env[name]; exists {
		if strings.HasPrefix(binding, "${setting.") && strings.HasSuffix(binding, "}") {
			key := strings.TrimSuffix(strings.TrimPrefix(binding, "${setting."), "}")
			if p := c.parameter(key); p != nil {
				if p.Label == "" {
					p.Label = humanLabel(name)
				}
				if p.Group == "" {
					p.Group = parameterGroup(name)
				}
				if p.Description == "" {
					p.Description = source + ": " + name + ". " + description
				}
			}
		}
		return
	}
	if managedEnvironment(name) {
		return
	}
	key := strings.ToLower(name)
	if c.parameter(key) != nil {
		return
	}
	p := inferredParameter(key, value)
	p.Label, p.Group = humanLabel(name), parameterGroup(name)
	p.Optional = optional
	p.Description = source + ": " + name + ". " + description
	if optional {
		p.Default = nil
		p.Description += " Unset preserves the upstream default (including computed values)."
	}
	if literal {
		p.Description += " Literal KEY=value input; quotes are data, not shell syntax."
	}
	p.Description = boundedDescription(p.Description)
	c.manifest.Parameters = append(c.manifest.Parameters, p)
	c.workload.Env[name] = "${setting." + key + "}"
}

func (c *configurationCollector) environment(content []byte, source, format string) error {
	lines := strings.Split(string(content), "\n")
	var comments []string
	active := make(map[string]bool)
	// Active assignments beat commented alternative examples, regardless of order.
	for _, line := range lines {
		name, _, ok := assignmentText(strings.TrimSpace(line))
		if ok {
			active[name] = true
		}
	}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// This documented section explicitly requires a different Compose stack.
		// Exposing it here would produce fields that do not reach this lifecycle.
		if strings.HasPrefix(line, "# Stage-C overlay only") {
			break
		}
		optional := strings.HasPrefix(line, "#")
		candidate := line
		if optional {
			candidate = strings.TrimSpace(strings.TrimPrefix(line, "#"))
		}
		name, raw, ok := assignmentText(candidate)
		if !ok {
			if optional {
				comments = append(comments, strings.TrimSpace(strings.TrimPrefix(line, "#")))
			} else if line == "" {
				comments = nil
			}
			continue
		}
		if optional && active[name] {
			continue
		}
		value, known := raw, true
		description := conciseComments(comments)
		if format != "literal" {
			parsed, err := parseShell([]byte(candidate), source)
			if err != nil {
				if optional { // prose beginning NAME=value is not a declaration
					continue
				}
				return err
			}
			if len(parsed.Stmts) != 1 {
				return fmt.Errorf("expected a single environment assignment for %s", name)
			}
			call, ok := parsed.Stmts[0].Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) != 0 || len(call.Assigns) != 1 || call.Assigns[0].Array != nil {
				if optional {
					continue
				}
				return fmt.Errorf("unsupported environment assignment for %s", name)
			}
			value, known = constantWord(call.Assigns[0].Value)
			if inline := conciseSyntaxComments(parsed.Stmts[0].Comments); inline != "" {
				description = strings.TrimSpace(description + " " + inline)
			}
		}
		if !known {
			value, optional = "", true
		}
		// Credentials shown in examples are never real deployment defaults.
		if sensitiveEnvironment(name) {
			value, optional = "", true
		}
		c.bindEnvironment(name, value, description, source, optional, format == "literal")
		comments = nil
	}
	return nil
}

func assignmentText(line string) (string, string, bool) {
	line = strings.TrimPrefix(line, "export ")
	name, value, ok := strings.Cut(line, "=")
	if !ok || name == "" {
		return "", "", false
	}
	for i, r := range name {
		if !(r >= 'A' && r <= 'Z' || r == '_' || i > 0 && r >= '0' && r <= '9') {
			return "", "", false
		}
	}
	return name, value, true
}

var documentedAssignment = regexp.MustCompile(`\b([A-Z][A-Z0-9_]*)=`)

// Commented usage examples can document inputs consumed directly inside a
// function rather than declared as assignments (for example BUILD/SKIP_PULL).
// Require an actual parsed default expansion; prose alone never creates a field.
func (c *configurationCollector) documentedInputs(file *syntax.File, content []byte, source string) {
	documented := make(map[string]string)
	for _, line := range strings.Split(string(content), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "# Stage-C overlay only") {
			break
		}
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, match := range documentedAssignment.FindAllStringSubmatch(line, -1) {
			documented[match[1]] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		}
	}
	syntax.Walk(file, func(node syntax.Node) bool {
		p, ok := node.(*syntax.ParamExp)
		if !ok || p.Exp == nil || p.Exp.Op != syntax.DefaultUnset && p.Exp.Op != syntax.DefaultUnsetOrNull {
			return true
		}
		if description, exists := documented[p.Param.Value]; exists {
			value, _ := constantWord(p.Exp.Word)
			c.bindEnvironment(p.Param.Value, value, description, source, true, c.workload.Upstream.EnvFormat == "literal")
		}
		return true
	})
}

// These launchers assign host cache roots unconditionally, so exporting an env
// override would be a no-op. Bind their parsed RHS while preserving every use,
// mount, download and cache-sharing operation from the original procedure.
func (c *configurationCollector) hardcodedCaches(file *syntax.File, source string) {
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil || assignment.Value == nil || assignment.Array != nil {
				continue
			}
			name := assignment.Name.Value
			if name != "HF_HOME" && name != "TRITON_CACHE_DIR" {
				continue
			}
			key := strings.ToLower(name)
			if c.parameter(key) != nil {
				continue
			}
			minimum := 1
			c.manifest.Parameters = append(c.manifest.Parameters, recipe.Parameter{
				Name: key, Type: "string", Optional: true, MinLength: &minimum,
				Label: humanLabel(name), Group: "Memory and caches",
				Description: source + ": host " + name + " cache root. An explicit absolute path can reuse already-downloaded weights or compiled kernels. Unset retains the source's computed workspace-relative path. The original variable, bind mounts and downloads are unchanged.",
			})
			configuration := &c.workload.Upstream.Configuration[0]
			configuration.Edits = append(configuration.Edits, sourceconfig.Edit{
				Start: int(assignment.Value.Pos().Offset()), End: int(assignment.Value.End().Offset()),
				Parameter: key, Format: "shell",
			})
		}
	}
}

// Bind the source variable rather than just --port: its readiness URL and
// operator-facing URLs must follow the same endpoint as the actual listener.
func (c *configurationCollector) hardcodedPort(file *syntax.File) error {
	if len(c.workload.Ports) != 1 {
		return fmt.Errorf("direct SGLang launch requires one reviewed API port")
	}
	port := strconv.Itoa(c.workload.Ports[0].Container)
	found := false
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil || assignment.Name.Value != "PORT" {
				continue
			}
			value, literal := constantWord(assignment.Value)
			if found || assignment.Value == nil || assignment.Array != nil || assignment.Append || !literal {
				return fmt.Errorf("expected one literal PORT assignment")
			}
			found = true
			if value != port {
				configuration := &c.workload.Upstream.Configuration[0]
				configuration.Edits = append(configuration.Edits, sourceconfig.Edit{
					Start: int(assignment.Value.Pos().Offset()), End: int(assignment.Value.End().Offset()),
					Template: port, Format: "shell",
				})
			}
		}
	}
	if !found {
		return fmt.Errorf("direct SGLang launch has no literal PORT assignment")
	}
	return nil
}

func (c *configurationCollector) scriptInputs(file *syntax.File, source string) {
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) != 0 {
			continue
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil || assignment.Array != nil || assignment.Append {
				continue
			}
			name := assignment.Name.Value
			if _, _, ok := assignmentText(name + "="); !ok {
				continue
			}
			fallback := selfDefault(assignment.Value, name)
			if fallback == nil {
				continue
			}
			value, _ := constantWord(fallback.Word)
			c.bindEnvironment(name, value, conciseSyntaxComments(stmt.Comments), source, true, c.workload.Upstream.EnvFormat == "literal")
		}
	}
	// HF_TOKEN is consumed directly instead of through a default assignment.
	if c.manifest.Metadata.Engine == "sglang" {
		found := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if p, ok := node.(*syntax.ParamExp); ok && p.Param.Value == "HF_TOKEN" && p.Exp != nil {
				found = true
			}
			return true
		})
		if found {
			c.bindEnvironment("HF_TOKEN", "", "Optional upstream Hugging Face credential.", source, true, c.workload.Upstream.EnvFormat == "literal")
		}
	}
	c.externalInputs(file, source)
}

func selfDefault(word *syntax.Word, name string) *syntax.Expansion {
	if word == nil || len(word.Parts) != 1 {
		return nil
	}
	part := word.Parts[0]
	if quoted, ok := part.(*syntax.DblQuoted); ok {
		if len(quoted.Parts) != 1 {
			return nil
		}
		part = quoted.Parts[0]
	}
	p, ok := part.(*syntax.ParamExp)
	if !ok || p.Exp == nil || p.Exp.Op != syntax.DefaultUnset && p.Exp.Op != syntax.DefaultUnsetOrNull {
		return nil
	}
	if p.Param.Value == name {
		return p.Exp
	}
	// The single-Spark launcher snapshots CLI overrides before sourcing .env.
	if strings.HasPrefix(p.Param.Value, "_CLI_") {
		return selfDefault(p.Exp.Word, name)
	}
	return nil
}

// constantWord decodes quoting/escapes only after proving there are no variable,
// command, arithmetic, process, brace or tilde expansions. No host state is read.
func constantWord(word *syntax.Word) (string, bool) {
	if word == nil {
		return "", true
	}
	constant := true
	syntax.Walk(word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst, *syntax.ExtGlob, *syntax.BraceExp:
			constant = false
			return false
		case *syntax.Lit:
			if strings.HasPrefix(n.Value, "~") {
				constant = false
			}
		}
		return true
	})
	if !constant {
		return "", false
	}
	value, err := expand.Literal(nil, word)
	return value, err == nil
}

func simpleExpansion(word *syntax.Word) *syntax.ParamExp {
	if word == nil || len(word.Parts) != 1 {
		return nil
	}
	part := word.Parts[0]
	if quoted, ok := part.(*syntax.DblQuoted); ok {
		if len(quoted.Parts) != 1 {
			return nil
		}
		part = quoted.Parts[0]
	}
	p, _ := part.(*syntax.ParamExp)
	return p
}

func simpleVariable(word *syntax.Word) string {
	p := simpleExpansion(word)
	if p != nil && p.Exp == nil && p.Index == nil && !p.Excl && !p.Length && p.Slice == nil && p.Repl == nil {
		return p.Param.Value
	}
	return ""
}

func arrayExpansion(word *syntax.Word) bool {
	p := simpleExpansion(word)
	if p == nil {
		return false
	}
	index, ok := p.Index.(*syntax.Word)
	return ok && (index.Lit() == "@" || index.Lit() == "*")
}

func managedFlag(flag string) bool {
	switch flag {
	case "--host", "--port", "--served-model-name", "--tp", "--tp-size", "--tensor-parallel-size", "--nnodes", "--node-rank", "--dist-init-addr", "--base-gpu-id", "--gpu-id-step", "--dp", "--dp-size", "--ep", "--ep-size":
		return true
	}
	return false
}

func (c *configurationCollector) sglang(file *syntax.File, content []byte, source string) (sourceconfig.File, error) {
	digest := sha256.Sum256(content)
	configuration := sourceconfig.File{Path: source, SHA256: hex.EncodeToString(digest[:])}
	constants := make(map[string]string)
	var launch []*syntax.Word
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		if len(call.Args) == 0 {
			if launch != nil {
				continue
			}
			for _, a := range call.Assigns {
				if a.Name != nil && a.Array == nil {
					if value, ok := constantWord(a.Value); ok {
						constants[a.Name.Value] = value
					} else {
						delete(constants, a.Name.Value)
					}
				}
			}
			continue
		}
		first := call.Args[0].Lit()
		if first != "docker" && first != "python3" && first != "python" {
			continue
		}
		if first == "docker" && (len(call.Args) < 2 || call.Args[1].Lit() != "run") {
			continue
		}
		for i := 2; i < len(call.Args); i++ {
			if call.Args[i].Lit() == "sglang.launch_server" && call.Args[i-1].Lit() == "-m" && (call.Args[i-2].Lit() == "python3" || call.Args[i-2].Lit() == "python") {
				if launch != nil {
					return configuration, fmt.Errorf("multiple direct SGLang launch commands require source review")
				}
				if first == "docker" {
					if i < 5 || arrayExpansion(call.Args[i-3]) || strings.HasPrefix(call.Args[i-3].Lit(), "-") {
						return configuration, fmt.Errorf("direct Docker SGLang launch requires an image immediately before Python")
					}
					// The reviewed direct command places its image just before Python.
					// Append the placement binding after every source Docker option,
					// including environment arrays, so none can override its UUIDs.
					offset := int(call.Args[i-3].Pos().Offset())
					configuration.Edits = append(configuration.Edits, sourceconfig.Edit{
						Start: offset, End: offset, Format: "argv",
						Template: `["--env=CUDA_VISIBLE_DEVICES=${node.accelerators}"]`,
					})
				}
				launch = call.Args[i+1:]
			}
		}
	}
	if launch == nil {
		return configuration, fmt.Errorf("expected one direct SGLang argv; nested commands, heredocs and generated launchers are not adapted")
	}
	seen := make(map[string]bool)
	sourceLines := strings.Split(string(content), "\n")
	for i := 0; i < len(launch); {
		word := launch[i]
		flag := word.Lit()
		if !strings.HasPrefix(flag, "--") {
			i++ // dynamic array: its upstream environment controls remain authoritative
			continue
		}
		if strings.Contains(flag, "=") {
			return configuration, fmt.Errorf("equals-form SGLang flag %s requires source review", flag)
		}
		firstValue := i + 1
		j := firstValue
		for j < len(launch) && !strings.HasPrefix(launch[j].Lit(), "--") {
			// Array expansions delimit flag groups but are not single values.
			if arrayExpansion(launch[j]) {
				break
			}
			j++
		}
		i = j
		if j == firstValue && j < len(launch) && arrayExpansion(launch[j]) {
			continue // ambiguous dynamic values are never converted to boolean flags
		}
		if managedFlag(flag) {
			continue
		}
		values := make([]string, 0, j-firstValue)
		bound := false
		start := int(word.Pos().Offset())
		end := int(word.End().Offset())
		// Positions, not textual searches, select the original argument words.
		for k := firstValue; k < j; k++ {
			arg := launch[k]
			value, known := constantWord(arg)
			if !known {
				variable := simpleVariable(arg)
				if _, exists := c.workload.Env[variable]; exists {
					bound = true
					break
				}
				value, known = constants[variable]
			}
			if !known {
				bound = true // derived model/context/cache arguments stay coupled
				break
			}
			values = append(values, value)
			end = int(arg.End().Offset())
		}
		if bound {
			continue
		}
		key := strings.ReplaceAll(strings.TrimPrefix(flag, "--"), "-", "_")
		if seen[key] || c.parameter(key) != nil {
			return configuration, fmt.Errorf("ambiguous SGLang configuration flag %s", flag)
		}
		seen[key] = true
		p := recipe.Parameter{Name: key, Label: humanLabel(key), Group: "SGLang runtime", Description: source + ": " + flag + ". Applied to the original direct launch argv; upstream engine validation still applies."}
		if notes := flagNotes(sourceLines, flag); notes != "" {
			p.Description += " " + notes
		}
		edit := sourceconfig.Edit{Start: start, End: end, Parameter: key}
		switch len(values) {
		case 0:
			p.Type, p.Default = "bool", true
			edit.Format, edit.Flag = "flag", flag
		case 1:
			inferred := inferredParameter(key, values[0])
			p.Type, p.Default, p.Sensitive = inferred.Type, inferred.Default, inferred.Sensitive
			edit.Start = int(launch[firstValue].Pos().Offset())
			edit.Format = "shell"
		default:
			encoded, _ := json.Marshal(values)
			p.Type, p.Format, p.Default = "string", "argv", string(encoded)
			p.Description += " Enter a JSON array of strings; each element becomes one safely quoted argument."
			edit.Start = int(launch[firstValue].Pos().Offset())
			edit.Format = "argv"
		}
		c.manifest.Parameters = append(c.manifest.Parameters, p)
		configuration.Edits = append(configuration.Edits, edit)
	}
	return configuration, nil
}

func inferredParameter(name, value string) recipe.Parameter {
	p := recipe.Parameter{Name: name, Type: "string", Default: value, Sensitive: sensitiveEnvironment(name)}
	if p.Sensitive {
		return p
	}
	// These authored budgets are real-valued GiB arithmetic even when a sample
	// happens to spell a default without a decimal point.
	if strings.HasSuffix(name, "_gib") {
		p.Type = "float"
		if number, err := strconv.ParseFloat(value, 64); err == nil {
			p.Default = number
		}
		return p
	}
	if value == "true" || value == "false" {
		p.Type, p.Default = "bool", value == "true"
	} else if number, err := strconv.ParseInt(value, 10, 64); err == nil && strconv.FormatInt(number, 10) == value {
		p.Type, p.Default = "int", number
	} else if number, err := strconv.ParseFloat(value, 64); err == nil && strings.Contains(value, ".") {
		p.Type, p.Default = "float", number
	}
	return p
}

// Read documentation as documentation, never as an executable argv candidate.
func flagNotes(lines []string, flag string) string {
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, flag) {
			continue
		}
		var notes []string
		for j := i; j < len(lines) && j < i+4; j++ {
			comment := strings.TrimSpace(lines[j])
			if !strings.HasPrefix(comment, "#") {
				break
			}
			notes = append(notes, strings.TrimSpace(strings.TrimPrefix(comment, "#")))
		}
		return conciseComments(notes)
	}
	return ""
}

func sensitiveEnvironment(name string) bool {
	name = strings.ToUpper(name)
	return name == "HF_TOKEN" || name == "HUGGING_FACE_HUB_TOKEN" || name == "GHCR_TOKEN" || strings.Contains(name, "API_KEY") || strings.Contains(name, "PASSWORD") || strings.Contains(name, "SECRET")
}

func humanLabel(name string) string {
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(name, "_", " "), "-", " "))
	for i, word := range words {
		word = strings.ToLower(word)
		switch word {
		case "api", "hf", "kv", "gpu", "cpu", "mtp", "ssm", "nccl", "jit", "ple", "qsa", "ib":
			words[i] = strings.ToUpper(word)
		default:
			runes := []rune(word)
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

func parameterGroup(name string) string {
	name = strings.ToUpper(name)
	switch {
	case sensitiveEnvironment(name):
		return "Authentication"
	case strings.Contains(name, "CACHE") || strings.Contains(name, "MEM") || strings.Contains(name, "GIB"):
		return "Memory and caches"
	case strings.Contains(name, "MODEL") || strings.Contains(name, "QUANT") || strings.HasPrefix(name, "HF_") || strings.HasPrefix(name, "ABLIT"):
		return "Model"
	case strings.Contains(name, "SPEC") || strings.Contains(name, "DFLASH") || strings.Contains(name, "MTP"):
		return "Speculative decoding"
	case strings.Contains(name, "IMAGE") || strings.HasPrefix(name, "GHCR_"):
		return "Upstream dependencies"
	case strings.Contains(name, "NCCL") || strings.Contains(name, "TORCH_FR"):
		return "Runtime diagnostics and transport"
	default:
		return "Runtime"
	}
}

func conciseSyntaxComments(comments []syntax.Comment) string {
	lines := make([]string, 0, len(comments))
	for _, comment := range comments {
		lines = append(lines, strings.TrimSpace(comment.Text))
	}
	return conciseComments(lines)
}

func conciseComments(comments []string) string {
	text := strings.Join(strings.Fields(strings.Join(comments, " ")), " ")
	runes := []rune(text)
	if len(runes) > 600 {
		text = string(runes[:600]) + " (see pinned source for the complete notes)"
	}
	return text
}

func boundedDescription(text string) string {
	runes := []rune(text)
	if len(runes) <= 1000 {
		return text
	}
	return string(runes[:954]) + " (see pinned source for the complete notes)"
}
