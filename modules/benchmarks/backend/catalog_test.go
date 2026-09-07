package backend

import (
	"strings"
	"testing"
)

func TestStaticBenchmarkCatalog(t *testing.T) {
	catalog := staticBenchmarkCatalog()
	if len(catalog) != 2 {
		t.Fatalf("catalog entries = %d", len(catalog))
	}
	codeGeneration := catalog[0]
	if codeGeneration.BenchmarkID != "lmw-code-generation" || codeGeneration.Version != "1" || len(codeGeneration.Languages) != 6 {
		t.Fatalf("code generation catalog = %#v", codeGeneration)
	}

	terminalBench := catalog[1]
	if terminalBench.BenchmarkID != "terminal-bench" || terminalBench.Version != "3.0.0" {
		t.Fatalf("Terminal-Bench catalog = %#v", terminalBench)
	}
	if terminalBench.DatasetLocator != "terminal-bench/terminal-bench@3.0.0" {
		t.Fatalf("dataset locator = %q", terminalBench.DatasetLocator)
	}
	if terminalBench.SourceChecksum != "2b0442c3c583b710ca8da14c8e601b99f2f1f244" {
		t.Fatalf("source checksum = %q", terminalBench.SourceChecksum)
	}
	if terminalBench.TaskCount != 74 || len(terminalBench.TaskIDs) != 74 {
		t.Fatalf("task count = %d, manifest entries = %d", terminalBench.TaskCount, len(terminalBench.TaskIDs))
	}
	seen := make(map[string]bool, len(terminalBench.TaskIDs))
	for _, taskID := range terminalBench.TaskIDs {
		if seen[taskID] {
			t.Fatalf("duplicate task ID %q", taskID)
		}
		seen[taskID] = true
	}
	if !seen["html-js-filter"] {
		t.Fatal("one-task smoke target is missing from the manifest")
	}
	if len(terminalBench.Harnesses) != 3 || terminalBench.Harnesses[0].RequiresGenerationDeployment || !terminalBench.Harnesses[1].RequiresGenerationDeployment || !terminalBench.Harnesses[2].RequiresGenerationDeployment {
		t.Fatalf("harnesses = %#v", terminalBench.Harnesses)
	}
	if !terminalBench.SupportsTaskFilters || !terminalBench.SupportsRepeatedCandidates || !terminalBench.SupportsVerifier {
		t.Fatalf("Terminal-Bench capabilities = %#v", terminalBench)
	}
}

func TestRunnerImageConfigured(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	cases := []struct {
		name   string
		values map[string]any
		want   bool
	}{
		{"registry digest", map[string]any{"harbor_runner_image": "ghcr.io/example/runner@" + digest}, true},
		{"bare image ID", map[string]any{"harbor_runner_image": digest}, true},
		{"padded image ID", map[string]any{"harbor_runner_image": "  " + digest + "  "}, true},
		{"mutable tag", map[string]any{"harbor_runner_image": "ghcr.io/example/runner:latest"}, false},
		{"missing", map[string]any{}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := runnerImageConfigured(test.values); got != test.want {
				t.Fatalf("runnerImageConfigured = %v, want %v", got, test.want)
			}
		})
	}
}
