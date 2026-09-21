package sourceconfig

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const captureShell = "set -eu\ncapture() { for value; do printf '%s\\0' \"$value\"; done; }; export -f capture\n"

func runConfiguredScalar(t *testing.T, original string, edit Edit, values map[string]any, environment []string, want []string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash required for authored launch-script argument consumption")
	}
	edit.Start = strings.Index(original, "__VALUE__")
	if edit.Start < 0 {
		t.Fatal("missing source binding")
	}
	edit.End = edit.Start + len("__VALUE__")
	files := []File{{Path: "start.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{edit}}}
	resolved, err := Resolve(files, values, nil)
	if err != nil {
		t.Fatal(err)
	}
	configured, err := Apply([]byte(original), resolved[0].Edits)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "--noprofile", "--norc")
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C", "LMW_ENCODING_ENV=expanded"}, environment...)
	cmd.Stdin = strings.NewReader(captureShell + string(configured))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("original shell phases: %v: %s", err, output)
	}
	expected := strings.Join(want, "\x00") + "\x00"
	if string(output) != expected {
		t.Fatalf("selected data changed or executed across shell phases:\n got %q\nwant %q", output, expected)
	}
}

func TestScalarSettingsSurviveAuthoredShellPhases(t *testing.T) {
	value := "space 'single' \"double\" \\backslash $LMW_ENCODING_ENV $(printf injected) `printf backtick`\nLAUNCH_EOF\nprintf breakout\nline\rfinal β"
	for _, tc := range []struct {
		format string
		script string
	}{
		{"shell", "capture --setting __VALUE__\n"},
		{"shell-reparse", "ARGS=(--setting __VALUE__)\nbash <<LAUNCH_EOF\ncapture ${ARGS[*]}\nLAUNCH_EOF\n"},
		{"shell-double-quoted", "COMMAND=\"capture --setting __VALUE__\"\nbash -c \"$COMMAND\"\n"},
		{"shell-heredoc", "bash <<LAUNCH_EOF\ncapture --setting __VALUE__\nLAUNCH_EOF\n"},
		{"shell-quoted-heredoc", "bash <<'LAUNCH_EOF'\ncapture --setting __VALUE__\nLAUNCH_EOF\n"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			runConfiguredScalar(t, tc.script, Edit{Parameter: "setting", Format: tc.format}, map[string]any{"setting": value}, nil, []string{"--setting", value})
		})
	}
}

func TestComputedWordsKeepUpstreamDerivationAndLiteralMounts(t *testing.T) {
	base := "/data/hf cache/'quoted' $LMW_ENCODING_ENV $(printf injected) `printf backtick`\\path\nLAUNCH_EOF\nprintf breakout\nβ"
	prefix, suffix := "--volume=", ":/root/.cache/huggingface"
	derived := "ROOT=\"${BASE}/derived\"\n"
	for _, tc := range []struct {
		name     string
		script   string
		variable string
		indirect bool
	}{
		{"head-generated-mount", derived + "bash <<LAUNCH_EOF\ncapture __VALUE__\nLAUNCH_EOF\n", "ROOT", false},
		{"worker-assembled-mount", derived + "MOUNT=\"__VALUE__\"\nbash <<LAUNCH_EOF\ncapture $MOUNT\nLAUNCH_EOF\n", "ROOT", false},
		{"worker-indirect-environment", derived + "v=ROOT\nMOUNT=\"__VALUE__\"\nbash <<LAUNCH_EOF\ncapture $MOUNT\nLAUNCH_EOF\n", "v", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			edit := Edit{Variable: tc.variable, Indirect: tc.indirect, Prefix: prefix, Suffix: suffix, Format: "shell-word"}
			runConfiguredScalar(t, tc.script, edit, nil, []string{"BASE=" + base}, []string{prefix + base + "/derived" + suffix})
		})
	}
}

func TestIndirectWordPreservesUnsetEnvironmentFallback(t *testing.T) {
	edit := Edit{Variable: "v", Indirect: true, Prefix: "--env=OPTIONAL=", Format: "shell-word"}
	runConfiguredScalar(t, "v=LMW_UNSET_OPTIONAL\nARGS=\"__VALUE__\"\nbash <<LAUNCH_EOF\ncapture $ARGS\nLAUNCH_EOF\n", edit, nil, nil, []string{"--env=OPTIONAL="})
}

func TestComputedWordAffixesCannotCloseGeneratedHeredoc(t *testing.T) {
	prefix := "before\nLAUNCH_EOF\nprintf breakout\n$(printf injected) ' \\\""
	suffix := "after\n`printf backtick` $LMW_ENCODING_ENV"
	edit := Edit{Variable: "ROOT", Prefix: prefix, Suffix: suffix, Format: "shell-word"}
	runConfiguredScalar(t, "ROOT=value\nbash <<LAUNCH_EOF\ncapture __VALUE__\nLAUNCH_EOF\n", edit, nil, nil, []string{prefix + "value" + suffix})
}

func TestComputedWordRejectsExecutableVariableNames(t *testing.T) {
	edit := Edit{Variable: "ROOT[$(printf injected)]", Format: "shell-word"}
	_, err := Resolve([]File{{Path: "start.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{edit}}}, nil, nil)
	if err == nil {
		t.Fatal("executable array subscript accepted as a computed variable name")
	}
}

func TestCommandWordPreservesLiteralOutputAcrossRemoteParsing(t *testing.T) {
	value := "/cache/it's \"quoted\" $LMW_ENCODING_ENV $(printf injected) `printf backtick`\\path\nline:/root/.cache/huggingface:ro"
	edit := Edit{Command: "_mount", Format: "shell-word"}
	script := "_mount() { printf '%s\\n\\n' \"$MOUNT_VALUE\"; }\nCOMMAND=\"capture -v __VALUE__\"\nbash -c \"$COMMAND\"\n"
	// Command substitution still strips trailing newlines, as in the authored
	// call, while every other byte reaches the remote consumer literally.
	runConfiguredScalar(t, script, edit, nil, []string{"MOUNT_VALUE=" + value}, []string{"-v", value})
}

func TestCommandWordRejectsCodeAndMixedBindings(t *testing.T) {
	for _, edit := range []Edit{
		{Command: "_mount argument", Format: "shell-word"},
		{Command: "_mount;printf injected", Format: "shell-word"},
		{Command: "$(printf injected)", Format: "shell-word"},
		{Command: "/usr/bin/printf", Format: "shell-word"},
		{Command: "_mount", Variable: "ROOT", Format: "shell-word"},
		{Command: "_mount", Parameter: "root", Format: "shell-word"},
		{Command: "_mount", Template: "root", Format: "shell-word"},
		{Command: "_mount", Format: "shell"},
		{Command: "_mount", Indirect: true, Format: "shell-word"},
		{Command: "_mount", Prefix: "prefix", Format: "shell-word"},
		{Command: "_mount", Flag: "--volume", Format: "shell-word"},
	} {
		if _, err := Resolve([]File{{Path: "start.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{edit}}}, nil, nil); err == nil {
			t.Fatalf("unsafe command binding accepted: %#v", edit)
		}
	}
}
