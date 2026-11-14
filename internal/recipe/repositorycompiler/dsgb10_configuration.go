package repositorycompiler

import (
	"fmt"
	"slices"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/syntax"
)

// dsgb10WithoutKey is the reviewed adaptation of the DSGB10 service: LMW's
// readiness probe and OpenAI-compatible clients send no API key, so upstream's
// two key-file checks in check() and dsfront's --api-key-file argument pair in
// start_front() are deleted. An empty JSON argv template removes exactly those
// parsed words; any other change to them rejects the source.
func (c *configurationCollector) dsgb10WithoutKey(file *syntax.File, content []byte, source string) error {
	var checks []*syntax.Stmt
	var flag [][2]*syntax.Word
	for _, stmt := range file.Stmts {
		fn, ok := stmt.Cmd.(*syntax.FuncDecl)
		if !ok {
			continue
		}
		switch fn.Name.Value {
		case "check":
			block, ok := fn.Body.Cmd.(*syntax.Block)
			if !ok {
				continue
			}
			for _, statement := range block.Stmts {
				if keyFileGuard(statement) {
					checks = append(checks, statement)
				}
			}
		case "start_front":
			syntax.Walk(fn.Body, func(node syntax.Node) bool {
				call, ok := node.(*syntax.CallExpr)
				if !ok {
					return true
				}
				for i := 0; i+1 < len(call.Args); i++ {
					if call.Args[i].Lit() == "--api-key-file" && simpleVariable(call.Args[i+1]) == "KEY" {
						flag = append(flag, [2]*syntax.Word{call.Args[i], call.Args[i+1]})
					}
				}
				return true
			})
		}
	}
	if len(checks) != 2 || len(flag) != 1 {
		return fmt.Errorf("authored API-key requirement changed: found %d key-file checks and %d --api-key-file arguments, want 2 and 1", len(checks), len(flag))
	}
	configuration := c.configurationFile(content, source)
	remove := func(start, end syntax.Pos) {
		configuration.Edits = append(configuration.Edits, sourceconfig.Edit{Start: int(start.Offset()), End: int(end.Offset()), Template: "[]", Format: "argv"})
	}
	for _, check := range checks {
		remove(check.Pos(), check.End())
	}
	remove(flag[0][0].Pos(), flag[0][1].End())
	// The edits remove every consumer of the key file, so its inferred input
	// would be a setting with no effect.
	if c.workload.Env["DSGB10_KEY_FILE"] == "${setting.dsgb10_key_file}" {
		delete(c.workload.Env, "DSGB10_KEY_FILE")
		c.manifest.Parameters = slices.DeleteFunc(c.manifest.Parameters, func(p recipe.Parameter) bool { return p.Name == "dsgb10_key_file" })
	}
	return nil
}

// keyFileGuard matches `[ <test of "$KEY"> ] || die ...`.
func keyFileGuard(stmt *syntax.Stmt) bool {
	or, ok := stmt.Cmd.(*syntax.BinaryCmd)
	if !ok || or.Op != syntax.OrStmt {
		return false
	}
	test, ok := or.X.Cmd.(*syntax.CallExpr)
	if !ok || len(test.Args) == 0 || test.Args[0].Lit() != "[" {
		return false
	}
	die, ok := or.Y.Cmd.(*syntax.CallExpr)
	if !ok || len(die.Args) == 0 || die.Args[0].Lit() != "die" {
		return false
	}
	key := false
	syntax.Walk(test, func(node syntax.Node) bool {
		if p, ok := node.(*syntax.ParamExp); ok && p.Param.Value == "KEY" {
			key = true
		}
		return !key
	})
	return key
}
