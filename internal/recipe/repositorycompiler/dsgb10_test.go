package repositorycompiler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

const dsgb10Family = "deepseek-v41-flash-dsgb10-spark-tp4"

func dsgb10Source(t *testing.T) recipe.RepositorySource {
	t.Helper()
	id, _, _, err := recipe.RepositoryIdentity(recipe.Source{URL: DSGB10RepositoryURL, Path: DSGB10SourcePath})
	if err != nil {
		t.Fatal(err)
	}
	return recipe.RepositorySource{URL: DSGB10RepositoryURL, Path: DSGB10SourcePath, CommitSHA: "f2117916f2a4cac0f79fa41680ebd39bf5d42b51", RepositoryID: id}
}

// The fixture mirrors the pinned source directory; LICENSE and NOTICE come
// from the repository root.
func dsgb10Fixture(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", dsgb10Family, name))
}

func TestDSGB10PinnedCompiler(t *testing.T) {
	source := dsgb10Source(t)
	registry := NewRegistry(mustValidator(t))
	if _, ok := registry.LookupUpstream(recipe.RepositorySource{URL: DSGB10RepositoryURL, Path: ".", RepositoryID: repositoryID(t, DSGB10RepositoryURL)}); ok {
		t.Fatal("the repository root must not select the four-Spark procedure")
	}
	compiler, ok := registry.LookupUpstream(source)
	if !ok {
		t.Fatal("pico-dgx-ds4 four-Spark procedure is unsupported")
	}
	first, err := compiler.CompileRetained(context.Background(), source, dsgb10Fixture, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileRetained(context.Background(), source, dsgb10Fixture, nil)
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
	if manifest.Compatibility.NodeCount != 4 || len(manifest.Workloads) != 1 || manifest.Metadata.Source.Path != DSGB10SourcePath ||
		manifest.Compatibility.Fabric == nil || manifest.Compatibility.Fabric.Transport != "roce" || !manifest.Compatibility.Fabric.SharedGIDIndex {
		t.Fatalf("four-Spark switched binding changed: %+v", manifest.Compatibility)
	}
	workload := manifest.Workloads[0]
	upstream := workload.Upstream
	if !reflect.DeepEqual(workload.Ranks, []int{0, 1, 2, 3}) || upstream == nil || upstream.CoordinatorRank == nil || *upstream.CoordinatorRank != 0 ||
		!reflect.DeepEqual(upstream.Start, []string{"./engine/server/service.sh", "start"}) ||
		!reflect.DeepEqual(upstream.Stop, []string{"./engine/server/service.sh", "stop"}) ||
		!reflect.DeepEqual(upstream.Status, []string{"./engine/server/service.sh", "status"}) ||
		upstream.Containers == nil || len(upstream.Containers) != 0 || len(upstream.ContainersByRank) != 0 {
		t.Fatalf("compilation changed source-owned entrypoints or observation: %+v", upstream)
	}
	for rank := 1; rank < 4; rank++ {
		if !upstream.PlacementHolder(rank) {
			t.Fatalf("rank %d must only hold placement", rank)
		}
	}
	if len(upstream.Install) != 3 || upstream.Install[0][0] != "cmake" || upstream.Install[2][0] != "cargo" {
		t.Fatalf("authored builds changed: %v", upstream.Install)
	}
	for key, want := range map[string]string{
		"FLEET":              "${setting.fleet_file}",
		"DSGB10_PROD":        "1",
		"DSGB10_PORT":        "8888",
		"DSGB10_ADDR":        "127.0.0.1 ${node.address}",
		"DSGB10_FRONT_ARGS":  "--tokenizer ${setting.tokenizer} --encoding 1017 --model-name deepseek-v4.1-flash",
		"DSGB10_ENGINE_ARGS": "--max-ctx 1048576 --slots 16 --big-slots 4 --setenv DSGB10_BUDGET=1 --setenv DSGB10_OEA=4,12,9 --setenv DSGB10_DRAFT1=6 --setenv DSGB10_WIDE=64 --setenv DSGB10_WIDE_COST=24,2.2,57,1.1 --draft-head4 --draft-markov8 --setenv DSGB10_HEAD4_C1=1 --setenv DSGB10_CF_MLP=${setting.cf_mlp} --setenv DSGB10_CF_ALPHA=1.0 --setenv DSGB10_CERT=1 --setenv DSGB10_RING_DIRECT=2",
	} {
		if workload.Env[key] != want {
			t.Fatalf("%s = %q, want %q", key, workload.Env[key], want)
		}
	}
	for _, name := range []string{"fleet_file", "tokenizer", "cf_mlp"} {
		if p := manifest.ParameterByName(name); p == nil || p.Optional || p.Default != nil {
			t.Fatalf("operator path %s must be a required setting: %+v", name, p)
		}
	}
	if manifest.ParameterByName("dsgb10_key_file") != nil {
		t.Fatal("the removed API-key input is still offered as a setting")
	}
	if len(workload.Ports) != 1 || workload.Ports[0].Host != 8888 || workload.Readiness == nil || workload.Readiness.HTTPGet == nil ||
		workload.Readiness.HTTPGet.Path != "/health" || workload.Readiness.HTTPGet.Port != 8888 {
		t.Fatalf("cluster endpoint changed: %+v %+v", workload.Ports, workload.Readiness)
	}

	// The reviewed edits remove upstream's key-file requirement and nothing else.
	if len(upstream.Configuration) != 1 || upstream.Configuration[0].Path != "engine/server/service.sh" {
		t.Fatalf("unexpected source configuration: %+v", upstream.Configuration)
	}
	original, err := dsgb10Fixture("engine/server/service.sh")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := sourceconfig.Resolve(upstream.Configuration, nil, func(value string) (string, error) { return value, nil })
	if err != nil {
		t.Fatal(err)
	}
	configured, err := sourceconfig.Apply(original, resolved[0].Edits)
	if err != nil {
		t.Fatal(err)
	}
	text := string(configured)
	for _, removed := range []string{`[ -r "$KEY" ] || die`, `can be read by others`, `--api-key-file`} {
		if !strings.Contains(string(original), removed) || strings.Contains(text, removed) {
			t.Fatalf("API-key requirement %q not removed", removed)
		}
	}
	if !strings.Contains(text, `--engine-socket "$SOCK"`) || !strings.Contains(text, `--admin-socket "$ADMIN"`) ||
		len(original)-len(text) > len(`[ -r "$KEY" ] || die "no API key file $KEY"`)+len(`[ $((8#$(stat -c %a "$KEY") & 8#077)) = 0 ] || die "$KEY can be read by others: chmod 600 it"`)+len(`--api-key-file "$KEY"`) {
		t.Fatal("source edit removed more than the reviewed key requirement")
	}
	if _, err := parseShell(configured, "service.sh"); err != nil {
		t.Fatalf("configured service no longer parses: %v", err)
	}
	encoded, err := json.Marshal(upstream.Configuration)
	if err != nil || strings.Contains(string(encoded), `"parameter"`) {
		t.Fatalf("key removal must be fixed, not an operator setting: %s", encoded)
	}
}

func TestDSGB10RejectsChangedLifecycle(t *testing.T) {
	for name, tc := range map[string]struct{ file, old, new string }{
		"status dispatch removed":   {"engine/server/service.sh", "  status) cmd_status ;;\n", ""},
		"key requirement reworded":  {"engine/server/service.sh", `[ -r "$KEY" ] || die "no API key file $KEY"`, `[ -r "$KEY" ] || echo "no API key file $KEY"`},
		"key argument renamed":      {"engine/server/service.sh", `--api-key-file "$KEY"`, `--key-file "$KEY"`},
		"engine input dropped":      {"engine/server/service.sh", "DSGB10_ENGINE_ARGS", "DSGB10_ENGINE_ARGV"},
		"sourced dependency absent": {"engine/server/ringlock.sh", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			source := dsgb10Source(t)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(path string) ([]byte, error) {
				content, err := dsgb10Fixture(path)
				if err != nil || path != tc.file {
					return content, err
				}
				if tc.old == "" {
					return nil, os.ErrNotExist
				}
				if !strings.Contains(string(content), tc.old) {
					t.Fatalf("fixture lacks %q", tc.old)
				}
				return []byte(strings.ReplaceAll(string(content), tc.old, tc.new)), nil
			}
			_, err := compiler.CompileRetained(context.Background(), source, reader, nil)
			var packed *recipe.PackError
			if !errors.As(err, &packed) || packed.Code != "recipe.repository_layout_changed" {
				t.Fatalf("changed authored lifecycle was not rejected: %v", err)
			}
		})
	}
}
