package backend

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestSchemasRegister(t *testing.T) {
	reg := jobs.New(nil, "", context.Background(), nil, nil)
	if err := reg.Register("benchmarks", jobs.Spec{
		Kind:         "benchmark",
		Title:        "Benchmark",
		InputSchema:  inputSchema,
		OutputSchema: outputSchema,
		Executor:     func(ctx context.Context, c *jobs.Context) (map[string]any, error) { return nil, nil },
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func validateBenchmarkJobInput(t *testing.T, input map[string]any) error {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(inputSchema))
	if err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("benchmark-input.json", document); err != nil {
		t.Fatalf("add schema: %v", err)
	}
	schema, err := compiler.Compile("benchmark-input.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return schema.Validate(input)
}

func validateBenchmarkJobOutput(t *testing.T, output map[string]any) error {
	t.Helper()
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(outputSchema))
	if err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("benchmark-output.json", document); err != nil {
		t.Fatalf("add schema: %v", err)
	}
	schema, err := compiler.Compile("benchmark-output.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return schema.Validate(output)
}

func TestBenchmarkOutputSchemaCoversBothKinds(t *testing.T) {
	codeGeneration := map[string]any{"results": []any{map[string]any{
		"language": "go", "requests": 2, "successes": 1, "total_tokens": 10, "tokens_per_second": 4.2,
	}}}
	if err := validateBenchmarkJobOutput(t, codeGeneration); err != nil {
		t.Fatalf("code-generation output rejected: %v", err)
	}
	harbor := map[string]any{"summary": "summary.json", "trials": "trials.json", "bundle": "result-bundle.tar.gz"}
	if err := validateBenchmarkJobOutput(t, harbor); err != nil {
		t.Fatalf("terminal-bench output rejected: %v", err)
	}
	if err := validateBenchmarkJobOutput(t, map[string]any{}); err == nil {
		t.Fatal("empty output accepted")
	}
	partial := map[string]any{"summary": "summary.json", "trials": "trials.json"}
	if err := validateBenchmarkJobOutput(t, partial); err == nil {
		t.Fatal("terminal-bench output missing bundle accepted")
	}
	badItem := map[string]any{"results": []any{map[string]any{"language": "go"}}}
	if err := validateBenchmarkJobOutput(t, badItem); err == nil {
		t.Fatal("incomplete code-generation result accepted")
	}
}

func TestCodeGenerationPublishesSummaryAndSafeBundle(t *testing.T) {
	workspace := t.TempDir()
	published := map[string]string{}
	job := &jobs.Context{
		RunID:     "01900000-0000-7000-8000-000000000000",
		Workspace: workspace,
		PublishArtifact: func(kind, path string) (jobs.PublishedArtifact, error) {
			published[kind] = path
			return jobs.PublishedArtifact{ID: kind + "-id", Kind: kind, Path: path}, nil
		},
	}
	input := codeGenerationBenchmarkIn{
		BenchmarkID: "lmw-code-generation",
		Version:     "1",
		Harness:     "lmw-oneshot",
	}
	results := []map[string]any{{
		"language": "python", "requests": 1, "successes": 1,
		"total_tokens": 12, "tokens_per_second": 3.5,
	}}
	summaryArtifact, bundleArtifact, err := publishCodeGenerationArtifacts(job, input, results)
	if err != nil {
		t.Fatal(err)
	}
	if summaryArtifact.ID != "benchmark-summary-id" || bundleArtifact.ID != "benchmark-result-bundle-id" {
		t.Fatalf("artifact IDs = %q %q", summaryArtifact.ID, bundleArtifact.ID)
	}
	if published["benchmark-summary"] != "summary.json" || published["benchmark-result-bundle"] != "result-bundle.tar.gz" {
		t.Fatalf("published paths = %#v", published)
	}
	summary, err := os.ReadFile(filepath.Join(workspace, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(summary, &document); err != nil {
		t.Fatal(err)
	}
	if document["run_id"] != job.RunID || document["benchmark_id"] != input.BenchmarkID {
		t.Fatalf("summary identity = %#v", document)
	}

	bundle, err := os.Open(filepath.Join(workspace, "result-bundle.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	gzipReader, err := gzip.NewReader(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "summary.json" || header.Typeflag != tar.TypeReg {
		t.Fatalf("bundle entry = %q type %d", header.Name, header.Typeflag)
	}
	bundledSummary, err := io.ReadAll(tarReader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bundledSummary, summary) {
		t.Fatal("bundled summary differs from published summary")
	}
	if _, err := tarReader.Next(); err != io.EOF {
		t.Fatalf("unexpected extra bundle entry: %v", err)
	}
}

func TestPrepareCoordinatorWorkspace(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Chmod(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := prepareCoordinatorWorkspace(workspace); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 {
		t.Fatalf("workspace mode = %v, want 0777", info.Mode().Perm())
	}
	if err := prepareCoordinatorWorkspace(filepath.Join(workspace, "missing")); err == nil {
		t.Fatal("missing workspace accepted")
	} else if !strings.Contains(err.Error(), "prepare coordinator workspace") {
		t.Fatalf("error lacks workspace context: %v", err)
	}
}

func TestBenchmarkInputSchemaAcceptsCatalogContracts(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
	}{
		{
			name: "lmw code generation",
			input: map[string]any{
				"benchmark_id": "lmw-code-generation", "version": "1", "harness": "lmw-oneshot",
				"generation_deployment_id": "01900000-0000-7000-8000-000000000000", "generation_model": "local",
				"candidate_count": 1, "concurrency": 1, "seed": 0,
				"languages": []any{"go"}, "prompts_per_language": 8, "max_tokens": 512, "temperature": 0.0,
			},
		},
		{
			name: "terminal oracle",
			input: map[string]any{
				"benchmark_id": "terminal-bench", "version": "3.0.0", "harness": "oracle",
				"task_ids": []any{"html-js-filter"}, "candidate_count": 3, "concurrency": 2, "seed": 7,
			},
		},
		{
			name: "terminal agent with verifier",
			input: map[string]any{
				"benchmark_id": "terminal-bench", "version": "3.0.0", "harness": "codex",
				"generation_deployment_id": "01900000-0000-7000-8000-000000000000", "generation_model": "generator",
				"verification_deployment_id": "01900000-0000-7000-8000-000000000001", "verification_model": "verifier",
				"task_ids": []any{}, "candidate_count": 4, "concurrency": 2, "seed": 11,
				"verifier_repetitions": 8, "verifier_pivots": 2,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateBenchmarkJobInput(t, test.input); err != nil {
				t.Fatalf("valid input rejected: %v", err)
			}
		})
	}
}

func TestBenchmarkInputSchemaRejectsInvalidCombinations(t *testing.T) {
	base := map[string]any{
		"benchmark_id": "terminal-bench", "version": "3.0.0", "harness": "oracle",
		"task_ids": []any{"html-js-filter"}, "candidate_count": 1, "concurrency": 1, "seed": 0,
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "obsolete deployment field", mutate: func(input map[string]any) { input["deployment_id"] = "01900000-0000-7000-8000-000000000000" }},
		{name: "unknown task", mutate: func(input map[string]any) { input["task_ids"] = []any{"not-in-the-manifest"} }},
		{name: "oracle deployment", mutate: func(input map[string]any) { input["generation_deployment_id"] = "01900000-0000-7000-8000-000000000000" }},
		{name: "unsupported catalog key", mutate: func(input map[string]any) { input["version"] = "2.0.0" }},
		{name: "verifier with one candidate", mutate: func(input map[string]any) {
			input["harness"] = "codex"
			input["generation_deployment_id"] = "01900000-0000-7000-8000-000000000000"
			input["generation_model"] = "generator"
			input["verification_deployment_id"] = "01900000-0000-7000-8000-000000000001"
			input["verification_model"] = "verifier"
			input["verifier_repetitions"] = 8
			input["verifier_pivots"] = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := make(map[string]any, len(base)+4)
			for key, value := range base {
				input[key] = value
			}
			test.mutate(input)
			if err := validateBenchmarkJobInput(t, input); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}
func TestHarborSpecAcceptsOnlyContentAddressedImages(t *testing.T) {
	const runID = "01900000-0000-7000-8000-000000000000"
	digest := "sha256:" + strings.Repeat("c", 64)
	for name, reference := range map[string]string{
		"registry digest": "ghcr.io/example/worker@" + digest,
		"local image ID":  digest,
	} {
		t.Run(name, func(t *testing.T) {
			spec, _, err := harborSpec(runID, reference, "/workspace", []byte(`{}`), "run", "")
			if err != nil {
				t.Fatal(err)
			}
			if runtime.ImageRef(spec) != reference {
				t.Fatalf("image reference = %q, want %q", runtime.ImageRef(spec), reference)
			}
		})
	}
	for _, reference := range []string{"example/worker:latest", "sha256:" + strings.Repeat("c", 63)} {
		if _, _, err := harborSpec(runID, reference, "/workspace", []byte(`{}`), "run", ""); err == nil {
			t.Fatalf("accepted mutable or malformed image: %q", reference)
		}
	}
}

func TestGraderSpec(t *testing.T) {
	p := graderParams{baseURL: "http://10.0.0.1:8000", model: "local", prompts: 8, maxTokens: 512, temperature: 0.7}
	_, specJSON, err := graderSpec("run-1", p, "python")
	if err != nil {
		t.Fatalf("graderSpec: %v", err)
	}
	var spec runtime.ContainerSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	if spec.ImageDigest == "" || runtime.ImageRef(&spec) != graderImages["python"] {
		t.Fatalf("image not digest-pinned: %s", runtime.ImageRef(&spec))
	}
	if spec.Entrypoint != nil {
		// The pinned image has no entrypoint; the spec must not introduce one.
		t.Fatalf("entrypoint must be omitted, got %#v", spec.Entrypoint)
	}
	if len(spec.Cmd) != 4 || spec.Cmd[0] != "python3" || spec.Cmd[1] != "-c" {
		t.Fatalf("cmd shape: %#v", spec.Cmd)
	}
	decoded, err := base64.StdEncoding.DecodeString(spec.Cmd[3])
	if err != nil {
		t.Fatalf("cmd b64: %v", err)
	}
	if string(decoded) != graderScript {
		t.Fatalf("embedded script round-trip mismatch")
	}
	env := map[string]string{}
	for _, e := range spec.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	want := map[string]string{
		"LMW_BASE_URL":    "http://10.0.0.1:8000",
		"LMW_MODEL":       "local",
		"LMW_LANG":        "python",
		"LMW_PROMPTS":     "8",
		"LMW_MAX_TOKENS":  "512",
		"LMW_TEMPERATURE": "0.7",
		"LMW_RUN_ID":      "run-1",
	}
	if len(env) != len(want) {
		t.Fatalf("env set: %#v", env)
	}
	for k, v := range want {
		if env[k] != v {
			t.Fatalf("env %s = %q, want %q", k, env[k], v)
		}
	}
	if spec.NetworkMode != "host" || !spec.ReadonlyRootfs || !spec.NoNewPrivileges || spec.TmpfsBytes == 0 || spec.PidsLimit == 0 || len(spec.CapDrop) != 1 || spec.CapDrop[0] != "ALL" {
		t.Fatalf("hardening: %#v", spec)
	}
	if spec.Labels[runtime.LabelManaged] != "true" || spec.Labels[runtime.LabelRun] != "run-1" || spec.Labels[runtime.LabelModule] != "benchmarks" {
		t.Fatalf("labels: %#v", spec.Labels)
	}
}

func TestReadRunnerError(t *testing.T) {
	workspace := t.TempDir()
	if got := readRunnerError(workspace); got != "" {
		t.Fatalf("missing file read = %q", got)
	}
	if err := os.WriteFile(filepath.Join(workspace, "runner-error.txt"), []byte("benchmarks.runner_failed: boom\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readRunnerError(workspace); got != "benchmarks.runner_failed: boom" {
		t.Fatalf("read = %q", got)
	}
}

func TestGraderConsumesVersionedOpenAIAPIBase(t *testing.T) {
	if !strings.Contains(graderScript, `base.rstrip("/") + "/chat/completions"`) {
		t.Fatal("grader does not append chat/completions to the shared /v1 API base")
	}
	if strings.Contains(graderScript, `base.rstrip("/") + "/v1/chat/completions"`) {
		t.Fatal("grader duplicates the /v1 path")
	}
}

func TestEveryLanguageHasExecutableGrader(t *testing.T) {
	markers := map[string]string{
		"python": "python3", "javascript": "node", "go": "\"go\", \"run\"",
		"rust": "rustc", "cpp": "g++", "java": "javac",
	}
	for _, language := range supportedLanguages {
		spec, _, err := graderSpec("run", graderParams{}, language)
		if err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		if runtime.ImageRef(spec) != graderImages[language] || !strings.Contains(graderScript, markers[language]) {
			t.Fatalf("%s grader is not executable", language)
		}
	}
}

func TestGraderDropsNetworkBeforeExecutingPrograms(t *testing.T) {
	command := exec.Command("python3", "-c", `import sys,socket
ns={"__name__":"graderlib"}
exec(sys.stdin.read(),ns)
ns["deny_network"]()
try:
    socket.socket()
except OSError as error:
    raise SystemExit(0 if error.errno == 1 else 2)
raise SystemExit(3)`)
	command.Stdin = strings.NewReader(graderScript)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("network sandbox: %v: %s", err, output)
	}
}

func TestParseResultLine(t *testing.T) {
	gr, err := parseResultLine("noise\n[python 1/2] ok\nRESULT:{\"lang\":\"python\",\"requests\":2,\"successes\":2,\"total_tokens\":10,\"tokens_per_second\":1.5}\ntrailing\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if gr.Lang != "python" || gr.Requests != 2 || gr.Successes != 2 || gr.TotalTokens != 10 || gr.TokensPerSecond != 1.5 {
		t.Fatalf("result: %#v", gr)
	}
	// last marker wins
	gr, err = parseResultLine("RESULT:{\"bad\":true}\nRESULT:{\"lang\":\"rust\",\"requests\":1,\"successes\":1,\"total_tokens\":1,\"tokens_per_second\":1}")
	if err != nil || gr.Lang != "rust" {
		t.Fatalf("last wins: %#v %v", gr, err)
	}
	// no marker
	if _, err := parseResultLine("no result here"); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("missing marker: %v", err)
	}
}

func TestDecodeCodeGenerationInput(t *testing.T) {
	raw := map[string]any{
		"benchmark_id": "lmw-code-generation", "version": "1", "harness": "lmw-oneshot",
		"generation_deployment_id": "01900000-0000-7000-8000-000000000000", "generation_model": "local",
		"candidate_count": 1, "concurrency": 2, "seed": 3,
		"languages": []any{"go"}, "prompts_per_language": 8, "max_tokens": 512, "temperature": 0.5,
	}
	var input codeGenerationBenchmarkIn
	if err := decodeBenchmarkInput(raw, &input); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if input.GenerationDeploymentID != raw["generation_deployment_id"] || input.GenerationModel != "local" ||
		input.PromptsPerLanguage != 8 || input.MaxTokens != 512 || input.Temperature != 0.5 {
		t.Fatalf("decoded input: %#v", input)
	}
}
