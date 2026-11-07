package repositorycompiler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

const glm1104TensorFoldFamily = "glm53-flash-1104-tensorfold-spark-tp2"

func glm1104TensorFoldSource(t *testing.T) recipe.RepositorySource {
	t.Helper()
	return recipe.RepositorySource{URL: GLM1104TensorFoldRepositoryURL, Path: ".", CommitSHA: "e9153a4fc40c6dc088692550c8f9f939fa8fbdf0", RepositoryID: repositoryID(t, GLM1104TensorFoldRepositoryURL)}
}

func glm1104TensorFoldFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", glm1104TensorFoldFamily, name))
}

func TestGLM1104TensorFoldPinnedCompiler(t *testing.T) {
	source := glm1104TensorFoldSource(t)
	compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
	if !ok {
		t.Fatal("GLM 1104 TensorFold repository is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, glm1104TensorFoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, glm1104TensorFoldFixture, nil)
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
	if manifest.Compatibility.NodeCount != 2 || len(manifest.Workloads) != 1 {
		t.Fatal("maintained GLM 1104 binding no longer selects exactly two nodes")
	}
	workload := manifest.Workloads[0]
	upstream := workload.Upstream
	if !reflect.DeepEqual(workload.Ranks, []int{0, 1}) || upstream == nil ||
		!reflect.DeepEqual(upstream.Start, []string{"./start.sh"}) || !reflect.DeepEqual(upstream.Stop, []string{"./stop.sh"}) ||
		upstream.CoordinatorRank == nil || *upstream.CoordinatorRank != 0 ||
		!reflect.DeepEqual(upstream.Containers, []string{"glm53-spark"}) ||
		!reflect.DeepEqual(upstream.ContainersByRank, map[int][]string{1: {"glm53-spark"}}) ||
		upstream.EnvFile != ".env" || upstream.EnvFormat != "literal" {
		t.Fatal("compilation changed source-owned entrypoints or rank observation")
	}
	for name, want := range map[string]any{"context": "auto", "worker_weights": "rsync", "parallel": float64(4)} {
		parameter := manifest.ParameterByName(name)
		if parameter == nil || parameter.Default != want || workload.Env[strings.ToUpper(name)] != "${setting."+name+"}" {
			t.Fatalf("template setting %s must carry upstream's default %v without its comment: %#v", name, want, parameter)
		}
	}
	if parameter := manifest.ParameterByName("host_cache_gib"); parameter == nil || parameter.Type != "float" || parameter.Default != float64(0) {
		t.Fatalf("commented numeric template default was not read as a number: %#v", parameter)
	}
}

func TestGLM1104TensorFoldRejectsBrokenAuthoredLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		edits      [][2]string
	}{
		{"configuration disconnected", "scripts/prepare.sh", [][2]string{{`source "$(dirname -- "${BASH_SOURCE[0]}")/config.sh"`, `echo config.sh`}}},
		{"local array not executed", "start.sh", [][2]string{{`"${cmd[@]}" >/dev/null ||`, `"${unrelated[@]}" >/dev/null ||`}}},
		{"local serving removed", "start.sh", [][2]string{{`    --entrypoint "$TF_BIN" "$IMAGE" serve /model`, `    --entrypoint "$TF_BIN" "$IMAGE" idle /model`}}},
		{"remote launch disconnected", "start.sh", [][2]string{{`wssh "${wcmd[@]}" >/dev/null`, `echo "${wcmd[@]}" >/dev/null`}}},
		{"remote serving removed", "start.sh", [][2]string{{`wcmd+=(--entrypoint "$TF_BIN" "$IMAGE" serve`, `wcmd+=(--entrypoint "$TF_BIN" "$IMAGE" idle`}}},
		{"worker transport removed", "scripts/nodes.sh", [][2]string{{`ssh $SSH_OPTS -- "$WORKER" "$remote"`, `echo $SSH_OPTS -- "$WORKER" "$remote"`}}},
		{"remote serialization removed", "scripts/nodes.sh", [][2]string{{`remote+=" $(printf '%q' "$a")"`, `remote+=" $a"`}}},
		{"local teardown disconnected", "stop.sh", [][2]string{
			{`  docker stop -t "$STOP_TIMEOUT"`, `  echo stop -t "$STOP_TIMEOUT"`},
			{`    docker kill "$CONTAINER_NAME"`, `    echo kill "$CONTAINER_NAME"`},
			{`|| docker rm "$CONTAINER_NAME" >/dev/null || fail`, `|| echo rm "$CONTAINER_NAME" >/dev/null || fail`},
		}},
		{"remote teardown removed", "stop.sh", [][2]string{{`wssh docker`, `echo docker`}}},
		{"literal file no longer read", "scripts/config.sh", [][2]string{{`done < "$REPO_ROOT/.env"`, `done < /dev/null`}}},
		{"bound endpoint overwritten", "scripts/config.sh", [][2]string{{`PORT="${PORT:-8020}"`, `PORT=9999`}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := glm1104TensorFoldSource(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := glm1104TensorFoldFixture(name)
				if err == nil && name == tc.file {
					for _, edit := range tc.edits {
						if !strings.Contains(string(content), edit[0]) {
							t.Fatalf("fixture lacks mutation target %q", edit[0])
						}
						content = []byte(strings.ReplaceAll(string(content), edit[0], edit[1]))
					}
				}
				return content, err
			}
			_, err := compiler.CompileRetained(context.Background(), source, reader, nil)
			var packed *recipe.PackError
			if !errors.As(err, &packed) || packed.Code != "recipe.repository_layout_changed" {
				t.Fatalf("broken authored lifecycle was not rejected: %v", err)
			}
		})
	}
}
