package repositorycompiler

import (
	"bytes"
	"fmt"
	"path"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

type lifecycleFunction struct {
	declaration *syntax.FuncDecl
	content     []byte
}

// Only literal source paths enter the retained dependency graph. Private local
// settings and runtime env files are inputs, not repository dependencies.
// dir is the repository-relative directory of the sourcing file, which a
// "$(dirname -- "${BASH_SOURCE[0]}")/name" operand is relative to.
func literalSource(call *syntax.CallExpr, dir string) (string, bool) {
	if len(call.Args) != 2 || call.Args[0].Lit() != "source" && call.Args[0].Lit() != "." {
		return "", false
	}
	name, ok := constantWord(call.Args[1])
	if !ok {
		var relative string
		if relative, ok = sourceDirectoryWord(call.Args[1]); ok {
			name = "./" + path.Join(dir, relative)
		}
	}
	if !ok || !strings.HasPrefix(name, "./") {
		return "", false
	}
	name = path.Clean(name)
	if name == ".." || strings.HasPrefix(name, "../") {
		return "", false
	}
	return name, true
}

func (c *configurationCollector) sourcedInputs(file *syntax.File, dir string, readSource func(string) ([]byte, error), seen map[string]bool) error {
	var failure error
	syntax.Walk(file, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || failure != nil {
			return failure == nil
		}
		name, known := literalSource(call, dir)
		if !known || seen[name] {
			return true
		}
		seen[name] = true
		content, err := readSource(name)
		if err != nil {
			failure = layoutError(name, err)
			return false
		}
		dependency, err := parseShell(content, name)
		if err != nil {
			failure = layoutError(name, err)
			return false
		}
		c.scriptInputs(dependency, name)
		failure = c.sourcedInputs(dependency, path.Dir(name), readSource, seen)
		return failure == nil
	})
	return failure
}

func (e *lifecycleEvidence) sourcedScript(name string, values map[string]string, depth int) error {
	if depth > 8 || e.active[name] {
		return layoutError(name, fmt.Errorf("recursive authored source dependency is unsupported"))
	}
	content, err := e.readSource(name)
	if err != nil {
		return layoutError(name, err)
	}
	file, err := parseShell(content, name)
	if err != nil {
		return layoutError(name, err)
	}
	e.active[name] = true
	defer delete(e.active, name)
	previous := values["_LMW_SOURCE_DIR"]
	values["_LMW_SOURCE_DIR"] = path.Dir(name)
	defer func() { values["_LMW_SOURCE_DIR"] = previous }()
	return e.shell(file, content, values, depth)
}

// Recognize this source-relative root calculation by its parsed shell syntax,
// not by evaluating a command substitution or trusting the variable's name.
func repositoryRootWord(word *syntax.Word) bool {
	if word == nil {
		return false
	}
	var canonical bytes.Buffer
	if err := syntax.NewPrinter().Print(&canonical, word); err != nil {
		return false
	}
	switch canonical.String() {
	case `$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)`,
		`$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)`:
		return true
	}
	return false
}

func forwardsSSH(fn *syntax.FuncDecl) bool {
	forwarded := false
	syntax.Walk(fn.Body, func(node syntax.Node) bool {
		call, ok := node.(*syntax.CallExpr)
		if !ok || len(call.Args) == 0 || call.Args[0].Lit() != "ssh" {
			return true
		}
		for _, word := range call.Args[1:] {
			if forwardedPosition(word) == 1 {
				forwarded = true
			}
		}
		return true
	})
	return forwarded
}

func (e *lifecycleEvidence) forwardedSSH(args []*syntax.Word, values map[string]string, depth int) error {
	for _, arg := range args {
		offset := forwardedPosition(arg)
		if offset == 0 {
			continue
		}
		var commands []string
		for i := offset; ; i++ {
			value, ok := values[strconv.Itoa(i)]
			if !ok {
				break
			}
			commands = append(commands, value)
		}
		content := []byte(strings.Join(commands, " "))
		file, err := parseShell(content, "forwarded SSH command")
		if err != nil {
			continue // An unknown/non-shell argument cannot establish evidence.
		}
		remote := make(map[string]string, len(values)+1)
		for key, value := range values {
			remote[key] = value
		}
		remote["_LMW_REMOTE"] = "1"
		if err := e.shell(file, content, remote, depth); err != nil {
			return err
		}
	}
	return nil
}

// Recognize the authored array -> printf %q -> SSH command dataflow. A dead
// docker array, an echo, or a disconnected serialization cannot prove launch.
func (e *lifecycleEvidence) serializedDocker(file *syntax.File, args []*syntax.Word, values map[string]string) error {
	used := map[string]bool{}
	for _, word := range args {
		syntax.Walk(word, func(node syntax.Node) bool {
			if p, ok := node.(*syntax.ParamExp); ok {
				used[p.Param.Value] = true
			}
			return true
		})
	}
	var failure error
	syntax.Walk(file, func(node syntax.Node) bool {
		loop, ok := node.(*syntax.ForClause)
		if !ok || failure != nil {
			return failure == nil
		}
		iter, ok := loop.Loop.(*syntax.WordIter)
		if !ok || len(iter.Items) != 1 || !arrayExpansion(iter.Items[0]) {
			return true
		}
		p := simpleExpansion(iter.Items[0])
		if p == nil {
			return true
		}
		words := e.tensorfoldArrays[p.Param.Value]
		if len(words) < 2 || words[0].Lit() != "docker" || words[1].Lit() != "run" {
			return true
		}
		serialized := false
		syntax.Walk(loop, func(node syntax.Node) bool {
			a, ok := node.(*syntax.Assign)
			if !ok || a.Name == nil || !a.Append || !used[a.Name.Value] {
				return true
			}
			syntax.Walk(a.Value, func(node syntax.Node) bool {
				call, ok := node.(*syntax.CallExpr)
				if ok && len(call.Args) == 3 && call.Args[0].Lit() == "printf" {
					format, constant := constantWord(call.Args[1])
					if constant && format == "%q" && simpleVariable(call.Args[2]) == iter.Name.Value {
						serialized = true
					}
				}
				return true
			})
			return true
		})
		if serialized {
			remote := make(map[string]string, len(values)+1)
			for key, value := range values {
				remote[key] = value
			}
			remote["_LMW_REMOTE"] = "1"
			failure = e.docker(words[1:], remote)
		}
		return true
	})
	return failure
}

// A redirection alone does not prove literal env loading. Require the authored
// read/regex-capture/export parser, and reject any eval/source of the input.
func literalEnvironmentLoop(loop *syntax.WhileClause) bool {
	read, captures, exported, executable := false, map[string]string{}, false, false
	syntax.Walk(loop, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) != 0 {
			switch call.Args[0].Lit() {
			case "read":
				for _, arg := range call.Args[1:] {
					read = read || arg.Lit() == "-r"
				}
			case "eval", "source", ".":
				executable = true
			}
		}
		if a, ok := node.(*syntax.Assign); ok && a.Name != nil {
			p := simpleExpansion(a.Value)
			if p != nil && p.Param.Value == "BASH_REMATCH" {
				if index, ok := p.Index.(*syntax.Word); ok {
					captures[index.Lit()] = a.Name.Value
				}
			}
		}
		if decl, ok := node.(*syntax.DeclClause); ok && decl.Variant.Value == "export" {
			for _, arg := range decl.Args {
				if arg.Value == nil {
					continue
				}
				var key, value bool
				syntax.Walk(arg.Value, func(node syntax.Node) bool {
					if p, ok := node.(*syntax.ParamExp); ok {
						key = key || p.Param.Value == captures["2"]
						value = value || p.Param.Value == captures["3"]
					}
					return true
				})
				exported = exported || key && value
			}
		}
		return true
	})
	return read && exported && captures["2"] != "" && captures["3"] != "" && !executable
}

// sourceDirectoryWord matches "$(dirname -- "${BASH_SOURCE[0]}")/name" (with or
// without --) and returns name; nothing else in the word may vary.
func sourceDirectoryWord(word *syntax.Word) (string, bool) {
	var canonical bytes.Buffer
	if err := syntax.NewPrinter().Print(&canonical, word); err != nil {
		return "", false
	}
	text := canonical.String()
	for _, prefix := range []string{`"$(dirname -- "${BASH_SOURCE[0]}")/`, `"$(dirname "${BASH_SOURCE[0]}")/`} {
		rest, found := strings.CutPrefix(text, prefix)
		if !found || !strings.HasSuffix(rest, `"`) {
			continue
		}
		name := strings.TrimSuffix(rest, `"`)
		if name == "" || strings.ContainsAny(name, "$`\\\"' *?[") {
			return "", false
		}
		return name, true
	}
	return "", false
}
