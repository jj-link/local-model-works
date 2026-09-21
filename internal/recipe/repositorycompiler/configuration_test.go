package repositorycompiler

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	recipeassets "github.com/jj-link/local-model-works/recipes"
	"mvdan.cc/sh/v3/syntax"
)

// Fixtures are unmodified files from each template's exact metadata.source
// revision. Tests parse source only; no upstream shell commands are executed.
func TestPinnedRuntimeConfiguration(t *testing.T) {
	validator := mustValidator(t)
	for _, family := range []string{
		"qwen38-27b-rtx6000pro-dflash2", "qwen38-27b-dgx-spark-mtp",
		"qwen38-flash-next-spark-tp1", "qwen38-flash-next-dspark-tp2",
		"glm53-flash-exl3-dflash2-spark-tp2", "deepseek-v4-flash-vision-exp-dspark-tp2",
	} {
		t.Run(family, func(t *testing.T) {
			manifest := configurationTemplate(t, family)
			before, _ := json.Marshal(manifest.Workloads[0].Upstream)
			readSource := func(name string) ([]byte, error) {
				return os.ReadFile(filepath.Join("testdata", family, name))
			}
			if err := compileConfiguration(manifest, readSource); err != nil {
				t.Fatal(err)
			}
			second := configurationTemplate(t, family)
			if err := compileConfiguration(second, readSource); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(manifest, second) {
				t.Fatal("same original source produced different configuration")
			}
			document, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			_, diagnostics, err := validator.ValidateStrict(document)
			if err != nil {
				t.Fatal(err)
			}
			for _, diagnostic := range diagnostics {
				if diagnostic.Severity == "error" {
					t.Fatalf("compiled configuration is invalid: %s: %s", diagnostic.Path, diagnostic.Message)
				}
			}
			upstream := *manifest.Workloads[0].Upstream
			upstream.Configuration = nil
			after, _ := json.Marshal(upstream)
			if string(before) != string(after) {
				t.Fatal("configuration extraction changed the original lifecycle or dependencies")
			}
			parameters := make(map[string]recipe.Parameter)
			for _, parameter := range manifest.Parameters {
				if _, exists := parameters[parameter.Name]; exists {
					t.Fatalf("duplicate parameter %s", parameter.Name)
				}
				parameters[parameter.Name] = parameter
			}
			for _, name := range []string{"port", "master_port", "head_ip", "worker_ip", "node_rank", "tensor_parallel_size", "container_name", "worker2_host"} {
				if _, exists := parameters[name]; exists {
					t.Fatalf("uncoupled topology/identity override exposed: %s", name)
				}
			}
			// Every generated field must reach either the original environment or
			// an explicit original-source edit, not just the manifest UI.
			bound := make(map[string]bool)
			for _, binding := range manifest.Workloads[0].Env {
				if strings.HasPrefix(binding, "${setting.") && strings.HasSuffix(binding, "}") {
					bound[strings.TrimSuffix(strings.TrimPrefix(binding, "${setting."), "}")] = true
				}
			}
			for _, file := range manifest.Workloads[0].Upstream.Configuration {
				content, err := readSource(file.Path)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(content)
				if file.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatal("adaptation does not pin the exact original bytes")
				}
				for _, edit := range file.Edits {
					bound[edit.Parameter] = true
				}
			}
			for name := range parameters {
				if !bound[name] {
					t.Fatalf("setting %s cannot reach startup", name)
				}
			}
			switch family {
			case "qwen38-27b-rtx6000pro-dflash2":
				assertDefault(t, parameters, "kv_cache_dtype", "fp8_e4m3")
				assertDefault(t, parameters, "context_length", int64(262144))
				assertDefault(t, parameters, "trust_remote_code", true)
				assertDefault(t, parameters, "speculative_draft_model_path", "z-lab/Qwen3.8-27B-DFlash2")
				if _, exists := parameters["max_running_requests"]; exists {
					t.Fatal("duplicated existing max_concurrent_requests binding")
				}
				if _, exists := parameters["extra_args"]; exists {
					t.Fatal("RTX has no upstream extra-arguments input")
				}
			case "qwen38-27b-dgx-spark-mtp":
				assertDefault(t, parameters, "context_length", int64(262144))
				assertOptional(t, parameters, "spec_steps")
				assertOptional(t, parameters, "prefill_cuda_graph")
				assertOptional(t, parameters, "extra_args")
				if _, exists := parameters["chunked_prefill_size"]; exists {
					t.Fatal("duplicated CHUNKED_PREFILL environment control")
				}
			case "qwen38-flash-next-spark-tp1":
				assertOptional(t, parameters, "container_mem_gib")
				assertOptional(t, parameters, "gpu_memory_utilization")
				assertOptional(t, parameters, "extra_vllm_args")
			case "qwen38-flash-next-dspark-tp2":
				assertDefault(t, parameters, "yarn_enable", false)
				assertDefault(t, parameters, "gpu_memory_utilization", 0.835)
				assertOptional(t, parameters, "ple_embedding_dtype")
			case "glm53-flash-exl3-dflash2-spark-tp2":
				assertOptional(t, parameters, "exl3_temp_rows_fused")
				assertOptional(t, parameters, "hf_home")
				assertOptional(t, parameters, "extra_args")
			case "deepseek-v4-flash-vision-exp-dspark-tp2":
				assertDefault(t, parameters, "dspark_max_inflight_prefills", int64(1))
				assertOptional(t, parameters, "hf_cache")
				assertOptional(t, parameters, "dspark_api_keys")
				assertOptional(t, parameters, "nccl_gin_enable")
				if !parameters["dspark_api_keys"].Sensitive || !parameters["vllm_api_key"].Sensitive {
					t.Fatal("documented API credentials are not sensitive")
				}
				if _, exists := parameters["vllm_dspark_confidence_scheduler"]; exists {
					t.Fatal("Stage-C-only knob exposed for the Anemll lifecycle")
				}
			}
		})
	}
}

func TestEnvironmentPreservesShellAndLiteralSemantics(t *testing.T) {
	for _, tc := range []struct {
		format string
		input  string
		want   string
	}{
		{"shell", "VALUE='a b # c $(not-executed)'\n", "a b # c $(not-executed)"},
		{"literal", "VALUE=\"a b # c\"\n", `"a b # c"`},
	} {
		t.Run(tc.format, func(t *testing.T) {
			manifest := &recipe.Manifest{Workloads: []recipe.Workload{{Env: map[string]string{}}}}
			collector := configurationCollector{manifest: manifest, workload: &manifest.Workloads[0]}
			if err := collector.environment([]byte(tc.input+"# HF_TOKEN=hf_fake_sample\nCACHE=${HOME}/cache\n"), ".env.example", tc.format); err != nil {
				t.Fatal(err)
			}
			if value := collector.parameter("value"); value == nil || value.Default != tc.want {
				t.Fatalf("quoting semantics changed: %#v", value)
			}
			if token := collector.parameter("hf_token"); token == nil || !token.Sensitive || !token.Optional || token.Default != nil {
				t.Fatalf("sample credential became a default: %#v", token)
			}
			cache := collector.parameter("cache")
			if tc.format == "shell" && (!cache.Optional || cache.Default != nil) {
				t.Fatal("computed shell default was frozen as literal text")
			}
			if tc.format == "literal" && cache.Default != "${HOME}/cache" {
				t.Fatal("literal dollar text was interpreted as shell syntax")
			}
		})
	}
}

func TestDirectArgvEditsPreserveArgumentsAndSurroundingSource(t *testing.T) {
	original := "#!/bin/bash\n# Unicode prefix: α — byte offsets, not rune offsets\n" +
		"docker run image python3 -m sglang.launch_server --trust-remote-code --capture-sizes 1 2 4 --json '{\"a\":1}' --port 8888 >log\n"
	file, err := parseShell([]byte(original), "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	manifest := &recipe.Manifest{Workloads: []recipe.Workload{{Env: map[string]string{}}}}
	collector := configurationCollector{manifest: manifest, workload: &manifest.Workloads[0]}
	configuration, err := collector.sglang(file, []byte(original), "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := sourceconfig.Resolve([]sourceconfig.File{configuration}, map[string]any{
		"trust_remote_code": false,
		"capture_sizes":     `["8","a b","$(touch forbidden)"]`,
		"json":              `{"message":"it's $(not a command)"}`,
	}, func(template string) (string, error) {
		return strings.ReplaceAll(template, "${node.accelerators}", "GPU-reviewed"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	patched := original
	for i := len(resolved[0].Edits) - 1; i >= 0; i-- {
		edit := resolved[0].Edits[i]
		patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
	}
	parsed, err := parseShell([]byte(patched), "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	call := parsed.Stmts[0].Cmd.(*syntax.CallExpr)
	var args []string
	for _, arg := range call.Args {
		value, constant := constantWord(arg)
		if !constant {
			t.Fatal("user argument introduced shell expansion")
		}
		args = append(args, value)
	}
	want := []string{"docker", "run", "--env=CUDA_VISIBLE_DEVICES=GPU-reviewed", "image", "python3", "-m", "sglang.launch_server", "--capture-sizes", "8", "a b", "$(touch forbidden)", "--json", `{"message":"it's $(not a command)"}`, "--port", "8888"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("argv changed: got %#v, want %#v", args, want)
	}
	if !strings.HasPrefix(patched, "#!/bin/bash\n# Unicode prefix: α — byte offsets, not rune offsets\n") || !strings.HasSuffix(patched, " >log\n") {
		t.Fatal("source outside argument edits changed")
	}
}

func TestUnsupportedSGLangLayoutIsNotPatched(t *testing.T) {
	for _, original := range []string{
		"docker run image bash -c 'python3 -m sglang.launch_server --context-length 128'\n",
		"cat <<'EOF'\npython3 -m sglang.launch_server --context-length 128\nEOF\n",
		"echo python3 -m sglang.launch_server --context-length 128\n",
		"python3 -m sglang.launch_server --context-length=128\n",
		"python3 -m sglang.launch_server --context-length 128\npython3 -m sglang.launch_server --context-length 256\n",
	} {
		file, err := parseShell([]byte(original), "start.sh")
		if err != nil {
			t.Fatal(err)
		}
		collector := configurationCollector{manifest: &recipe.Manifest{}, workload: &recipe.Workload{}}
		if _, err := collector.sglang(file, []byte(original), "start.sh"); err == nil {
			t.Fatalf("unsupported source layout accepted: %s", original)
		}
	}
}

func configurationTemplate(t *testing.T, family string) *recipe.Manifest {
	t.Helper()
	content, err := recipeassets.Templates.ReadFile(family + "/recipe.yaml")
	if err != nil {
		t.Fatal(err)
	}
	document, err := recipe.YAMLOrJSON(content)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse(document)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertDefault(t *testing.T, parameters map[string]recipe.Parameter, name string, want any) {
	t.Helper()
	parameter, exists := parameters[name]
	if !exists || !reflect.DeepEqual(parameter.Default, want) {
		t.Fatalf("%s default: got %#v, want %#v", name, parameter.Default, want)
	}
}

func assertOptional(t *testing.T, parameters map[string]recipe.Parameter, name string) {
	t.Helper()
	parameter, exists := parameters[name]
	if !exists || !parameter.Optional || parameter.Default != nil {
		t.Fatalf("%s must preserve an unspecified upstream default: %#v", name, parameter)
	}
}
