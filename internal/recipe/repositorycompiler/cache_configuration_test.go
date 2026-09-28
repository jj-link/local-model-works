package repositorycompiler

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
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

func TestLiteralHostCacheDefaultsRemainAuthoritative(t *testing.T) {
	source := []byte("HF_HOME='/source/model cache'\nTRITON_CACHE_DIR='/source/kernels'\n")
	parsed, err := parseShell(source, "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	manifest := &recipe.Manifest{}
	workload := &recipe.Workload{Upstream: &recipe.UpstreamExecution{
		Configuration: []sourceconfig.File{{Path: "start.sh"}},
	}}
	collector := configurationCollector{manifest: manifest, workload: workload}
	collector.hardcodedCaches(parsed, "start.sh")
	settings, err := manifest.EffectiveSettings(nil)
	if err != nil {
		t.Fatal(err)
	}
	if settings["hf_home"] != "/source/model cache" || settings["triton_cache_dir"] != "/source/kernels" {
		t.Fatalf("literal source caches lost precedence to inferred device defaults: %v", settings)
	}
	overridden, err := manifest.EffectiveSettings(map[string]any{"hf_home": "/operator/models"})
	if err != nil || overridden["hf_home"] != "/operator/models" || overridden["triton_cache_dir"] != "/source/kernels" {
		t.Fatalf("operator override did not preserve other literal defaults: %v, %v", overridden, err)
	}
}

func TestExecutableHostCacheFallbacksRetainLiteralDefaults(t *testing.T) {
	for _, tc := range []struct {
		name, source, setting string
		want                  any
	}{
		{"assignment", `HF_HOME="${HF_HOME:-/source/models}"`, "hf_home", "/source/models"},
		{"function", `launch() { printf '%s' "${HF_CACHE:-/source/cache}"; }`, "hf_cache", "/source/cache"},
		{"documented", "# HF_HOME=/example/path\nlaunch() { printf '%s' \"${HF_HOME:-/actual/models}\"; }", "hf_home", "/actual/models"},
		{"computed", `HF_HOME="${HF_HOME:-$HOME/.cache/huggingface}"`, "hf_home", nil},
		{"empty-sentinel", `WORKER_HF_CACHE="${WORKER_HF_CACHE:-${HF_CACHE:-}}"`, "hf_cache", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseShell([]byte(tc.source), "start.sh")
			if err != nil {
				t.Fatal(err)
			}
			manifest := &recipe.Manifest{}
			workload := &recipe.Workload{Env: map[string]string{}, Upstream: &recipe.UpstreamExecution{}}
			collector := configurationCollector{manifest: manifest, workload: workload}
			if err := collector.environment([]byte("# HF_HOME=/commented/example\n"), ".env.example", "shell"); err != nil {
				t.Fatal(err)
			}
			collector.documentedInputs(parsed, []byte(tc.source), "start.sh")
			collector.scriptInputs(parsed, "start.sh")
			settings, err := manifest.EffectiveSettings(nil)
			if err != nil {
				t.Fatal(err)
			}
			if settings[tc.setting] != tc.want {
				t.Fatalf("executable default lost or commented/computed value promoted: got %v, want %v", settings[tc.setting], tc.want)
			}
		})
	}
}

func TestDeepSeekComputedCacheRetainsDeviceDefaultEligibility(t *testing.T) {
	const family = "deepseek-v4-flash-vision-exp-dspark-tp2"
	manifest := configurationTemplate(t, family)
	reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", family, name)) }
	if err := compileConfiguration(manifest, reader); err != nil {
		t.Fatal(err)
	}
	cache := manifest.ParameterByName("hf_cache")
	if cache == nil || cache.Default != nil {
		t.Fatalf("nested empty fallback bypasses configured shared-cache inference: %+v", cache)
	}
}
