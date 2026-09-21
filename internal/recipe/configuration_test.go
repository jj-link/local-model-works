package recipe

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

func TestEffectiveSettingsRequiredOptionalAndArgv(t *testing.T) {
	manifest := &Manifest{Parameters: []Parameter{
		{Name: "required", Type: "string"},
		{Name: "optional", Type: "string", Optional: true},
		{Name: "args", Type: "string", Format: "argv", Optional: true},
		{Name: "defaulted", Type: "int", Default: 8},
	}}
	if _, err := manifest.EffectiveSettings(nil); err == nil {
		t.Fatal("required missing input accepted")
	}
	settings, err := manifest.EffectiveSettings(map[string]any{"required": "", "args": `["a b","$(literal)"]`})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := settings["optional"]; exists {
		t.Fatal("optional omission turned into a value")
	}
	if settings["required"] != "" || settings["defaulted"] != 8 {
		t.Fatalf("explicit empty value or default lost: %v", settings)
	}
	if _, err := manifest.EffectiveSettings(map[string]any{"required": "value", "args": `[null]`}); err == nil {
		t.Fatal("invalid argv bypassed settings validation")
	}
}

func TestConfigurationSchemaRejectsUndeclaredAndWrongTypedEdits(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	manifest := upstreamValidationManifest()
	manifest.Parameters = append(manifest.Parameters, Parameter{Name: "flag", Type: "bool", Optional: true, Label: "Runtime flag", Group: "Runtime"})
	manifest.Workloads[0].Upstream.Configuration = []sourceconfig.File{{Path: "start.sh", SHA256: strings.Repeat("A", 64), Edits: []sourceconfig.Edit{{Start: 0, End: 4, Parameter: "flag", Format: "flag", Flag: "--enable"}}}}
	validate := func() bool {
		t.Helper()
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		_, findings, err := validator.ValidateStrict(data)
		return err == nil && len(findings) == 0
	}
	if !validate() {
		t.Fatal("valid optional runtime flag rejected")
	}
	manifest.Workloads[0].Upstream.Configuration[0].Edits[0].Parameter = "missing"
	if validate() {
		t.Fatal("undeclared source parameter accepted")
	}
	manifest.Workloads[0].Upstream.Configuration[0].Edits[0].Parameter = "port"
	if validate() {
		t.Fatal("integer parameter accepted as boolean flag")
	}
}

func TestConfigurationTemplatesValidateAndRequireReviewedBindings(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	manifest := upstreamValidationManifest()
	manifest.Workloads[0].Upstream.Configuration = []sourceconfig.File{{
		Path: "start.sh", SHA256: strings.Repeat("a", 64),
		Edits: []sourceconfig.Edit{{Start: 0, End: 0, Template: `["--env=CUDA_VISIBLE_DEVICES=${node.accelerators}"]`, Format: "argv"}},
	}}
	validate := func() bool {
		t.Helper()
		document, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		_, findings, err := validator.ValidateStrict(document)
		return err == nil && len(findings) == 0
	}
	if !validate() {
		t.Fatal("reviewed accelerator template rejected")
	}
	edit := &manifest.Workloads[0].Upstream.Configuration[0].Edits[0]
	edit.Parameter = "port"
	if validate() {
		t.Fatal("schema accepted both parameter and template")
	}
	edit.Parameter = ""
	edit.Template = `["${node.unknown}"]`
	if validate() {
		t.Fatal("undeclared source template accepted")
	}
	edit.Template = `["${setting.undeclared}"]`
	if validate() {
		t.Fatal("undeclared source setting accepted")
	}
	if _, err := manifest.Render(TemplNodeAccelerators, RenderContext{
		Settings: map[string]any{"node.accelerators": "GPU-user", "accelerators": "GPU-user"},
	}); err == nil {
		t.Fatal("settings supplied a missing reviewed accelerator binding")
	}
}

func TestSettingsArgumentPoliciesProtectTopologyAndDockerCommand(t *testing.T) {
	manifest := &Manifest{Parameters: []Parameter{
		{Name: "engine", Type: "string", Format: "argv", Optional: true, ForbiddenArgs: []string{"--port", "-p", "--node-rank"}},
		{Name: "docker", Type: "string", Format: "argv", Optional: true, ArgvPolicy: "docker", ForbiddenArgs: []string{"--name", "--entrypoint"}, ForbiddenEnv: []string{"NCCL_SOCKET_IFNAME", "VLLM_HOST_IP"}},
	}}
	for _, item := range []struct {
		parameter string
		args      []string
	}{
		{"engine", []string{"--port", "9000"}},
		{"engine", []string{"--port=9000"}},
		{"engine", []string{"--po=9000"}},
		{"engine", []string{"--node_rank=1"}},
		{"engine", []string{"-p9000"}},
		{"engine", []string{"-xp"}},
		{"docker", []string{"foreign-image", "command"}},
		{"docker", []string{"--"}},
		{"docker", []string{"--privileged"}},
		{"docker", []string{"-e", "SAFE=value"}},
		{"docker", []string{"--env-file=unsafe.env"}},
		{"docker", []string{"--env=NCCL_SOCKET_IFNAME=foreign"}},
		{"docker", []string{"--env=VLLM_HOST_IP"}},
		{"docker", []string{"--name=foreign-container"}},
		{"docker", []string{"--entrypoint=foreign-command"}},
	} {
		encoded, err := json.Marshal(item.args)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manifest.EffectiveSettings(map[string]any{item.parameter: string(encoded)}); err == nil {
			t.Fatalf("%s configuration bypassed source-bound policy: %v", item.parameter, item.args)
		}
	}
	if _, err := manifest.EffectiveSettings(map[string]any{
		"engine": `["--port-mode=auto","--dtype","bfloat16"]`,
		"docker": `["--env=VLLM_USE_V2_MODEL_RUNNER=1","--env=SAFE=a b; $(literal)","--privileged=false","--ipc=host"]`,
	}); err != nil {
		t.Fatalf("independent supported options rejected: %v", err)
	}
}
