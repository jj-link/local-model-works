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

const dsv41TensorFoldFamily = "dsv41-flash-1017-exl3-tensorfold-spark-tp2"

func dsv41TensorFoldSource(t *testing.T) recipe.RepositorySource {
	t.Helper()
	return recipe.RepositorySource{URL: DSV41TensorFoldRepositoryURL, Path: ".", CommitSHA: "31457d05f8917301bbf609c3d12c7f364af8a4a6", RepositoryID: repositoryID(t, DSV41TensorFoldRepositoryURL)}
}

func dsv41TensorFoldFixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", dsv41TensorFoldFamily, name))
}

func TestDSV41TensorFoldPinnedCompiler(t *testing.T) {
	source := dsv41TensorFoldSource(t)
	compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
	if !ok {
		t.Fatal("DeepSeek TensorFold repository is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, dsv41TensorFoldFixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, dsv41TensorFoldFixture, nil)
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
		t.Fatal("maintained DeepSeek binding no longer selects exactly two nodes")
	}
	workload := manifest.Workloads[0]
	upstream := workload.Upstream
	if !reflect.DeepEqual(workload.Ranks, []int{0, 1}) || upstream == nil ||
		!reflect.DeepEqual(upstream.Start, []string{"./start.sh"}) || !reflect.DeepEqual(upstream.Stop, []string{"./stop.sh"}) ||
		upstream.CoordinatorRank == nil || *upstream.CoordinatorRank != 0 ||
		!reflect.DeepEqual(upstream.Containers, []string{"dsv41-1017-exl3-tf"}) ||
		!reflect.DeepEqual(upstream.ContainersByRank, map[int][]string{1: {"dsv41-1017-exl3-tf"}}) ||
		upstream.EnvFile != ".env" || upstream.EnvFormat != "literal" {
		t.Fatal("compilation changed source-owned entrypoints or rank observation")
	}
	for _, name := range []string{"context", "max_tokens", "model_id", "model_revision"} {
		parameter := manifest.ParameterByName(name)
		if parameter == nil || !parameter.Optional || parameter.Default != nil || workload.Env[strings.ToUpper(name)] != "${setting."+name+"}" {
			t.Fatalf("source setting %s must preserve its native default until explicitly selected", name)
		}
	}
}

func TestDSV41TensorFoldRejectsBrokenAuthoredLifecycle(t *testing.T) {
	for _, tc := range []struct{ name, file, old, replacement string }{
		{"configuration disconnected", "start.sh", "source ./scripts/config.sh", "echo ./scripts/config.sh"},
		{"local array not executed", "start.sh", `"${here[@]}" >/dev/null`, `"${unrelated[@]}" >/dev/null`},
		{"local serving removed", "start.sh", `"$IMAGE" "${SERVE[@]}")`, `"$IMAGE" "${UNRELATED[@]}")`},
		{"remote launch disconnected", "start.sh", `worker 1 "$remote" >/dev/null`, `echo 1 "$remote" >/dev/null`},
		{"remote command string replaced", "start.sh", `remote="mkdir -p`, `remote=$(echo); unused="mkdir -p`},
		{"remote follower replaced", "start.sh", `cmd="tf-dsv41-lanes `, `cmd="true `},
		{"worker transport removed", "scripts/nodes.sh", `  ssh -o BatchMode=yes`, `  echo -o BatchMode=yes`},
		{"local teardown disconnected", "stop.sh", `stop_here() {`, `unused_stop_here() {`},
		{"remote teardown removed", "stop.sh", `worker "$1" "docker`, `echo "$1" "docker`},
		{"literal file no longer read", "scripts/config.sh", `done < "$_cfg_root/.env"`, `done < /dev/null`},
		{"bound endpoint overwritten", "scripts/config.sh", `PORT="${PORT:-8888}"`, `PORT=9999`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := dsv41TensorFoldSource(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := dsv41TensorFoldFixture(name)
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
