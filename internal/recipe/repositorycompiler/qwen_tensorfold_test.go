package repositorycompiler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

const qwenTensorFoldFamily = "qwen38-flash-next-tensorfold-spark-tp1"

func qwenTensorFoldSource(t *testing.T) recipe.RepositorySource {
	t.Helper()
	return recipe.RepositorySource{URL: QwenFlashNextTensorFoldRepositoryURL, Path: ".", CommitSHA: "20e15e41f2f514a0d287710f4665ee1b5009ba41", RepositoryID: repositoryID(t, QwenFlashNextTensorFoldRepositoryURL)}
}

func qwenTensorFoldFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", qwenTensorFoldFamily, name))
}

func TestQwenTensorFoldPinnedCompiler(t *testing.T) {
	source := qwenTensorFoldSource(t)
	compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
	if !ok {
		t.Fatal("single-Spark TensorFold source is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, qwenTensorFoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, qwenTensorFoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.ManifestDigest != second.ManifestDigest || string(first.ConfigJSON) != string(second.ConfigJSON) {
		t.Fatal("pinned source compilation is not deterministic")
	}
	manifest, err := recipe.Parse(first.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metadata.Source.Revision != source.CommitSHA || manifest.Compatibility.NodeCount != 1 || len(manifest.Workloads) != 1 || !reflect.DeepEqual(manifest.Workloads[0].Ranks, []int{0}) {
		t.Fatal("compiled source identity or single-node placement changed")
	}
	for _, name := range []string{"context", "parallel", "kv_dtype", "checkpoint", "vision", "model_id", "model_revision", "image", "tf_ref", "hf_cache", "kernel_cache"} {
		parameter := manifest.ParameterByName(name)
		if parameter == nil || !parameter.Optional || parameter.Default != nil {
			t.Fatalf("%s must preserve its authored default until selected: %+v", name, parameter)
		}
	}
	for _, name := range []string{"port", "host", "container_name", "served_name"} {
		if manifest.ParameterByName(name) != nil {
			t.Fatalf("unsupported or coupled lifecycle setting exposed: %s", name)
		}
	}
	if w := manifest.Workloads[0]; w.Env["PORT"] != recipe.TemplNodePort || len(w.Ports) != 1 || w.Ports[0].Host != 8000 || w.Readiness.HTTPGet.Port != 8000 {
		t.Fatal("serving port must follow the placement with 8000 as the unlabelled fallback")
	}
	for _, selected := range []map[string]any{
		{},
		{"context": 131072, "hf_cache": "/data/shared HF cache", "kernel_cache": "/data/kernel cache"},
	} {
		settings, err := manifest.EffectiveSettings(selected)
		if err != nil {
			t.Fatal(err)
		}
		upstream, environment, edits, err := recipe.RenderInstallationUpstream(manifest, recipe.InstallationUpdateSpec{Workload: 0, Rank: 0, Parameters: settings, Bindings: map[string]string{recipe.TemplNodePort: "8001"}})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(upstream.Install, [][]string{{"./scripts/prepare.sh"}}) || !reflect.DeepEqual(upstream.Start, []string{"./start.sh"}) || !reflect.DeepEqual(upstream.Stop, []string{"./stop.sh"}) ||
			!reflect.DeepEqual(upstream.Containers, []string{"qwen38-flash-next-tf"}) || len(upstream.ContainersByRank) != 0 ||
			upstream.EnvFile != ".env" || upstream.EnvFormat != "literal" || len(edits) != 0 || len(upstream.Configuration) != 0 {
			t.Fatal("rendering rewrote the original lifecycle or changed container observation")
		}
		if environment["HOST"] != "0.0.0.0" || environment["PORT"] != "8001" || environment["CONTAINER_NAME"] != "qwen38-flash-next-tf" || environment["SERVED_NAME"] != "Qwen3.8-Flash-Next" {
			t.Fatal("fixed endpoint, container identity or model alias changed")
		}
		for _, name := range []string{"context", "hf_cache", "kernel_cache"} {
			value, exists := environment[strings.ToUpper(name)]
			wanted, chosen := selected[name]
			if exists != chosen || chosen && value != fmt.Sprint(wanted) {
				t.Fatalf("%s lost the selected value or replaced an upstream default: %q", name, value)
			}
		}
		for _, name := range []string{"PARALLEL", "KV_DTYPE", "CHECKPOINT", "VISION", "MODEL_ID"} {
			if _, exists := environment[name]; exists {
				t.Fatalf("unselected native default overridden: %s", name)
			}
		}
	}
}

func TestQwenTensorFoldRejectsDisconnectedLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, file, old, replacement string }{
		{"fixed observation renamed", "scripts/config.sh", `CONTAINER_NAME="${CONTAINER_NAME:-qwen38-flash-next-tf}"`, `CONTAINER_NAME="${CONTAINER_NAME:-unrelated}"`},
		{"configuration disconnected", "start.sh", "source ./scripts/config.sh", "echo ./scripts/config.sh"},
		{"serving removed", "start.sh", `tensorfold-native serve "$MODEL_ARG" --name "$SERVED_NAME"`, `echo serve "$MODEL_ARG" --name "$SERVED_NAME"`},
		{"launch array not run", "start.sh", "\"${here_cmd[@]}\" >/dev/null", "echo \"${here_cmd[@]}\" >/dev/null"},
		{"launch array overwritten", "start.sh", "\"${here_cmd[@]}\" >/dev/null", "here_cmd=(docker run unrelated)\n\"${here_cmd[@]}\" >/dev/null"},
		{"served folder disconnected", "start.sh", `MODEL_ARG="/root/.cache/huggingface/$sub"`, `MODEL_ARG="/root/.cache/huggingface/$UNRELATED"`},
		{"snapshot lookup disconnected", "start.sh", `sub=$(model_sub)`, `sub=$(unrelated)`},
		{"model alias disconnected", "start.sh", `serve "$MODEL_ARG" --name "$SERVED_NAME" --host`, `serve "$MODEL_ARG" --name unrelated --host`},
		{"forwarded arguments replaced", "start.sh", `USER_ARGS=("$@")`, `USER_ARGS=(--unrelated)`},
		{"serving array disconnected", "start.sh", `"${SERVE_ARGS[@]}" "${USER_ARGS[@]}")`, `"${UNRELATED[@]}" "${USER_ARGS[@]}")`},
		{"selected context disconnected", "start.sh", `--context "$CONTEXT"`, `--context 262144`},
		{"indexed serving argument overwritten", "start.sh", `here_cmd=(docker run -d --name "$CONTAINER_NAME"`, "SERVE_ARGS[4]=--unrelated\nhere_cmd=(docker run -d --name \"$CONTAINER_NAME\""},
		{"serving arguments unset", "start.sh", `here_cmd=(docker run -d --name "$CONTAINER_NAME"`, "unset SERVE_ARGS\nhere_cmd=(docker run -d --name \"$CONTAINER_NAME\""},
		{"listener port disconnected", "start.sh", `--host "$HOST" --port "$PORT"
          "${SERVE_ARGS[@]}"`, `--host "$HOST" --port 9999
          "${SERVE_ARGS[@]}"`},
		{"listener host disconnected", "start.sh", `--name "$SERVED_NAME" --host "$HOST" --port "$PORT"
          "${SERVE_ARGS[@]}"`, `--name "$SERVED_NAME" --host 127.0.0.1 --port "$PORT"
          "${SERVE_ARGS[@]}"`},
		{"container disconnected", "start.sh", `here_cmd=(docker run -d --name "$CONTAINER_NAME"`, `here_cmd=(docker run -d --name unrelated`},
		{"stop disconnected", "stop.sh", `"$CONTAINER_NAME" >/dev/null`, `unrelated >/dev/null`},
		{"literal parser disconnected", "scripts/config.sh", `done < .env`, `done < /dev/null`},
		{"literal export removed", "scripts/config.sh", `export "$_key=$_value"`, `echo "$_key=$_value"`},
		{"endpoint overwritten", "scripts/config.sh", `PORT="${PORT:-8888}"`, `PORT=9999`},
		{"container overwritten", "scripts/config.sh", `CONTAINER_NAME="${CONTAINER_NAME:-qwen38-flash-next-tf}"`, `CONTAINER_NAME=unrelated`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := qwenTensorFoldSource(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := qwenTensorFoldFixture(name)
				if err == nil && name == tc.file {
					if !strings.Contains(string(content), tc.old) {
						t.Fatalf("fixture lacks mutation target %q", tc.old)
					}
					content = []byte(strings.ReplaceAll(string(content), tc.old, tc.replacement))
				}
				return content, err
			}
			_, err := compiler.CompileRetained(context.Background(), source, reader, nil)
			var packed *recipe.PackError
			if !errors.As(err, &packed) || packed.Code != "recipe.repository_layout_changed" {
				t.Fatalf("disconnected lifecycle accepted: %v", err)
			}
		})
	}
}
