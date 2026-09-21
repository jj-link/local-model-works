package repositorycompiler

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/syntax"
)

func extraEnvironment(name string) bool {
	return name == "EXTRA_ARGS" || name == "EXTRA_VLLM_ARGS" || name == "EXTRA_DOCKER_ARGS" || name == "DOCKER_ENV"
}

// Additional arguments are literal JSON argv, never raw text passed through an
// upstream string parser. Bind the actual invocation and retain all inherited
// arguments (notably GLM's computed graph sizes and the single-Spark V2 runner).
func (c *configurationCollector) extraArguments(file *syntax.File, content []byte, source string) error {
	var edits []sourceconfig.Edit
	add := func(key, label, format string, offset int, docker bool) {
		if c.parameter(key) == nil {
			forbidden := protectedServerArguments(c.manifest.Metadata.Engine)
			if docker {
				forbidden = protectedDockerArguments()
			}
			parameter := recipe.Parameter{
				Name: key, Label: label, Group: "Additional arguments", Type: "string", Format: "argv", Optional: true,
				ForbiddenArgs: forbidden,
				Description:   source + ": appended to the original invocation after its inherited arguments. Enter a JSON array of strings; each element is one literal argument, including JSON or whitespace. Unset and [] preserve upstream defaults. Application-managed topology, endpoints and identities cannot be overridden.",
			}
			if docker {
				parameter.ArgvPolicy = "docker"
				parameter.ForbiddenEnv = protectedDockerEnvironment(c.workload)
				parameter.Description += ` Docker syntax requires one --long-option=value per element, for example ["--env=VLLM_USE_V2_MODEL_RUNNER=1","--memory=100g"]. Boolean options use =true or =false. No short flags, bare options, positional image/command, or --env-file.`
			}
			c.manifest.Parameters = append(c.manifest.Parameters, parameter)
		}
		edits = append(edits, sourceconfig.Edit{Start: offset, End: offset, Parameter: key, Format: format})
	}
	if c.manifest.Metadata.Engine == "sglang" {
		for _, stmt := range file.Stmts {
			call, ok := stmt.Cmd.(*syntax.CallExpr)
			if !ok || len(call.Args) < 2 || call.Args[0].Lit() != "docker" || call.Args[1].Lit() != "run" {
				continue
			}
			for _, word := range call.Args {
				p := simpleExpansion(word)
				if p == nil || !arrayExpansion(word) {
					continue
				}
				switch p.Param.Value {
				case "EXTRA_ARGS_ARR":
					add("extra_args", "Additional SGLang arguments", "argv", int(word.End().Offset()), false)
				case "DOCKER_ENV_ARGS":
					add("docker_env", "Additional Docker arguments", "argv", int(word.End().Offset()), true)
				}
			}
		}
	} else {
		fragments, err := generatedBashHeredocs(file, content)
		if err != nil {
			return err
		}
		for _, fragment := range fragments {
			body, offset, quoted := fragment.body, fragment.offset, fragment.quoted
			decoded, positions := heredocShell(body, offset, quoted)
			inner, err := parseShell(decoded, source+" (Bash heredoc)")
			if err != nil {
				return fmt.Errorf("unsupported generated Bash launcher: %w", err)
			}
			for _, stmt := range inner.Stmts {
				call, ok := stmt.Cmd.(*syntax.CallExpr)
				if !ok || len(call.Args) < 3 {
					continue
				}
				if quoted && call.Args[0].Lit() == "exec" && call.Args[1].Lit() == "vllm" && call.Args[2].Lit() == "serve" {
					last := call.Args[len(call.Args)-1]
					add("extra_args", "Additional vLLM arguments", "argv-quoted-heredoc", positions[last.End().Offset()], false)
				}
				if !quoted && call.Args[0].Lit() == "docker" && call.Args[1].Lit() == "run" {
					for _, word := range call.Args {
						switch simpleVariable(word) {
						case "VLLM_ARGS_STR":
							add("extra_vllm_args", "Additional vLLM arguments", "argv-heredoc", positions[word.End().Offset()], false)
						case "EXTRA_DOCKER_ARGS":
							add("extra_docker_args", "Additional Docker arguments", "argv-heredoc", positions[word.End().Offset()], true)
						}
					}
				}
			}
		}
	}
	if len(edits) == 0 {
		return nil
	}
	upstream := c.workload.Upstream
	if len(upstream.Configuration) == 0 {
		digest := sha256.Sum256(content)
		upstream.Configuration = []sourceconfig.File{{Path: source, SHA256: hex.EncodeToString(digest[:])}}
	}
	configuration := &upstream.Configuration[0]
	configuration.Edits = append(configuration.Edits, edits...)
	return nil
}

func protectedDockerEnvironment(workload *recipe.Workload) []string {
	keys := strings.Fields("CUDA_VISIBLE_DEVICES NVIDIA_VISIBLE_DEVICES HF_HOME VLLM_HOST_IP NODE_RANK RANK LOCAL_RANK WORLD_SIZE MASTER_ADDR MASTER_PORT")
	if workload.Upstream.EnvFormat == "literal" {
		keys = append(keys, "TRITON_CACHE_DIR")
	} else {
		keys = append(keys, "VLLM_PLE_PACKED_TABLE_DIR", "VLLM_MTP_DRAFT_VOCAB")
	}
	for key, binding := range workload.Env {
		if strings.Contains(binding, "${cluster.") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

type bashHeredocFragment struct {
	redirect *syntax.Redirect
	body     []byte
	offset   int
	quoted   bool
}

// Generated Bash programs may be emitted in multiple cat heredocs, with other
// generated content between them. Only a shebang or an append to a known Bash
// target identifies shell content; unrelated heredocs must not be parsed as Bash.
func generatedBashHeredocs(file *syntax.File, content []byte) ([]bashHeredocFragment, error) {
	var fragments []bashHeredocFragment
	targets := make(map[string]bool)
	var failure error
	syntax.Walk(file, func(node syntax.Node) bool {
		if failure != nil {
			return false
		}
		stmt, ok := node.(*syntax.Stmt)
		if !ok {
			return true
		}
		var target string
		var appendTarget bool
		if call, ok := stmt.Cmd.(*syntax.CallExpr); ok && len(call.Args) > 0 && call.Args[0].Lit() == "cat" {
			for _, redirect := range stmt.Redirs {
				if (redirect.Op == syntax.RdrOut || redirect.Op == syntax.AppOut) && (redirect.N == nil || redirect.N.Value == "1") {
					target = string(content[redirect.Word.Pos().Offset():redirect.Word.End().Offset()])
					appendTarget = redirect.Op == syntax.AppOut
				}
			}
		}
		for _, redirect := range stmt.Redirs {
			if redirect.Hdoc == nil || redirect.Op != syntax.Hdoc {
				continue
			}
			body, offset, quoted, err := bashHeredoc(redirect, content)
			if err != nil {
				failure = err
				return false
			}
			shebang := bytes.HasPrefix(body, []byte("#!/bin/bash\n"))
			if shebang || (appendTarget && targets[target]) {
				fragments = append(fragments, bashHeredocFragment{redirect, body, offset, quoted})
			}
			if target != "" && (shebang || !appendTarget) {
				targets[target] = shebang
			}
		}
		return true
	})
	return fragments, failure
}

// bashHeredoc uses the parsed redirect and delimiter to recover original bytes.
// Only ordinary (not tab-stripping) Bash heredocs are supported. The parser has
// already verified the delimiter; locating that exact line preserves byte maps.
func bashHeredoc(redirect *syntax.Redirect, content []byte) ([]byte, int, bool, error) {
	delimiter, constant := constantWord(redirect.Word)
	if !constant {
		return nil, 0, false, fmt.Errorf("nonliteral heredoc delimiter")
	}
	start := int(redirect.Hdoc.Pos().Offset())
	quoted := false
	for _, part := range redirect.Word.Parts {
		switch part.(type) {
		case *syntax.SglQuoted, *syntax.DblQuoted:
			quoted = true
		}
	}
	for at := start; at <= len(content); {
		end := bytes.IndexByte(content[at:], '\n')
		if end < 0 {
			end = len(content)
		} else {
			end += at
		}
		if string(content[at:end]) == delimiter {
			return content[start:at], start, quoted, nil
		}
		if end == len(content) {
			break
		}
		at = end + 1
	}
	return nil, 0, false, fmt.Errorf("cannot map parsed heredoc delimiter %q to source bytes", delimiter)
}

// Decode exactly the backslash processing performed by one unquoted heredoc,
// without expanding parameters or running substitutions. Retaining placeholders
// lets the nested Bash parser identify direct argv boundaries. Every decoded
// byte boundary maps back to the original hash-pinned source for safe edits.
func heredocShell(content []byte, base int, quoted bool) ([]byte, []int) {
	decoded := make([]byte, 0, len(content))
	positions := make([]int, 0, len(content)+1)
	for i := 0; i < len(content); i++ {
		at := i
		value := content[i]
		if !quoted && value == '\\' && i+1 < len(content) {
			switch content[i+1] {
			case '\n':
				i++
				continue
			case '\\', '$', '`':
				i++
				value = content[i]
			}
		}
		positions = append(positions, base+at)
		decoded = append(decoded, value)
	}
	positions = append(positions, base+len(content))
	return decoded, positions
}

func protectedServerArguments(engine string) []string {
	var flags []string
	if engine == "sglang" {
		flags = strings.Fields("--host --port --served-model-name --model-path --model --tp --tp-size --tensor-parallel-size --nnodes --node-rank --dist-init-addr --dist-init-address --base-gpu-id --gpu-id-step --dp --dp-size --ep --ep-size --distributed-executor-backend --config")
	} else {
		flags = strings.Fields("--host --port --served-model-name --model --tensor-parallel-size -tp --pipeline-parallel-size -pp --data-parallel-size -dp --data-parallel-rank --data-parallel-start-rank --data-parallel-size-local --data-parallel-address --data-parallel-rpc-port --nnodes --node-rank --master-addr --master-port --headless --distributed-executor-backend --config")
	}
	return flags
}

func protectedDockerArguments() []string {
	return strings.Fields("--name --hostname -h --network --net --ipc --pid --uts --userns --gpus --device --entrypoint --publish -p --publish-all -P --env-file --label-file --cidfile --rm --restart --detach -d --volume -v --mount --volumes-from --workdir -w")
}
