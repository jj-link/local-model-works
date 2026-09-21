package repositorycompiler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/syntax"
)

func TestHostCacheOverrideReusesExistingRoot(t *testing.T) {
	for _, family := range []string{"qwen38-27b-rtx6000pro-dflash2", "qwen38-27b-dgx-spark-mtp"} {
		t.Run(family, func(t *testing.T) {
			manifest := configurationTemplate(t, family)
			reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", family, name)) }
			if err := compileConfiguration(manifest, reader); err != nil {
				t.Fatal(err)
			}
			configuration := manifest.Workloads[0].Upstream.Configuration[0]
			var edits []sourceconfig.Edit
			for _, edit := range configuration.Edits {
				if edit.Parameter == "hf_home" || edit.Parameter == "triton_cache_dir" {
					edits = append(edits, edit)
				}
			}
			configuration.Edits = edits
			original, err := reader(configuration.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, settings := range []map[string]any{
				{},
				{"hf_home": "/home/workbench/existing HF cache", "triton_cache_dir": "/data/it's a cache/$(literal)"},
			} {
				resolved, err := sourceconfig.Resolve([]sourceconfig.File{configuration}, settings, nil)
				if err != nil {
					t.Fatal(err)
				}
				patched := string(original)
				for _, file := range resolved {
					for i := len(file.Edits) - 1; i >= 0; i-- {
						edit := file.Edits[i]
						patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
					}
				}
				if len(settings) == 0 {
					if patched != string(original) {
						t.Fatal("unset cache setting changed the computed upstream default")
					}
					continue
				}
				parsed, err := parseShell([]byte(patched), "start.sh")
				if err != nil {
					t.Fatal(err)
				}
				roots := make(map[string]string)
				for _, stmt := range parsed.Stmts {
					call, ok := stmt.Cmd.(*syntax.CallExpr)
					if !ok || len(call.Args) != 0 {
						continue
					}
					for _, assignment := range call.Assigns {
						if assignment.Name != nil {
							if value, literal := constantWord(assignment.Value); literal {
								roots[assignment.Name.Value] = value
							}
						}
					}
				}
				if roots["HF_HOME"] != settings["hf_home"] || roots["TRITON_CACHE_DIR"] != settings["triton_cache_dir"] {
					t.Fatalf("original cache variables did not receive literal selected paths: %#v", roots)
				}
			}
		})
	}
}
