package repositorycompiler

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
	"mvdan.cc/sh/v3/syntax"
)

func TestAdditionalArgumentsReachEveryOriginalLauncher(t *testing.T) {
	for _, tc := range []struct {
		family    string
		parameter string
		launchers int
	}{
		{"qwen38-27b-dgx-spark-mtp", "extra_args", 1},
		{"qwen38-flash-next-spark-tp1", "extra_vllm_args", 1},
		{"qwen38-flash-next-dspark-tp2", "extra_vllm_args", 2},
		{"glm53-flash-exl3-dflash2-spark-tp2", "extra_args", 2},
	} {
		t.Run(tc.family, func(t *testing.T) {
			manifest := configurationTemplate(t, tc.family)
			reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", tc.family, name)) }
			if err := compileConfiguration(manifest, reader); err != nil {
				t.Fatal(err)
			}
			configuration := manifest.Workloads[0].Upstream.Configuration[0]
			var edits []sourceconfig.Edit
			for _, edit := range configuration.Edits {
				if edit.Parameter == tc.parameter {
					edits = append(edits, edit)
				}
			}
			configuration.Edits = edits
			// Newlines matching both real delimiter names must remain one argv
			// value, not terminate the outer heredoc or introduce a command.
			want := []string{"--literal-test", "it's $(not executed) `also not`\nEOF\nLAUNCH_EOF\r\nα"}
			encoded, _ := json.Marshal(want)
			resolved, err := sourceconfig.Resolve([]sourceconfig.File{configuration}, map[string]any{tc.parameter: string(encoded)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			original, err := reader(configuration.Path)
			if err != nil {
				t.Fatal(err)
			}
			patched := string(original)
			for i := len(resolved[0].Edits) - 1; i >= 0; i-- {
				edit := resolved[0].Edits[i]
				if configuration.Edits[i].Format != "argv" && strings.ContainsAny(edit.Replacement, "\r\n") {
					t.Fatal("heredoc replacement can introduce a delimiter line")
				}
				patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
			}
			before := captureLauncherArgv(t, original)
			after := captureLauncherArgv(t, []byte(patched))
			if len(before) != tc.launchers || len(after) != tc.launchers {
				t.Fatalf("captured %d original and %d configured launchers, want %d", len(before), len(after), tc.launchers)
			}
			for launcher, arguments := range after {
				found := -1
				for i := 0; i+len(want) <= len(arguments); i++ {
					if reflect.DeepEqual(arguments[i:i+len(want)], want) {
						if found >= 0 {
							t.Fatalf("launcher %d received literal arguments more than once", launcher)
						}
						found = i
					}
				}
				if found < 0 {
					t.Fatalf("launcher %d did not receive literal arguments: %q", launcher, arguments)
				}
				inherited := append(append([]string(nil), arguments[:found]...), arguments[found+len(want):]...)
				if !reflect.DeepEqual(inherited, before[launcher]) {
					t.Fatalf("launcher %d changed inherited argv:\ngot  %#v\nwant %#v", launcher, inherited, before[launcher])
				}
			}
		})
	}
}

func TestReviewedAcceleratorsReachOriginalDockerSGLangLaunch(t *testing.T) {
	for _, tc := range []struct {
		family       string
		accelerators string
	}{
		{"qwen38-27b-rtx6000pro-dflash2", "GPU-6ac06ab6-ac30-f3ad-b246-064a8e6f1309"},
		{"qwen38-27b-dgx-spark-mtp", "GPU-e665a1dd-c279-05ca-a396-e7b95d783790,GPU-6ac06ab6-ac30-f3ad-b246-064a8e6f1309"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			manifest := configurationTemplate(t, tc.family)
			reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", tc.family, name)) }
			if err := compileConfiguration(manifest, reader); err != nil {
				t.Fatal(err)
			}
			configuration := manifest.Workloads[0].Upstream.Configuration[0]
			original, err := reader(configuration.Path)
			if err != nil {
				t.Fatal(err)
			}
			// User settings cannot supply the placement-owned template value.
			settings := map[string]any{"node.accelerators": "GPU-not-selected"}
			resolved, err := sourceconfig.Resolve([]sourceconfig.File{configuration}, settings, func(template string) (string, error) {
				return strings.ReplaceAll(template, "${node.accelerators}", tc.accelerators), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			patched := string(original)
			for i := len(resolved[0].Edits) - 1; i >= 0; i-- {
				edit := resolved[0].Edits[i]
				patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
			}
			before := captureDirectSGLangLaunch(t, original)
			after := captureDirectSGLangLaunch(t, []byte(patched))
			image := -1
			for i, argument := range before {
				if argument == "fixture/image:reviewed" {
					image = i
					break
				}
			}
			if image < 0 {
				t.Fatal("original image did not reach Docker")
			}
			// Exact argv comparison protects all engine flags, mounts and Docker
			// options. The one new argument must be last among Docker options,
			// after even inherited array-provided CUDA environment overrides.
			want := append([]string(nil), before[:image]...)
			want = append(want, "--env=CUDA_VISIBLE_DEVICES="+tc.accelerators)
			want = append(want, before[image:]...)
			if !reflect.DeepEqual(after, want) {
				t.Fatalf("reviewed GPU placement changed original argv:\ngot  %#v\nwant %#v", after, want)
			}
		})
	}
}

func TestQwenPortReachesOriginalListenerAndReadiness(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is required to exercise the original listener and readiness")
	}
	family := "qwen38-27b-rtx6000pro-dflash2"
	manifest := configurationTemplate(t, family)
	reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", family, name)) }
	if err := compileConfiguration(manifest, reader); err != nil {
		t.Fatal(err)
	}
	resolved, err := sourceconfig.Resolve(manifest.Workloads[0].Upstream.Configuration, nil, func(template string) (string, error) {
		return strings.ReplaceAll(template, "${node.accelerators}", "GPU-reviewed"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	original, err := reader("start.sh")
	if err != nil {
		t.Fatal(err)
	}
	patched := string(original)
	for i := len(resolved[0].Edits) - 1; i >= 0; i-- {
		edit := resolved[0].Edits[i]
		patched = patched[:edit.Start] + edit.Replacement + patched[edit.End:]
	}
	file, err := parseShell([]byte(patched), "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	var script strings.Builder
	script.WriteString("docker() { printf '%s\\0' \"$@\"; }\n")
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok {
			continue
		}
		for _, assignment := range call.Assigns {
			if assignment.Name != nil && (assignment.Name.Value == "PORT" || assignment.Name.Value == "READY_URL") {
				script.WriteString(patched[assignment.Pos().Offset():assignment.End().Offset()] + "\n")
			}
		}
		if len(call.Args) > 1 && call.Args[0].Lit() == "docker" && call.Args[1].Lit() == "run" {
			script.WriteString(patched[call.Pos().Offset():call.End().Offset()] + "\n")
		}
	}
	script.WriteString("printf 'readiness=%s\\0' \"$READY_URL\"\n")
	output, err := exec.Command(bash, "-c", script.String()).CombinedOutput()
	if err != nil {
		t.Fatalf("execute original endpoint expressions: %v\n%s", err, output)
	}
	args := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	port := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--port" {
			port = args[i+1]
		}
	}
	if port != "8000" || args[len(args)-1] != "readiness=http://127.0.0.1:8000/v1/models" {
		t.Fatalf("listener and readiness did not move together to 8000: %q", args)
	}
}

func captureDirectSGLangLaunch(t *testing.T, source []byte) []string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is required to exercise the original Docker invocation")
	}
	file, err := parseShell(source, "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	capture := filepath.Join(directory, "argv")
	if err := os.WriteFile(filepath.Join(directory, "docker"), []byte("#!/bin/bash\nprintf '%s\\0' \"$@\" > \"$LMW_ARGV_CAPTURE\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 || call.Args[0].Lit() != "docker" || call.Args[1].Lit() != "run" {
			continue
		}
		// Execute only the original direct invocation, never upstream host
		// preparation, package installation, readiness polling or teardown.
		setup := "DOCKER_ENV_ARGS=(-e CUDA_VISIBLE_DEVICES=GPU-old); ALLOW_LONGER_ARGS=(--env=CUDA_VISIBLE_DEVICES=GPU-later); "
		command := exec.Command(bash, "-c", setup+string(source[stmt.Pos().Offset():stmt.End().Offset()]))
		command.Env = append(os.Environ(),
			"PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "LMW_ARGV_CAPTURE="+capture,
			"IMAGE=fixture/image:reviewed", "MODEL_ID=fixture/model", "SERVED_MODEL_NAME=reviewed model",
			"HF_HOME=/fixture/it's a cache", "TRITON_CACHE_DIR=/fixture/triton cache", "PATCH_DIR=/fixture/patches",
		)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("execute original Docker invocation: %v\n%s", err, output)
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		var arguments []string
		for _, argument := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
			arguments = append(arguments, string(argument))
		}
		return arguments
	}
	t.Fatal("original direct Docker launch was not found")
	return nil
}

func TestExtraArgumentsDoNotUseRawEnvironmentOrDeadDockerArray(t *testing.T) {
	for _, family := range []string{"qwen38-27b-dgx-spark-mtp", "qwen38-flash-next-spark-tp1", "qwen38-flash-next-dspark-tp2", "glm53-flash-exl3-dflash2-spark-tp2"} {
		manifest := configurationTemplate(t, family)
		if err := compileConfiguration(manifest, func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join("testdata", family, name))
		}); err != nil {
			t.Fatal(err)
		}
		for key := range manifest.Workloads[0].Env {
			if extraEnvironment(key) {
				t.Fatalf("%s passes raw %s through an upstream string parser", family, key)
			}
		}
		base := make(map[string]any)
		for _, parameter := range manifest.Parameters {
			if parameter.Default == nil && !parameter.Optional {
				base[parameter.Name] = "fixture"
			}
		}
		for _, parameter := range manifest.Parameters {
			if family == "qwen38-flash-next-dspark-tp2" && parameter.Name == "extra_docker_args" {
				t.Fatal("dead upstream DOCKER_ARGS array exposed as a live option")
			}
			if parameter.Group == "Additional arguments" {
				valid := `["--context-length","131072"]`
				if manifest.Metadata.Engine == "vllm" {
					valid = `["--max-model-len","131072"]`
				}
				invalid := []string{`["--port=9999"]`, `["--tensor_parallel_size=7"]`}
				if parameter.Name == "docker_env" || parameter.Name == "extra_docker_args" {
					valid = `["--env=VLLM_USE_V2_MODEL_RUNNER=1"]`
					invalid = []string{`["--network=bridge"]`, `["--env=HF_HOME=/wrong-cache"]`, `["--env=CUDA_VISIBLE_DEVICES=0"]`, `["--env=NVIDIA_VISIBLE_DEVICES=all"]`, `["--privileged=true","replacement-image"]`, `["--env-file=/tmp/overrides"]`}
				}
				base[parameter.Name] = valid
				if _, err := manifest.EffectiveSettings(base); err != nil {
					t.Fatalf("%s rejected ordinary literal argv: %v", parameter.Name, err)
				}
				for _, value := range invalid {
					base[parameter.Name] = value
					if _, err := manifest.EffectiveSettings(base); err == nil {
						t.Fatalf("%s accepted an uncoupled override: %s", parameter.Name, value)
					}
				}
				delete(base, parameter.Name)
			}
		}
	}
}

// Exercise only launch invocations, not upstream installation or model startup.
// Generating each heredoc still checks that literal values survive both shell
// layers, including launch tails appended separately from the Bash shebang.
func captureLauncherArgv(t *testing.T, source []byte) [][]string {
	t.Helper()
	file, err := parseShell(source, "start.sh")
	if err != nil {
		t.Fatal(err)
	}
	launchers := captureExecutedArgv(t, source, file)
	fragments, err := generatedBashHeredocs(file, source)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range fragments {
		delimiter, literal := constantWord(fragment.redirect.Word)
		if !literal {
			t.Fatal("nonliteral heredoc delimiter")
		}
		header := source[fragment.redirect.Word.Pos().Offset():fragment.redirect.Word.End().Offset()]
		generate := exec.Command("bash", "-c", "cat <<"+string(header)+"\n"+string(fragment.body)+delimiter+"\n")
		generated, err := generate.Output()
		if err != nil {
			t.Fatalf("evaluate original heredoc: %v", err)
		}
		inner, err := parseShell(generated, "generated Bash launcher")
		if err != nil {
			t.Fatal(err)
		}
		launchers = append(launchers, captureExecutedArgv(t, generated, inner)...)
	}
	return launchers
}

func captureExecutedArgv(t *testing.T, source []byte, file *syntax.File) [][]string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash is required to exercise the original launchers")
	}
	directory := t.TempDir()
	capture := filepath.Join(directory, "argv")
	for _, name := range []string{"docker", "vllm"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/bash\nprintf '%s\\0' \"$@\" > \"$LMW_ARGV_CAPTURE\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var launchers [][]string
	for _, stmt := range file.Stmts {
		call, ok := stmt.Cmd.(*syntax.CallExpr)
		if !ok || len(call.Args) < 2 {
			continue
		}
		docker := call.Args[0].Lit() == "docker" && call.Args[1].Lit() == "run"
		vllm := len(call.Args) > 2 && call.Args[0].Lit() == "exec" && call.Args[1].Lit() == "vllm" && call.Args[2].Lit() == "serve"
		if !docker && !vllm {
			continue
		}
		// Populate inherited arrays and positional parameters without running
		// the upstream setup. The before/after comparison protects these values
		// along with every original literal and model positional argument.
		setup := "ARGS=(--inherited 'value with spaces' --cudagraph-capture-sizes 1 2 4); EXTRA_ARGS_ARR=(--inherited 'value with spaces'); set -- 'inherited positional' '$(not evaluated)'; "
		command := exec.Command(bash, "-c", setup+string(source[stmt.Pos().Offset():stmt.End().Offset()]))
		command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "LMW_ARGV_CAPTURE="+capture, "MODEL_DIR=/fixture/model")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("execute original launch invocation: %v\n%s", err, output)
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		var arguments []string
		for _, argument := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
			arguments = append(arguments, string(argument))
		}
		launchers = append(launchers, arguments)
	}
	return launchers
}
