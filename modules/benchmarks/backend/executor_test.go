package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/jobs"
	"github.com/jj-link/local-model-works/internal/runtime"
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

func TestGraderBaseURLUsesColocatedLoopback(t *testing.T) {
	got, err := graderBaseURL(8000)
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://127.0.0.1:8000" {
		t.Fatalf("base URL = %q", got)
	}
	if _, err := graderBaseURL(0); err == nil {
		t.Fatal("zero endpoint port accepted")
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

func TestParseInputDefaults(t *testing.T) {
	in, err := parseInput(map[string]any{"deployment_id": "01900000-0000-7000-8000-000000000000", "languages": []any{"go"}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if in.PromptsPerLanguage != 0 || in.MaxTokens != 0 || in.Temperature != 0 {
		t.Fatalf("defaults: %#v", in)
	}
}
