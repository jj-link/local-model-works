package repositorycompiler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"gopkg.in/yaml.v3"
	"mvdan.cc/sh/v3/syntax"
)

func compiledFragments(t *testing.T, family string, settings map[string]any) (*recipe.Manifest, map[string]string) {
	t.Helper()
	manifest := configurationTemplate(t, family)
	reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", family, name)) }
	if err := compileConfiguration(manifest, reader); err != nil {
		t.Fatal(err)
	}
	resolved, err := sourceconfig.Resolve(manifest.Workloads[0].Upstream.Configuration, settings, func(value string) (string, error) {
		return strings.ReplaceAll(value, "${node.accelerators}", "GPU-reviewed"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]string)
	start := strings.TrimPrefix(manifest.Workloads[0].Upstream.Start[0], "./")
	original, err := reader(start)
	if err != nil {
		t.Fatal(err)
	}
	files[start] = string(original)
	for _, file := range resolved {
		original, err := reader(file.Path)
		if err != nil {
			t.Fatal(err)
		}
		patched := string(original)
		for i := len(file.Edits) - 1; i >= 0; i-- {
			edit := file.Edits[i]
			patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
		}
		files[file.Path] = patched
	}
	return manifest, files
}

func boundEnvironment(t *testing.T, manifest *recipe.Manifest, selected map[string]any) []string {
	t.Helper()
	var result []string
	for key, value := range selected {
		if manifest.ParameterByName(key) == nil {
			t.Fatalf("undeclared setting %s", key)
		}
		for name, binding := range manifest.Workloads[0].Env {
			if binding == "${setting."+key+"}" {
				result = append(result, name+"="+fmt.Sprint(value))
			}
		}
	}
	return result
}

func fragmentArgv(t *testing.T, script string, environment []string) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is required for original-fragment consumption regressions")
	}
	command := exec.Command(bash, "-c", script)
	command.Dir = t.TempDir()
	command.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + command.Dir, "LC_ALL=C"}, environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("original fragment: %v\n%s", err, output)
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
}

func originalAssignments(t *testing.T, content string, accept func(*syntax.Assign) bool) string {
	t.Helper()
	file, err := parseShell([]byte(content), "original fragment")
	if err != nil {
		t.Fatal(err)
	}
	var result strings.Builder
	syntax.Walk(file, func(node syntax.Node) bool {
		if assignment, ok := node.(*syntax.Assign); ok && assignment.Name != nil && accept(assignment) {
			result.WriteString(content[assignment.Pos().Offset():assignment.End().Offset()])
			result.WriteByte('\n')
			return false
		}
		return true
	})
	return result.String()
}

func flagValue(t *testing.T, arguments []string, flag string) string {
	t.Helper()
	for i := range len(arguments) - 1 {
		if arguments[i] == flag {
			return arguments[i+1]
		}
	}
	t.Fatalf("missing %s in %#v", flag, arguments)
	return ""
}

func TestQwenSelectedInputsReachGeneratedDocker(t *testing.T) {
	literal := "/data/hf cache/it's $(not_a_command) `literal`\nLAUNCH_EOF"
	for _, family := range []string{"qwen38-flash-next-spark-tp1", "qwen38-flash-next-dspark-tp2"} {
		for _, selected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/selected=%v", family, selected), func(t *testing.T) {
				settings := map[string]any{}
				if selected {
					settings["hf_home"] = literal
				}
				manifest, files := compiledFragments(t, family, settings)
				content := files["start.sh"]
				cache := originalAssignments(t, content, func(a *syntax.Assign) bool { return a.Name.Value == "HF_CACHE_DIR" })
				if cache == "" {
					t.Fatal("missing original cache selection")
				}
				file, err := parseShell([]byte(content), "original launch")
				if err != nil {
					t.Fatal(err)
				}
				launches := 0
				syntax.Walk(file, func(node syntax.Node) bool {
					r, ok := node.(*syntax.Redirect)
					if !ok || r.Hdoc == nil || r.Op != syntax.Hdoc {
						return true
					}
					body, _, quoted, err := bashHeredoc(r, []byte(content))
					if err != nil {
						t.Fatal(err)
					}
					if quoted || !strings.HasPrefix(string(body), "#!/bin/bash\n") {
						return true
					}
					delimiter, _ := constantWord(r.Word)
					worker := strings.Contains(string(body), "$WORKER_HF_MOUNT")
					mount := ""
					if worker {
						selectedVariable := "REMOTE_HF"
						if selected {
							selectedVariable = "NFS_VOLUME"
						}
						mount = originalAssignments(t, content, func(a *syntax.Assign) bool {
							return a.Name.Value == "WORKER_HF_MOUNT" && strings.Contains(content[a.Pos().Offset():a.End().Offset()], selectedVariable)
						})
					}
					script := "docker() { printf '%s\\0' \"$@\"; }; not_a_command() { exit 91; }; unset HF_HOME\n"
					if selected {
						script = "docker() { printf '%s\\0' \"$@\"; }; not_a_command() { exit 91; }\n"
					}
					script += cache + mount + "generated=$(cat <<" + delimiter + "\n" + string(body) + delimiter + "\n)\neval \"$generated\"\n"
					environment := append(boundEnvironment(t, manifest, settings), "HOME=/computed/home", "REMOTE_HF="+literal, "NFS_VOLUME=authored-shared-cache", "IMAGE=fixture/image", "MODEL_ID=model/selected", "VLLM_ARGS_STR=--inherited sentinel")
					arguments := fragmentArgv(t, script, environment)
					want := "/computed/home/.cache/huggingface:/root/.cache/huggingface"
					if selected || worker {
						want = literal + ":/root/.cache/huggingface"
					}
					if worker && selected {
						want = "authored-shared-cache:/root/.cache/huggingface:ro"
					}
					found := false
					for i := range len(arguments) - 1 {
						if arguments[i] == "-v" && arguments[i+1] == want {
							found = true
						}
					}
					if !found {
						t.Fatalf("selected/computed cache split at generated launch: want %q in %#v", want, arguments)
					}
					if got := flagValue(t, arguments, "--inherited"); got != "sentinel" {
						t.Fatalf("inherited arguments changed: %q", got)
					}
					launches++
					return true
				})
				want := 1
				if strings.Contains(family, "dspark") {
					want = 2
				}
				if launches != want {
					t.Fatalf("exercised %d original launchers, want %d", launches, want)
				}
			})
		}
	}
}

func TestQwenHardcodedArgumentsPreserveFlattenedLiteralWords(t *testing.T) {
	literal := "parser it's $(not_a_command) `literal`\nLAUNCH_EOF\r"
	for _, family := range []string{"qwen38-flash-next-spark-tp1", "qwen38-flash-next-dspark-tp2"} {
		t.Run(family, func(t *testing.T) {
			for _, configured := range []bool{false, true} {
				settings := map[string]any{}
				if configured {
					settings = map[string]any{"tool_call_parser": literal, "load_format": "custom literal", "enable_auto_tool_choice": false}
				}
				_, files := compiledFragments(t, family, settings)
				fragment := originalAssignments(t, files["start.sh"], func(a *syntax.Assign) bool {
					if a.Name.Value == "VLLM_ARGS_STR" {
						return true
					}
					if a.Name.Value != "VLLM_ARGS" || a.Array == nil {
						return false
					}
					if len(a.Array.Elems) == 0 {
						return true
					}
					flag, _ := constantWord(a.Array.Elems[0].Value)
					return flag == "--tool-call-parser" || flag == "--load-format" || flag == "--enable-auto-tool-choice" || flag == "--kv-cache-dtype"
				})
				arguments := fragmentArgv(t, "capture() { printf '%s\\0' \"$@\"; }; not_a_command() { exit 92; }\n"+fragment+"eval \"capture $VLLM_ARGS_STR\"", []string{"KV_CACHE_DTYPE=literal dtype ' $(not_a_command)"})
				wantParser, wantLoad := "qwen3_coder", "safetensors"
				if configured {
					wantParser, wantLoad = literal, "custom literal"
				}
				if got := flagValue(t, arguments, "--tool-call-parser"); got != wantParser {
					t.Fatalf("parser split: %q", got)
				}
				if got := flagValue(t, arguments, "--load-format"); got != wantLoad {
					t.Fatalf("load format split: %q", got)
				}
				if got := flagValue(t, arguments, "--kv-cache-dtype"); got != "literal dtype ' $(not_a_command)" {
					t.Fatalf("environment-controlled scalar split: %q", got)
				}
				present := false
				for _, argument := range arguments {
					if argument == "--enable-auto-tool-choice" {
						present = true
					}
				}
				if present == configured {
					t.Fatalf("presence flag removal/default violated: %#v", arguments)
				}
			}
		})
	}
}

func TestSingleSparkControllingAliasAndFractionalBudgets(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python is required for the original budget arithmetic fragment")
	}
	family := "qwen38-flash-next-spark-tp1"
	selected := map[string]any{"tp1_model_id": "org/model's literal $(not_a_command)", "host_slack_gib": 5.5, "kv_target_gib": 20.5, "host_reserve_gib": 26.25}
	manifest, files := compiledFragments(t, family, selected)
	if _, err := manifest.EffectiveSettings(selected); err != nil {
		t.Fatal(err)
	}
	content := files["start.sh"]
	file, err := parseShell([]byte(content), "model selection")
	if err != nil {
		t.Fatal(err)
	}
	var branch, budget string
	for _, stmt := range file.Stmts {
		if _, ok := stmt.Cmd.(*syntax.IfClause); ok && strings.HasPrefix(content[stmt.Pos().Offset():stmt.End().Offset()], "if [[ -n \"${TP1_MODEL_ID:-}\"") {
			branch = content[stmt.Pos().Offset():stmt.End().Offset()]
		}
		if strings.HasPrefix(content[stmt.Pos().Offset():stmt.End().Offset()], "read -r WEIGHTS_GPU_GIB ") {
			budget = content[stmt.Pos().Offset():stmt.End().Offset()]
		}
	}
	if branch == "" {
		t.Fatal("missing original TP1 override branch")
	}
	if budget == "" {
		t.Fatal("missing original budget calculation")
	}
	defaults := originalAssignments(t, content, func(a *syntax.Assign) bool {
		return a.Name.Value == "STOCK_MODEL_ID" || a.Name.Value == "ABLIT_MODEL_ID" || a.Name.Value == "ABLIT" || a.Name.Value == "HOST_SLACK_GIB" || a.Name.Value == "KV_TARGET_GIB" || a.Name.Value == "HOST_RESERVE_GIB"
	})
	container := originalAssignments(t, content, func(a *syntax.Assign) bool { return a.Name.Value == "CONTAINER_MEM_GIB" })
	environment := append(boundEnvironment(t, manifest, selected), "WEIGHT_BYTES=10737418240", "PLE_GIB=2", "OVERHEAD_GIB=3", "MTP_GIB=0", "MAX_MODEL_LEN=1", "KV_BYTES_PER_TOKEN=29482", "KV_MULT=1", "MEM_TOTAL_GIB=100")
	arguments := fragmentArgv(t, "warn() { :; }; unset CONTAINER_MEM_GIB\n"+defaults+branch+"\n"+budget+"\n"+container+"\nprintf '%s\\0' \"$MODEL_ID\" \"$BUDGET_GIB\" \"$BUDGET_CAP_GIB\" \"$CONTAINER_MEM_GIB\"", environment)
	want := []string{selected["tp1_model_id"].(string), "31.50", "73.75", "37"}
	if !reflect.DeepEqual(arguments, want) {
		t.Fatalf("controlling alias/fractional budget changed: got %#v, want %#v", arguments, want)
	}
}

func TestDeepSeekOriginalComposeConsumesRuntimeControls(t *testing.T) {
	literal := "deepseek parser's $(not_a_command)\nline\r"
	selected := map[string]any{"tool_call_parser": literal, "kv_cache_dtype": "custom_dtype", "enable_prefix_caching": false, "generation_config": "auto", "docker_shm_size": "48gb", "vllm_cache_root": "/cache/literal's $root", "gb10_hybrid_nvfp4_m_threshold": 192, "dspark_skip_issue22_hotfix": 1, "flashinfer_workspace_base": "/cache/work space's $root", "dspark_tmp_host": "/host/tmp space", "vllm_plugins": "plugin_a,plugin_b"}
	manifest, files := compiledFragments(t, "deepseek-v4-flash-vision-exp-dspark-tp2", selected)
	if manifest.ParameterByName("gpu_memory_utilization") != nil {
		t.Fatal("overwritten historical alias is selectable")
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(files["docker-compose.dspark.yml"]), &document); err != nil {
		t.Fatal(err)
	}
	service := yamlField(yamlField(document.Content[0], "services"), "vllm-dspark")
	if got := yamlField(service, "shm_size").Value; got != "48gb" {
		t.Fatalf("Compose shm input not selected: %q", got)
	}
	cache := strings.ReplaceAll(yamlField(yamlField(service, "environment"), "VLLM_CACHE_ROOT").Value, "$$", "$")
	if cache != selected["vllm_cache_root"] {
		t.Fatalf("Compose YAML scalar corrupted: %q", cache)
	}
	command := strings.ReplaceAll(yamlField(service, "command").Content[2].Value, "$$", "$")
	parsed, err := parseShell([]byte(command), "original Compose command")
	if err != nil {
		t.Fatal(err)
	}
	var invocation string
	syntax.Walk(parsed, func(node syntax.Node) bool {
		if call, ok := node.(*syntax.CallExpr); ok && len(call.Args) > 3 && call.Args[0].Lit() == "exec" && call.Args[1].Lit() == "/usr/local/bin/vllm" {
			invocation = "capture " + command[call.Args[2].Pos().Offset():call.End().Offset()]
		}
		return true
	})
	if invocation == "" {
		t.Fatal("missing original Compose exec")
	}
	arguments := fragmentArgv(t, "capture() { printf '%s\\0' \"$@\"; }; not_a_command() { exit 93; }\n"+invocation, boundEnvironment(t, manifest, selected))
	if got := flagValue(t, arguments, "--tool-call-parser"); got != literal {
		t.Fatalf("Compose parser literal corrupted: %q", got)
	}
	if got := flagValue(t, arguments, "--generation-config"); got != "auto" {
		t.Fatalf("Compose generation config ignored: %q", got)
	}
	if got := flagValue(t, arguments, "--kv-cache-dtype"); got != "custom_dtype" {
		t.Fatalf("Compose KV dtype ignored: %q", got)
	}
	if got := flagValue(t, arguments, "--master-port"); got != "25000" {
		t.Fatalf("folded Compose exec truncated after flag removal: %q", got)
	}
	for _, argument := range arguments {
		if argument == "--enable-prefix-caching" {
			t.Fatal("removed Compose flag is still consumed")
		}
	}
	// Evaluate only the original Compose scalar expansions, not its hotfix or
	// setup command. These are the map values forwarded to the real container.
	var probe strings.Builder
	probe.WriteString("printf '%s\\0'")
	for _, name := range []string{"GB10_HYBRID_NVFP4_M_THRESHOLD", "DSPARK_SKIP_ISSUE22_HOTFIX", "FLASHINFER_WORKSPACE_BASE", "VLLM_PLUGINS"} {
		value := yamlField(yamlField(service, "environment"), name)
		if value == nil {
			t.Fatalf("missing consumed Compose map input %s", name)
		}
		probe.WriteString(" \"" + value.Value + "\"")
	}
	for _, volume := range yamlField(service, "volumes").Content {
		if strings.HasPrefix(volume.Value, "${DSPARK_TMP_HOST:-") {
			probe.WriteString(" \"" + volume.Value + "\"")
		}
	}
	consumed := fragmentArgv(t, probe.String(), boundEnvironment(t, manifest, selected))
	want := []string{"192", "1", selected["flashinfer_workspace_base"].(string), "plugin_a,plugin_b", "/host/tmp space:/tmp"}
	if !reflect.DeepEqual(consumed, want) {
		t.Fatalf("Compose inputs ignored: got %#v, want %#v", consumed, want)
	}
}

func TestGLMWorkerEnvironmentSerializationKeepsLiteralValues(t *testing.T) {
	selected := map[string]any{"pytorch_cuda_alloc_conf": "expandable_segments:True literal's $(not_a_command)", "do_not_track": 0}
	manifest, files := compiledFragments(t, "glm53-flash-exl3-dflash2-spark-tp2", selected)
	content := files["start.sh"]
	common := originalAssignments(t, content, func(a *syntax.Assign) bool { return a.Name.Value == "nccl_common" && !a.Append })
	file, err := parseShell([]byte(content), "original worker environment")
	if err != nil {
		t.Fatal(err)
	}
	loops := make(map[string]string)
	syntax.Walk(file, func(node syntax.Node) bool {
		loop, ok := node.(*syntax.ForClause)
		if !ok {
			return true
		}
		syntax.Walk(loop, func(inner syntax.Node) bool {
			assignment, ok := inner.(*syntax.Assign)
			if !ok || assignment.Name == nil || !assignment.Append {
				return true
			}
			name := assignment.Name.Value
			if name == "worker_nccl" || name == "serve_env" {
				if _, exists := loops[name]; exists {
					t.Fatalf("multiple original serialization loops for %s", name)
				}
				loops[name] = content[loop.Pos().Offset():loop.End().Offset()]
			}
			return true
		})
		return false
	})
	if loops["worker_nccl"] == "" || loops["serve_env"] == "" {
		t.Fatal("missing original serialization loops")
	}
	script := "capture() { printf '%s\\0' \"$@\"; }; not_a_command() { exit 94; }\n" + common + "worker_nccl=''; serve_env=''\n" + loops["worker_nccl"] + "\n" + loops["serve_env"] + "\neval \"capture $worker_nccl $serve_env\""
	literal := "literal's $(not_a_command) `literal`\nvalue"
	arguments := fragmentArgv(t, script, append(boundEnvironment(t, manifest, selected), "MODEL_DIR="+literal, "LIMIT_MM="+literal))
	for _, want := range []string{"PYTORCH_CUDA_ALLOC_CONF=" + selected["pytorch_cuda_alloc_conf"].(string), "DO_NOT_TRACK=0", "MODEL_DIR=" + literal, "LIMIT_MM=" + literal} {
		found := false
		for i := 1; i < len(arguments); i++ {
			if arguments[i-1] == "-e" && arguments[i] == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("worker env split: missing %q in %#v", want, arguments)
		}
	}
}

func TestOriginalDockerConstantsReachEveryServingInvocation(t *testing.T) {
	for _, family := range []string{"qwen38-27b-rtx6000pro-dflash2", "qwen38-27b-dgx-spark-mtp", "glm53-flash-exl3-dflash2-spark-tp2"} {
		t.Run(family, func(t *testing.T) {
			selected := map[string]any{"docker_shm_size": "48g"}
			if strings.Contains(family, "rtx") {
				selected["image"] = "selected/image:tag"
			}
			_, files := compiledFragments(t, family, selected)
			content := files["start.sh"]
			assignments := originalAssignments(t, content, func(a *syntax.Assign) bool { return a.Name.Value == "IMAGE" })
			file, err := parseShell([]byte(content), "original Docker commands")
			if err != nil {
				t.Fatal(err)
			}
			var mountSelector string
			syntax.Walk(file, func(node syntax.Node) bool {
				function, ok := node.(*syntax.FuncDecl)
				if ok && function.Name.Value == "_hf_mount" {
					mountSelector = content[function.Pos().Offset():function.End().Offset()] + "\n"
					return false
				}
				return true
			})
			count := 0
			syntax.Walk(file, func(node syntax.Node) bool {
				call, ok := node.(*syntax.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				serving := false
				if call.Args[0].Lit() == "docker" && call.Args[1].Lit() == "run" {
					for _, word := range call.Args {
						if word.Lit() == "sglang.launch_server" || word.Lit() == "/start.sh" {
							serving = true
						}
					}
				}
				if call.Args[0].Lit() == "worker_ssh" && strings.HasPrefix(content[call.Args[1].Pos().Offset():call.Args[1].End().Offset()], "\"docker run ") {
					serving = true
				}
				if !serving {
					return true
				}
				fragment := content[call.Pos().Offset():call.End().Offset()]
				modes := []string{"0"}
				worker := call.Args[0].Lit() == "worker_ssh"
				if worker {
					if mountSelector == "" {
						t.Fatal("missing original worker mount selector")
					}
					modes = append(modes, "1")
				}
				for _, mode := range modes {
					cache := "/worker/cache's \"quoted\" $(literal) `literal` space\nline"
					nfsMount := "/shared/cache's \"quoted\" $(literal) `literal` space\nline:/root/.cache/huggingface:ro"
					script := "exec 3>&1; docker() { printf '%s\\0' \"$@\" >&3; }; worker_ssh() { eval \"$1\"; }; literal() { exit 95; }; nfs_hf_mount_spec() { printf '%s' \"$NFS_TEST_MOUNT\"; }\n" + mountSelector + assignments + fragment
					arguments := fragmentArgv(t, script, []string{"IMAGE=fixture/image", "WORKER_CACHE_DIR=" + cache, "NFS_SHARE=" + mode, "NFS_TEST_MOUNT=" + nfsMount})
					if got := flagValue(t, arguments, "--shm-size"); got != "48g" {
						t.Fatalf("Docker shm override ignored: %q", got)
					}
					if strings.Contains(family, "rtx") {
						found := false
						for _, argument := range arguments {
							if argument == "selected/image:tag" {
								found = true
							}
						}
						if !found {
							t.Fatalf("original image assignment not consumed: %#v", arguments)
						}
					}
					if worker {
						want := cache + ":/root/.cache/huggingface"
						if mode == "1" {
							want = nfsMount
						}
						found := false
						for i := 1; i < len(arguments); i++ {
							if arguments[i-1] == "-v" && arguments[i] == want {
								found = true
							}
						}
						if !found {
							t.Fatalf("original worker cache mount split (NFS_SHARE=%s): want %q in %#v", mode, want, arguments)
						}
					}
				}
				count++
				return true
			})
			want := 1
			if strings.HasPrefix(family, "glm") {
				want = 2
			}
			if count != want {
				t.Fatalf("exercised %d serving invocations, want %d", count, want)
			}
		})
	}
}

func TestGLMOriginalRankArraysConsumeParserAndPresenceChoices(t *testing.T) {
	literal := "parser's $(not_a_command)\nEOF"
	_, files := compiledFragments(t, "glm53-flash-exl3-dflash2-spark-tp2", map[string]any{"tool_call_parser": literal, "enable_auto_tool_choice": false})
	content := files["start.sh"]
	file, err := parseShell([]byte(content), "original rank launchers")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	syntax.Walk(file, func(node syntax.Node) bool {
		r, ok := node.(*syntax.Redirect)
		if !ok || r.Hdoc == nil || r.Op != syntax.Hdoc {
			return true
		}
		body, _, quoted, err := bashHeredoc(r, []byte(content))
		if err != nil {
			t.Fatal(err)
		}
		if !quoted || !strings.HasPrefix(string(body), "#!/bin/bash\n") {
			return true
		}
		fragment := originalAssignments(t, string(body), func(a *syntax.Assign) bool { return a.Name.Value == "ARGS" && a.Array != nil && !a.Append })
		if fragment == "" {
			return true
		}
		arguments := fragmentArgv(t, "not_a_command() { exit 95; }\n"+fragment+"printf '%s\\0' \"${ARGS[@]}\"", nil)
		if got := flagValue(t, arguments, "--tool-call-parser"); got != literal {
			t.Fatalf("rank parser literal corrupted: %q", got)
		}
		for _, argument := range arguments {
			if argument == "--enable-auto-tool-choice" {
				t.Fatal("removed rank flag still consumed")
			}
		}
		count++
		return true
	})
	if count != 2 {
		t.Fatalf("exercised %d original rank arrays, want 2", count)
	}
}

func TestDeepSeekWorkerCacheEnvironmentPreservesLiteralLocalAndSharedPaths(t *testing.T) {
	cache := "/data/hf cache/'quoted' $HOME $(printf injected) `printf backtick`\\weights"
	selected := map[string]any{"worker_hf_cache": cache}
	manifest, files := compiledFragments(t, "deepseek-v4-flash-vision-exp-dspark-tp2", selected)
	assignments := strings.Split(strings.TrimSpace(originalAssignments(t, files["start-deepseek-v4-flash-dspark.sh"], func(assignment *syntax.Assign) bool {
		return assignment.Name.Value == "WORKER_HF_COMPOSE_ENV"
	})), "\n")
	if len(assignments) < 2 {
		t.Fatal("missing the reviewed local and NFS worker cache branches")
	}
	for _, tc := range []struct {
		name       string
		assignment string
		want       []string
	}{
		{"local", assignments[0], []string{cache, ""}},
		{"NFS", assignments[1], []string{"reviewed-nfs-volume", cache}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environment := append(boundEnvironment(t, manifest, selected), "NFS_VOLUME=reviewed-nfs-volume")
			// Interpret the original forwarded assignments as the remote shell
			// does, without connecting to a worker or mounting an NFS share.
			script := "set -eu\n" + tc.assignment + "\neval \"$WORKER_HF_COMPOSE_ENV\"\nprintf '%s\\0' \"$HF_CACHE\" \"${DSPARK_JIT_CACHE:-}\"\n"
			if got := fragmentArgv(t, script, environment); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("worker cache selection changed in the remote shell: got %q, want %q", got, tc.want)
			}
		})
	}
}
