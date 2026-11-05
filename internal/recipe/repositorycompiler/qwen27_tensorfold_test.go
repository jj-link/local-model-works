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

const qwen27TensorFoldFamily = "qwen38-27b-tensorfold-spark-tp1"

func qwen27TensorFoldSource(t *testing.T) recipe.RepositorySource {
	t.Helper()
	return recipe.RepositorySource{URL: QwenTensorFoldRepositoryURL, Path: ".", CommitSHA: "c11ff138871cf43d12898d79dbfa4dc952d17ca4", RepositoryID: repositoryID(t, QwenTensorFoldRepositoryURL)}
}

func qwen27TensorFoldFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", qwen27TensorFoldFamily, name))
}

func TestQwen27TensorFoldPinnedCompiler(t *testing.T) {
	source := qwen27TensorFoldSource(t)
	compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
	if !ok {
		t.Fatal("single-Spark Qwen3.8-27B TensorFold source is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, qwen27TensorFoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, qwen27TensorFoldFixture, nil)
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
	for _, name := range []string{"context", "parallel", "yarn_factor", "kv_pool_gb", "kv_dtype", "prefill_fp8", "vision", "model_id", "draft_id", "image", "hf_cache", "kernel_cache"} {
		parameter := manifest.ParameterByName(name)
		if parameter == nil || !parameter.Optional || parameter.Default != nil {
			t.Fatalf("%s must preserve its authored default until selected: %+v", name, parameter)
		}
	}
	for _, name := range []string{"port", "host", "container_name", "served_name", "model_revision"} {
		if manifest.ParameterByName(name) != nil {
			t.Fatalf("unsupported or coupled lifecycle setting exposed: %s", name)
		}
	}
	for _, selected := range []map[string]any{
		{},
		{"context": "131072", "hf_cache": "/data/shared HF cache", "kernel_cache": "/data/kernel cache"},
	} {
		settings, err := manifest.EffectiveSettings(selected)
		if err != nil {
			t.Fatal(err)
		}
		upstream, environment, edits, err := recipe.RenderInstallationUpstream(manifest, recipe.InstallationUpdateSpec{Workload: 0, Rank: 0, Parameters: settings})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(upstream.Install, [][]string{{"./scripts/prepare.sh"}}) || !reflect.DeepEqual(upstream.Start, []string{"./start.sh", "--max-tokens", "131072"}) || !reflect.DeepEqual(upstream.Stop, []string{"./stop.sh"}) ||
			!reflect.DeepEqual(upstream.Containers, []string{"qwen38-27b-tf"}) || len(upstream.ContainersByRank) != 0 ||
			upstream.EnvFile != ".env" || upstream.EnvFormat != "literal" || len(edits) != 0 || len(upstream.Configuration) != 0 {
			t.Fatal("rendering rewrote the original lifecycle or changed container observation")
		}
		if environment["HOST"] != "0.0.0.0" || environment["PORT"] != "8000" || environment["CONTAINER_NAME"] != "qwen38-27b-tf" || environment["SERVED_NAME"] != "Qwen3.8-27B" {
			t.Fatal("fixed endpoint, container identity or model alias changed")
		}
		for _, name := range []string{"context", "hf_cache", "kernel_cache"} {
			value, exists := environment[strings.ToUpper(name)]
			wanted, chosen := selected[name]
			if exists != chosen || chosen && value != fmt.Sprint(wanted) {
				t.Fatalf("%s lost the selected value or replaced an upstream default: %q", name, value)
			}
		}
		for _, name := range []string{"PARALLEL", "YARN_FACTOR", "KV_POOL_GB", "KV_DTYPE", "VISION", "MODEL_ID", "DRAFT_ID"} {
			if _, exists := environment[name]; exists {
				t.Fatalf("unselected native default overridden: %s", name)
			}
		}
	}
}

func TestQwen27TensorFoldRejectsDisconnectedLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, file, old, replacement string }{
		{"fixed observation renamed", "scripts/config.sh", `CONTAINER_NAME="${CONTAINER_NAME:-qwen38-27b-tf}"`, `CONTAINER_NAME="${CONTAINER_NAME:-unrelated}"`},
		{"forwarded serving arguments dropped", "start.sh", `SERVE_ARGS+=("$@")`, `SERVE_ARGS+=()`},
		{"configuration disconnected", "start.sh", "source ./scripts/config.sh", "echo ./scripts/config.sh"},
		{"serving removed", "start.sh", `tensorfold serve "$MODEL_ID" --host "$HOST" --port "$PORT" "${SERVE_ARGS[@]}" >/dev/null`, `echo serve "$MODEL_ID" --host "$HOST" --port "$PORT" "${SERVE_ARGS[@]}" >/dev/null`},
		{"serving array disconnected", "start.sh", `"${SERVE_ARGS[@]}" >/dev/null`, `"${UNRELATED[@]}" >/dev/null`},
		{"selected context disconnected", "start.sh", `--context "$CONTEXT"`, `--context 262144`},
		{"indexed serving argument overwritten", "start.sh", `docker run -d --name "$CONTAINER_NAME"`, "SERVE_ARGS[4]=--unrelated\ndocker run -d --name \"$CONTAINER_NAME\""},
		{"serving arguments unset", "start.sh", `docker run -d --name "$CONTAINER_NAME"`, "unset SERVE_ARGS\ndocker run -d --name \"$CONTAINER_NAME\""},
		{"listener port disconnected", "start.sh", `tensorfold serve "$MODEL_ID" --host "$HOST" --port "$PORT"`, `tensorfold serve "$MODEL_ID" --host "$HOST" --port 9999`},
		{"listener host disconnected", "start.sh", `tensorfold serve "$MODEL_ID" --host "$HOST"`, `tensorfold serve "$MODEL_ID" --host 127.0.0.1`},
		{"container disconnected", "start.sh", `docker run -d --name "$CONTAINER_NAME"`, `docker run -d --name unrelated`},
		{"stop disconnected", "stop.sh", `"$CONTAINER_NAME" >/dev/null`, `unrelated >/dev/null`},
		{"literal parser disconnected", "scripts/config.sh", `done < .env`, `done < /dev/null`},
		{"literal export removed", "scripts/config.sh", `export "$_key=$_value"`, `echo "$_key=$_value"`},
		{"endpoint overwritten", "scripts/config.sh", `PORT="${PORT:-8888}"`, `PORT=9999`},
		{"container overwritten", "scripts/config.sh", `CONTAINER_NAME="${CONTAINER_NAME:-qwen38-27b-tf}"`, `CONTAINER_NAME=unrelated`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := qwen27TensorFoldSource(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := qwen27TensorFoldFixture(name)
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

func TestTensorFoldServeArgumentsAllowlist(t *testing.T) {
	if err := verifyTensorFoldServeArguments("./start.sh", []string{"--max-tokens", "131072"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--context", "1"}, {"--max-tokens"}, {"--max-tokens", "0"}, {"--max-tokens", "$(id)"}} {
		var packed *recipe.PackError
		if err := verifyTensorFoldServeArguments("./start.sh", args); !errors.As(err, &packed) || packed.Code != "recipe.repository_layout_changed" {
			t.Fatalf("unreviewed serving argument accepted: %q: %v", args, err)
		}
	}
}
