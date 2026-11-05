package repositorycompiler

import (
	"fmt"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// The single-node source invokes Docker directly, unlike the ranked array/SSH
// procedure. Prove its actual listener and serving arguments, not the separate
// parser-check container or an unrelated reference to HOST/PORT in a URL.
// With forwarded entrypoint arguments, also prove they reach the option array.
func verifySingleTensorFoldLaunch(name string, forwarded bool, readSource func(string) ([]byte, error)) error {
	content, err := readSource(strings.TrimPrefix(name, "./"))
	if err != nil {
		return layoutError(name, err)
	}
	file, err := parseShell(content, name)
	if err != nil {
		return layoutError(name, err)
	}
	arguments, forwards := false, false
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		if len(call.Args) > 0 && call.Args[0].Lit() == "unset" {
			for _, word := range call.Args[1:] {
				name, literal := constantWord(word)
				if literal && (name == "SERVE_ARGS" || strings.HasPrefix(name, "SERVE_ARGS[")) {
					arguments, forwards = false, false
				}
			}
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil || assignment.Name.Value != "SERVE_ARGS" {
				continue
			}
			if assignment.Append && assignment.Index == nil {
				// SERVE_ARGS+=("$@") appends the entrypoint's own arguments last.
				if assignment.Array != nil && len(assignment.Array.Elems) == 1 && forwardedPosition(assignment.Array.Elems[0].Value) == 1 {
					forwards = arguments
				}
				continue
			}
			arguments, forwards = false, false
			if assignment.Array != nil && len(assignment.Array.Elems) >= 6 {
				words := assignment.Array.Elems
				arguments = words[0].Value.Lit() == "--name" && simpleVariable(words[1].Value) == "SERVED_NAME" &&
					words[2].Value.Lit() == "--parallel" && simpleVariable(words[3].Value) == "PARALLEL" &&
					words[4].Value.Lit() == "--context" && simpleVariable(words[5].Value) == "CONTEXT"
			}
		}
		if len(call.Args) < 3 || call.Args[0].Lit() != "docker" || call.Args[1].Lit() != "run" {
			continue
		}
		named := false
		for i := 2; i+1 < len(call.Args); i++ {
			if call.Args[i].Lit() == "--name" && simpleVariable(call.Args[i+1]) == "CONTAINER_NAME" {
				named = true
			}
		}
		if !named {
			continue
		}
		// This exact direct suffix couples the named container, selected image,
		// original model, fixed endpoint and the source-owned option array.
		args := call.Args
		if len(args) >= 9 {
			args = args[len(args)-9:]
			p := simpleExpansion(args[8])
			if arguments && (forwards || !forwarded) && simpleVariable(args[0]) == "IMAGE" && args[1].Lit() == "tensorfold" && args[2].Lit() == "serve" &&
				simpleVariable(args[3]) == "MODEL_ID" && args[4].Lit() == "--host" && simpleVariable(args[5]) == "HOST" &&
				args[6].Lit() == "--port" && simpleVariable(args[7]) == "PORT" &&
				p != nil && p.Param.Value == "SERVE_ARGS" && arrayExpansion(args[8]) {
				return nil
			}
		}
	}
	return layoutError(name, fmt.Errorf("authored single-node TensorFold launch no longer couples its named container, endpoint and serving settings"))
}

// Only reviewed serving options may follow the entrypoint, each with a literal
// value; anything else would reach tensorfold serve unreviewed.
func verifyTensorFoldServeArguments(name string, args []string) error {
	for i := 0; i < len(args); i += 2 {
		if args[i] != "--max-tokens" || i+1 >= len(args) {
			return layoutError(name, fmt.Errorf("serving argument %q is not a supported TensorFold option", args[i]))
		}
		if value, err := strconv.Atoi(args[i+1]); err != nil || value < 1 {
			return layoutError(name, fmt.Errorf("--max-tokens requires a positive integer, got %q", args[i+1]))
		}
	}
	return nil
}
