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

const tensorfoldFamily = "glm53-flash-exl3-tensorfold-spark-tp2"

func tensorfoldSource(t *testing.T) recipe.RepositorySource {
	t.Helper()
	return recipe.RepositorySource{URL: GLM53TensorFoldRepositoryURL, Path: ".", CommitSHA: "22f55f5ff129cf5e48bf847b23af3d873705587f", RepositoryID: repositoryID(t, GLM53TensorFoldRepositoryURL)}
}

func tensorfoldFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", tensorfoldFamily, name))
}

func TestTensorFoldPinnedCompiler(t *testing.T) {
	source := tensorfoldSource(t)
	compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
	if !ok {
		t.Fatal("TensorFold repository is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, tensorfoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, tensorfoldFixture, nil)
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
		t.Fatal("maintained TensorFold binding no longer selects exactly two nodes")
	}
	workload := manifest.Workloads[0]
	worker2, clearsWorker2 := workload.Env["WORKER2"]
	if !reflect.DeepEqual(workload.Ranks, []int{0, 1}) || workload.Env["TP"] != "2" || !clearsWorker2 || worker2 != "" {
		t.Fatal("authored lifecycle can escape the two selected ranks")
	}
	upstream := workload.Upstream
	if upstream == nil || !reflect.DeepEqual(upstream.Start, []string{"./start.sh"}) || !reflect.DeepEqual(upstream.Stop, []string{"./stop.sh"}) ||
		upstream.CoordinatorRank == nil || *upstream.CoordinatorRank != 0 ||
		!reflect.DeepEqual(upstream.Containers, []string{"glm53-flash-tf"}) ||
		!reflect.DeepEqual(upstream.ContainersByRank, map[int][]string{1: {"glm53-flash-tf"}}) ||
		upstream.EnvFile != ".env" || upstream.EnvFormat != "literal" || len(upstream.Configuration) != 0 {
		t.Fatal("compilation changed source-owned entrypoints, rank observation, or original lifecycle bytes")
	}
	for _, name := range []string{"tp", "worker2", "fabric_peer2", "worker_hf_cache2", "worker_weights2", "nfs_server2"} {
		if manifest.ParameterByName(name) != nil {
			t.Fatalf("unselected rank control is configurable: %s", name)
		}
	}
	for _, name := range []string{"context", "image", "ghcr_image", "image_tag", "image_digest", "model_id", "model_revision"} {
		parameter := manifest.ParameterByName(name)
		if parameter == nil || !parameter.Optional || parameter.Default != nil || workload.Env[strings.ToUpper(name)] != "${setting."+name+"}" {
			t.Fatalf("source setting %s must preserve its native default until explicitly selected", name)
		}
	}
	settings, err := manifest.EffectiveSettings(map[string]any{"worker_ssh": "worker-selected", "context": "262144"})
	if err != nil || settings["context"] != "262144" {
		t.Fatalf("256k context cannot be selected through the original CONTEXT input: %v", err)
	}
}

func TestTensorFoldRejectsBrokenAuthoredLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, file, old, replacement string }{
		{"configuration disconnected", "start.sh", "source ./scripts/config.sh", "echo ./scripts/config.sh"},
		{"local serving disconnected", "start.sh", `"$IMAGE" "${rank0[@]}"`, `"$IMAGE" "${unrelated[@]}"`},
		{"local array not executed", "start.sh", `"${here_cmd[@]}" >/dev/null`, `"${unrelated[@]}" >/dev/null`},
		{"remote launch disconnected", "start.sh", `worker "$i" "mkdir -p \$HOME/.cache/tensorfold-glm53 &&$remote"`, `echo "$i" "mkdir -p \$HOME/.cache/tensorfold-glm53 &&$remote"`},
		{"remote array not executed", "start.sh", `"${worker_cmd[@]}"`, `"${unrelated[@]}"`},
		{"remote serialization removed", "start.sh", `printf '%q' "$a"`, `echo "$a"`},
		{"worker transport removed", "scripts/nodes.sh", `  ssh -o BatchMode=yes`, `  echo -o BatchMode=yes`},
		{"worker selector not consumed", "scripts/nodes.sh", `local i=$1; shift`, `local i=$1`},
		{"local teardown disconnected", "stop.sh", `stop_here() {`, `unused_stop_here() {`},
		{"remote teardown removed", "stop.sh", `worker "$1" "docker`, `echo "$1" "docker`},
		{"bound topology overwritten", "scripts/config.sh", `TP="${TP:-2}"`, `TP=3`},
		{"unselected worker forced", "scripts/config.sh", `WORKER2="${WORKER2:-}"`, `WORKER2=unselected`},
		{"literal file no longer read", "scripts/config.sh", `done < "$_cfg_root/.env"`, `done < /dev/null`},
		{"literal parser no longer exports", "scripts/config.sh", `export "$_key=$_value"`, `echo "$_key=$_value"`},
		{"bound endpoint overwritten", "scripts/config.sh", `PORT="${PORT:-8888}"`, `PORT=9999`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := tensorfoldSource(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := tensorfoldFixture(name)
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
				t.Fatalf("broken authored lifecycle was not rejected: %v", err)
			}
		})
	}
}
