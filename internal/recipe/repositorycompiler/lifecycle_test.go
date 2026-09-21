package repositorycompiler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestMaintainedRepositoriesAdvanceWithoutManualCommitApproval(t *testing.T) {
	for _, family := range []string{
		"qwen38-27b-rtx6000pro-dflash2", "qwen38-27b-dgx-spark-mtp",
		"qwen38-flash-next-spark-tp1", "qwen38-flash-next-dspark-tp2",
		"glm53-flash-exl3-dflash2-spark-tp2", "deepseek-v4-flash-vision-exp-dspark-tp2",
	} {
		t.Run(family, func(t *testing.T) {
			template := configurationTemplate(t, family)
			source := recipe.RepositorySource{URL: template.Metadata.Source.URL, Path: ".", CommitSHA: template.Metadata.Source.Revision}
			source.RepositoryID = repositoryID(t, source.URL)
			compiler, ok := NewRegistry(mustValidator(t)).LookupUpstream(source)
			if !ok {
				t.Fatal("maintained compiler missing")
			}
			reader := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", family, name)) }
			baseline, err := compiler.CompileRetained(context.Background(), source, reader, nil)
			if err != nil {
				t.Fatal(err)
			}
			source.CommitSHA = strings.Repeat("e", 40)
			startPath := strings.TrimPrefix(template.Workloads[0].Upstream.Start[0], "./")
			updatedReader := func(name string) ([]byte, error) {
				content, err := reader(name)
				if err == nil && name == startPath {
					content = append(content, []byte("\n# Upstream documentation evolves without changing its interface.\n")...)
				}
				return content, err
			}
			updated, err := compiler.CompileRetained(context.Background(), source, updatedReader, nil)
			if err != nil {
				t.Fatalf("supported source evolution rejected: %v", err)
			}
			manifest, err := recipe.Parse(updated.ConfigJSON)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Metadata.Source.Revision != source.CommitSHA || baseline.ManifestDigest == updated.ManifestDigest {
				t.Fatal("new exact source revision did not reach the saved package")
			}
			for _, config := range manifest.Workloads[0].Upstream.Configuration {
				content, err := updatedReader(config.Path)
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(content)
				if config.SHA256 != hex.EncodeToString(digest[:]) {
					t.Fatalf("stale source hash for %s", config.Path)
				}
			}
		})
	}
}

func TestUpdatedSourceConfigurationUsesTargetOptions(t *testing.T) {
	const family = "qwen38-27b-rtx6000pro-dflash2"
	template := configurationTemplate(t, family)
	source := recipe.RepositorySource{URL: template.Metadata.Source.URL, Path: ".", CommitSHA: strings.Repeat("f", 40)}
	source.RepositoryID = repositoryID(t, source.URL)
	compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
	reader := func(name string) ([]byte, error) {
		content, err := os.ReadFile(filepath.Join("testdata", family, name))
		if name == "start.sh" {
			before := string(content)
			content = []byte(strings.ReplaceAll(before, "--kv-cache-dtype fp8_e4m3", "--kv-cache-dtype auto"))
			if string(content) == before {
				t.Fatal("fixture lacks the source option being evolved")
			}
		}
		return content, err
	}
	packed, err := compiler.CompileRetained(context.Background(), source, reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := recipe.Parse(packed.ConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	parameters := map[string]recipe.Parameter{}
	for _, parameter := range manifest.Parameters {
		parameters[parameter.Name] = parameter
	}
	assertDefault(t, parameters, "kv_cache_dtype", "auto")
	content, _ := reader("start.sh")
	digest := sha256.Sum256(content)
	if manifest.Workloads[0].Upstream.Configuration[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("updated option offsets are not bound to target source bytes")
	}
}

func TestMaintainedCompilerRejectsIncompatibleLifecycleEvidence(t *testing.T) {
	for _, tc := range []struct{ name, family, file, old, replacement string }{
		{"missing stop operation", "qwen38-27b-rtx6000pro-dflash2", "stop.sh", "docker stop", "docker inspect"},
		{"stop renamed independently", "qwen38-27b-rtx6000pro-dflash2", "stop.sh", "qwen3.8-27b-sglang-6000pro", "unrelated-container"},
		{"launch removed", "qwen38-27b-rtx6000pro-dflash2", "start.sh", "docker run", "echo run"},
		{"bound input removed", "qwen38-27b-rtx6000pro-dflash2", "start.sh", "MAX_CONCURRENT_REQUESTS", "NEW_REQUEST_LIMIT"},
		{"bound input overwritten", "qwen38-27b-rtx6000pro-dflash2", "start.sh", "MAX_CONCURRENT_REQUESTS=\"${MAX_CONCURRENT_REQUESTS:-8}\"", "MAX_CONCURRENT_REQUESTS=4"},
		{"installer argument removed", "deepseek-v4-flash-vision-exp-dspark-tp2", "prepare-dspark-model-cache.sh", "--yes|-y)", "--new-option)"},
		{"shell environment no longer sourced", "qwen38-flash-next-spark-tp1", "start.sh", "source .env", "echo .env"},
		{"serving argv disconnected", "qwen38-flash-next-spark-tp1", "start.sh", "VLLM_ARGS_STR", "UNSUPPORTED_ARGS"},
		{"literal environment changed", "qwen38-27b-dgx-spark-mtp", "start.sh", "done < \"${SCRIPT_DIR}/.env\"", "done < /dev/null"},
		{"compose stop removed", "deepseek-v4-flash-vision-exp-dspark-tp2", "stop-deepseek-v4-flash-dspark.sh", "down --remove-orphans", "config --remove-orphans"},
		{"delegated stop removed", "glm53-flash-exl3-dflash2-spark-tp2", "stop.sh", "\"$SCRIPT_DIR/start.sh\" stop", "echo stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := configurationTemplate(t, tc.family)
			source := recipe.RepositorySource{URL: template.Metadata.Source.URL, Path: ".", CommitSHA: strings.Repeat("d", 40)}
			source.RepositoryID = repositoryID(t, source.URL)
			compiler, _ := NewRegistry(mustValidator(t)).LookupUpstream(source)
			reader := func(name string) ([]byte, error) {
				content, err := os.ReadFile(filepath.Join("testdata", tc.family, name))
				if name == tc.file {
					if !strings.Contains(string(content), tc.old) {
						t.Fatalf("fixture does not contain %q", tc.old)
					}
					content = []byte(strings.ReplaceAll(string(content), tc.old, tc.replacement))
				}
				return content, err
			}
			_, err := compiler.CompileRetained(context.Background(), source, reader, nil)
			var packErr *recipe.PackError
			if !errors.As(err, &packErr) || packErr.Code != "recipe.repository_layout_changed" {
				t.Fatalf("incompatible source contract was not rejected precisely: %v", err)
			}
		})
	}
}
