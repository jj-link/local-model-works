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
	// The native-engine source keeps --name beside the endpoint, forwards the
	// entrypoint's arguments through USER_ARGS and runs Docker from an array.
	native, userForwards, snapshot, model := false, false, false, false
	launches := map[string]bool{}
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		if len(call.Args) > 0 && call.Args[0].Lit() == "unset" {
			for _, word := range call.Args[1:] {
				name, literal := constantWord(word)
				if !literal {
					continue
				}
				name, _, _ = strings.Cut(name, "[")
				switch name {
				case "SERVE_ARGS":
					arguments, forwards, native = false, false, false
				case "USER_ARGS":
					userForwards = false
				case "sub":
					snapshot = false
				case "MODEL_ARG":
					model = false
				}
				delete(launches, name)
			}
		}
		if len(call.Args) == 1 && arrayExpansion(call.Args[0]) && launches[simpleExpansion(call.Args[0]).Param.Value] {
			return nil
		}
		for _, assignment := range call.Assigns {
			if assignment.Name == nil {
				continue
			}
			switch assignment.Name.Value {
			case "SERVE_ARGS":
			case "USER_ARGS":
				// USER_ARGS=("$@") is the only reviewed content of that array.
				userForwards = !assignment.Append && assignment.Index == nil && assignment.Array != nil &&
					len(assignment.Array.Elems) == 1 && forwardedPosition(assignment.Array.Elems[0].Value) == 1
				continue
			case "sub":
				snapshot = !assignment.Append && assignment.Index == nil && modelSnapshotCall(assignment.Value)
				continue
			case "MODEL_ARG":
				// The served folder is the prepared snapshot under the mounted cache.
				model = snapshot && !assignment.Append && assignment.Index == nil && onlyExpands(assignment.Value, "sub")
				continue
			default:
				delete(launches, assignment.Name.Value)
				if !assignment.Append && assignment.Index == nil && assignment.Array != nil {
					words := make([]*syntax.Word, 0, len(assignment.Array.Elems))
					for _, elem := range assignment.Array.Elems {
						words = append(words, elem.Value)
					}
					if native && userForwards && model && nativeTensorFoldLaunch(words) {
						launches[assignment.Name.Value] = true
					}
				}
				continue
			}
			if assignment.Append && assignment.Index == nil {
				// SERVE_ARGS+=("$@") appends the entrypoint's own arguments last.
				if assignment.Array != nil && len(assignment.Array.Elems) == 1 && forwardedPosition(assignment.Array.Elems[0].Value) == 1 {
					forwards = arguments
				}
				continue
			}
			arguments, forwards, native = false, false, false
			if assignment.Array != nil && len(assignment.Array.Elems) >= 6 {
				words := assignment.Array.Elems
				arguments = words[0].Value.Lit() == "--name" && simpleVariable(words[1].Value) == "SERVED_NAME" &&
					words[2].Value.Lit() == "--parallel" && simpleVariable(words[3].Value) == "PARALLEL" &&
					words[4].Value.Lit() == "--context" && simpleVariable(words[5].Value) == "CONTEXT"
			}
			if assignment.Index == nil && assignment.Array != nil && len(assignment.Array.Elems) >= 4 {
				words := assignment.Array.Elems
				native = words[0].Value.Lit() == "--context" && simpleVariable(words[1].Value) == "CONTEXT" &&
					words[2].Value.Lit() == "--parallel" && simpleVariable(words[3].Value) == "PARALLEL"
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

// nativeTensorFoldLaunch recognizes the Docker command the native-engine source
// builds as an array: the named container, selected image, prepared snapshot,
// model alias, fixed endpoint, then the source-owned and forwarded options.
func nativeTensorFoldLaunch(words []*syntax.Word) bool {
	if len(words) < 16 || words[0].Lit() != "docker" || words[1].Lit() != "run" {
		return false
	}
	named := false
	for i := 2; i+1 < len(words)-12; i++ {
		if words[i].Lit() == "--name" && simpleVariable(words[i+1]) == "CONTAINER_NAME" {
			named = true
		}
	}
	args := words[len(words)-12:]
	p, u := simpleExpansion(args[10]), simpleExpansion(args[11])
	return named && simpleVariable(args[0]) == "IMAGE" && args[1].Lit() == "tensorfold-native" && args[2].Lit() == "serve" &&
		simpleVariable(args[3]) == "MODEL_ARG" && args[4].Lit() == "--name" && simpleVariable(args[5]) == "SERVED_NAME" &&
		args[6].Lit() == "--host" && simpleVariable(args[7]) == "HOST" && args[8].Lit() == "--port" && simpleVariable(args[9]) == "PORT" &&
		p != nil && p.Param.Value == "SERVE_ARGS" && arrayExpansion(args[10]) &&
		u != nil && u.Param.Value == "USER_ARGS" && arrayExpansion(args[11])
}

// modelSnapshotCall reports whether word is exactly $(model_sub), the source's
// own lookup of the prepared checkpoint folder.
func modelSnapshotCall(word *syntax.Word) bool {
	if word == nil || len(word.Parts) != 1 {
		return false
	}
	subst, ok := word.Parts[0].(*syntax.CmdSubst)
	if !ok || len(subst.Stmts) != 1 {
		return false
	}
	call, ok := subst.Stmts[0].Cmd.(*syntax.CallExpr)
	return ok && len(call.Assigns) == 0 && len(call.Args) == 1 && call.Args[0].Lit() == "model_sub"
}

// onlyExpands reports whether word expands exactly one variable, name, once and
// plainly, among literal text.
func onlyExpands(word *syntax.Word, name string) bool {
	if word == nil {
		return false
	}
	count, plain := 0, true
	syntax.Walk(word, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.ParamExp:
			count++
			if n.Param == nil || n.Param.Value != name || n.Exp != nil || n.Index != nil || n.Excl || n.Length || n.Slice != nil || n.Repl != nil {
				plain = false
			}
		case *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst, *syntax.ExtGlob, *syntax.BraceExp:
			plain = false
		}
		return true
	})
	return plain && count == 1
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
