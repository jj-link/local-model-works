package sourceconfig

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeredocArgvSurvivesBothBashExecutionPhases(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("Bash required for authored heredoc launchers")
	}
	args := []string{
		"a b", "'single' and \"double\" quotes", `back\slash`, "$HOME", "$(touch substitution)", "`touch backtick`", "",
		"line one\nLAUNCH_EOF\nprintf injected > breakout\ncat <<LAUNCH_EOF\nline two\rfinal",
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"argv-heredoc", "argv-quoted-heredoc"} {
		t.Run(format, func(t *testing.T) {
			delimiter := "LAUNCH_EOF"
			if format == "argv-quoted-heredoc" {
				delimiter = "'LAUNCH_EOF'"
			}
			original := "#!/usr/bin/env bash\nset -eu\ncat > generated.sh <<" + delimiter + "\n#!/usr/bin/env bash\nset -eu\nprintf '%s\\0' fixed > args.bin\nprintf '%s' 'inner preserved' > inner.txt\nLAUNCH_EOF\nbash generated.sh\nprintf '%s' 'outer preserved' > outer.txt\n"
			start := strings.Index(original, "fixed") + len("fixed")
			files := []File{{Path: "outer.sh", SHA256: strings.Repeat("a", 64), Edits: []Edit{{Start: start, End: start, Parameter: "extras", Format: format}}}}
			resolved, err := Resolve(files, map[string]any{"extras": string(encoded)}, nil)
			if err != nil {
				t.Fatal(err)
			}
			configured, err := Apply([]byte(original), resolved[0].Edits)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "outer.sh"), configured, 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash, "outer.sh")
			cmd.Dir = dir
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("authored Bash phases failed: %s: %v", output, err)
			}
			want := strings.Join(append([]string{"fixed"}, args...), "\x00") + "\x00"
			got, err := os.ReadFile(filepath.Join(dir, "args.bin"))
			if err != nil || string(got) != want {
				t.Fatalf("argv changed between authored shell phases: got %q, want %q: %v", got, want, err)
			}
			for name, expected := range map[string]string{"inner.txt": "inner preserved", "outer.txt": "outer preserved"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(data) != expected {
					t.Fatalf("unrelated %s behavior changed: %q: %v", name, data, err)
				}
			}
			for _, name := range []string{"substitution", "backtick", "breakout"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Fatalf("argument executed during a shell phase: %s", name)
				}
			}
		})
	}
}
