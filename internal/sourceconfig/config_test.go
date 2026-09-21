package sourceconfig

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveOmissionFlagsAndArgv(t *testing.T) {
	original := "printf '%s\\0' x y z; printf '%s' untouched"
	start := strings.Index(original, "x y z")
	files := []File{{Path: "start.sh", SHA256: strings.Repeat("A", 64), Edits: []Edit{
		{Start: start, End: start + 1, Parameter: "omitted", Format: "shell"},
		{Start: start + 2, End: start + 3, Parameter: "enabled", Format: "flag", Flag: "--trust-remote-code"},
		{Start: start + 4, End: start + 5, Parameter: "args", Format: "argv"},
	}}}
	args, _ := json.Marshal([]string{"a b", "$(touch bad)", "'quoted'", ""})
	resolved, err := Resolve(files, map[string]any{"enabled": false, "args": string(args)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Apply([]byte(original), resolved[0].Edits)
	if err != nil {
		t.Fatal(err)
	}
	if resolved[0].SHA256 != strings.Repeat("a", 64) {
		t.Fatal("source hash not canonicalized")
	}
	for _, invalid := range []any{"null", `[null]`, `[1]`, `"command"`, `[$(touch bad)]`, true} {
		if _, err := Resolve(files, map[string]any{"args": invalid}, nil); err == nil {
			t.Fatalf("invalid argv accepted: %v", invalid)
		}
	}
	if _, err := Resolve(files, map[string]any{"enabled": "true"}, nil); err == nil {
		t.Fatal("nonboolean flag accepted")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell required")
	}
	dir := t.TempDir()
	cmd := exec.Command(sh, "-c", string(result))
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	want := strings.Join([]string{"x", "a b", "$(touch bad)", "'quoted'", ""}, "\x00") + "\x00untouched"
	if err != nil || string(output) != want {
		t.Fatalf("configured argv/default behavior = %q, want %q: %v", output, want, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
		t.Fatal("argv input was evaluated")
	}
}

func TestSourceEditBoundaries(t *testing.T) {
	for _, path := range []string{"../start.sh", "/start.sh", "a/../start.sh", "a\\start.sh", "a/.git/config", "a//start.sh", "C:script", "a/start.sh\n"} {
		if SafePath(path) {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
	for _, edits := range [][]ResolvedEdit{
		{{Start: 0, End: 2}, {Start: 1, End: 3}},
		{{Start: 0, End: 99}},
		{{Start: -1, End: 0}},
		{{Start: 1, End: 2}}, // Inside the first UTF-8 rune.
		{{Start: 0, End: 0}, {Start: 0, End: 0}},
	} {
		if _, err := Apply([]byte("écho"), edits); err == nil {
			t.Fatalf("invalid byte edits accepted: %+v", edits)
		}
	}
}

func TestResolveTemplatesAreQuotedAfterRendering(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell required")
	}
	const original = "printf '%s\\0' placeholder suffix"
	const bound = "GPU-a b'$(touch bad)"
	start := strings.Index(original, "placeholder")
	for _, format := range []string{"shell", "argv"} {
		t.Run(format, func(t *testing.T) {
			template := "${node.accelerators}"
			if format == "argv" {
				template = `["${node.accelerators}"]`
			}
			files := []File{{Path: "start.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{{
				Start: start, End: start + len("placeholder"), Template: template, Format: format,
			}}}}
			resolved, err := Resolve(files, map[string]any{"node.accelerators": "user-override"}, func(value string) (string, error) {
				return strings.ReplaceAll(value, "${node.accelerators}", bound), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			configured, err := Apply([]byte(original), resolved[0].Edits)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			cmd := exec.Command(sh, "-c", string(configured))
			cmd.Dir = dir
			got, err := cmd.CombinedOutput()
			if want := bound + "\x00suffix\x00"; err != nil || string(got) != want {
				t.Fatalf("rendered argument = %q, want %q: %v", got, want, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "bad")); !os.IsNotExist(err) {
				t.Fatal("rendered template was evaluated as shell syntax")
			}
		})
	}
}

func TestResolveTemplatesFailClosed(t *testing.T) {
	files := []File{{Path: "start.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{{
		Start: 0, End: 0, Template: `["${node.accelerators}"]`, Format: "argv",
	}}}}
	if _, err := Resolve(files, nil, nil); err == nil {
		t.Fatal("required template silently omitted without a renderer")
	}
	unbound := errors.New("unbound accelerator")
	if _, err := Resolve(files, nil, func(string) (string, error) { return "", unbound }); !errors.Is(err, unbound) {
		t.Fatalf("lost required binding error: %v", err)
	}
	files[0].Edits[0].Parameter = "override"
	if _, err := Resolve(files, map[string]any{"override": `["GPU-other"]`}, func(value string) (string, error) { return value, nil }); err == nil {
		t.Fatal("ambiguous parameter/template binding accepted")
	}
	files[0].Edits[0].Parameter = ""
	files[0].Edits[0].Template = ""
	if _, err := Resolve(files, nil, nil); err == nil {
		t.Fatal("edit without a binding accepted")
	}
}
