//go:build linux

package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

func configurationFixture(t *testing.T) (*ContainerSpec, string, []byte) {
	t.Helper()
	repo, revision := upstreamFixture(t, "printf 'install\\n' >> install.count")
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repo, revision
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	spec.Upstream.Start = []string{"./start.sh", "original argument"}
	spec.Env = []string{"VALUE=original environment"}
	original, err := pinnedConfigurationBytes(context.Background(), repo, revision, "recipe/start.sh")
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	return spec, workspace, original
}

func configureFixtureValue(t *testing.T, spec *ContainerSpec, original []byte, value string) {
	t.Helper()
	token := "'started\\n'"
	start := bytes.Index(original, []byte(token))
	if start < 0 {
		t.Fatal("fixture no longer has hardcoded runtime argument")
	}
	files, err := sourceconfig.Resolve([]sourceconfig.File{{Path: "start.sh", SHA256: strings.ToUpper(sourceBytesHash(original)), Edits: []sourceconfig.Edit{{Start: start, End: start + len(token), Parameter: "value", Format: "shell"}}}}, map[string]any{"value": value}, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec.Upstream.Configuration = files
}

func TestUpstreamConfigurationExecutesLiteralAndReconfiguresInPlace(t *testing.T) {
	spec, workspace, original := configurationFixture(t)
	ctx := context.Background()
	base := &upstreamTestRuntime{}
	working := filepath.Join(workspace, "repository", "recipe")
	run := func(operation string) error {
		return executeUpstreamJob(ctx, base, t.TempDir(), &upstreamRequest{Spec: *spec, Installation: workspace, Operation: operation}, io.Discard, io.Discard)
	}
	assertFile := func(name, expected string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(working, name))
		if err != nil || string(data) != expected {
			t.Fatalf("%s = %q, want %q: %v", name, data, expected, err)
		}
	}
	value := "changed ' value; $(touch injected); `touch backtick`; $HOME"
	configureFixtureValue(t, spec, original, value)
	if err := run("start"); err != nil {
		t.Fatal(err)
	}
	assertFile("started", value)
	assertFile("argv.txt", "original argument\n")
	assertFile("value.txt", "original environment\n")
	for _, name := range []string{"injected", "backtick"} {
		if _, err := os.Stat(filepath.Join(working, name)); !os.IsNotExist(err) {
			t.Fatalf("shell input executed: %s", name)
		}
	}
	first, err := os.ReadFile(filepath.Join(working, "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run("stop"); err != nil {
		t.Fatal(err)
	}
	if err := run("start"); err != nil {
		t.Fatal(err)
	}
	repeated, err := os.ReadFile(filepath.Join(working, "start.sh"))
	if err != nil || !bytes.Equal(first, repeated) {
		t.Fatal("identical restart changed configured source", err)
	}
	if err := run("stop"); err != nil {
		t.Fatal(err)
	}
	configureFixtureValue(t, spec, original, "second configured value")
	if err := run("start"); err != nil {
		t.Fatal(err)
	}
	assertFile("started", "second configured value")
	if err := run("stop"); err != nil {
		t.Fatal(err)
	}
	spec.Upstream.Configuration = nil
	if err := run("start"); err != nil {
		t.Fatal(err)
	}
	assertFile("started", "started\n")
	assertFile("start.sh", string(original))
	assertFile("tracked.py", "# exact pinned source\n"+strings.Repeat("# authored patch\n", 4))
	info, err := os.Stat(filepath.Join(working, "start.sh"))
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("configuration lost authored executable mode", err)
	}
}

func TestUpstreamConfigurationRejectsAllWritesOnHashOrSourceDrift(t *testing.T) {
	spec, workspace, original := configurationFixture(t)
	ctx := context.Background()
	repository := filepath.Join(workspace, "repository")
	if err := materializeUpstream(ctx, spec, repository, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	configureFixtureValue(t, spec, original, "new value")
	// The first valid file must not be changed when a later file is untrusted.
	spec.Upstream.Configuration = append(spec.Upstream.Configuration, sourceconfig.ResolvedFile{Path: "tracked.py", SHA256: strings.Repeat("0", 64), Edits: []sourceconfig.ResolvedEdit{{Start: 0, End: 1, Replacement: "#"}}})
	if err := configureUpstreamSource(ctx, spec, repository); err == nil || !strings.Contains(err.Error(), "hash_mismatch") {
		t.Fatalf("bad pinned hash accepted: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repository, "recipe/start.sh"))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("configuration partially applied before validation", err)
	}
	spec.Upstream.Configuration = spec.Upstream.Configuration[:1]
	if err := configureUpstreamSource(ctx, spec, repository); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repository, "recipe/start.sh")
	if err := os.WriteFile(path, append(got, []byte("# upstream modification\n")...), 0755); err != nil {
		t.Fatal(err)
	}
	// Supervised edits remain authoritative, including edits to configured text.
	if err := recordUpstreamSource(ctx, spec, repository); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpstreamSource(ctx, spec, repository); err != nil {
		t.Fatal(err)
	}
	if err := configureUpstreamSource(ctx, spec, repository); err != nil {
		t.Fatalf("supervised modification rejected: %v", err)
	}
	preserved, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(preserved, append(got, []byte("# upstream modification\n")...)) {
		t.Fatalf("supervised modification erased: %q, %v", preserved, err)
	}
	if err := os.WriteFile(path, append(preserved, []byte("# foreign modification\n")...), 0755); err != nil {
		t.Fatal(err)
	}
	if err := configureUpstreamSource(ctx, spec, repository); err == nil {
		t.Fatal("out-of-band source modification accepted")
	}
}

func TestUpstreamConfigurationRejectsSymlinkEscapes(t *testing.T) {
	spec, workspace, original := configurationFixture(t)
	ctx := context.Background()
	repository := filepath.Join(workspace, "repository")
	if err := materializeUpstream(ctx, spec, repository, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	configureFixtureValue(t, spec, original, "new value")
	outside := filepath.Join(t.TempDir(), "untouched.sh")
	if err := os.WriteFile(outside, original, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repository, "recipe/start.sh")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := configureUpstreamSource(ctx, spec, repository); err == nil {
		t.Fatal("symlink configuration target accepted")
	}
	data, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatal("outside source was modified", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.py", path); err != nil {
		t.Fatal(err)
	}
	if err := configureUpstreamSource(ctx, spec, repository); err == nil {
		t.Fatal("in-tree symlink confused pinned byte identity")
	}
}

func TestUpstreamSettingsSuccessorRetainsLegacyInstallation(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	base := &upstreamTestRuntime{}
	rt := upstreamTestWrapper(t, base, root)
	spec := upstreamTestSpec()
	legacy := rt.legacyWorkspace(spec)
	if err := os.MkdirAll(legacy, 0700); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(legacy, "retained-cache")
	if err := os.WriteFile(cache, []byte("downloaded"), 0600); err != nil {
		t.Fatal(err)
	}
	rec := &upstreamRecord{ID: "~lmw-upstream-legacy", Spec: *spec, Workspace: legacy, Observed: map[string]string{}}
	if err := os.MkdirAll(filepath.Join(rt.root, rec.ID), 0700); err != nil {
		t.Fatal(err)
	}
	if err := rt.save(rec); err != nil {
		t.Fatal(err)
	}
	rt = upstreamTestWrapper(t, base, root)
	spec.Name = "settings-successor"
	spec.Labels[LabelDeployment] = "successor-deployment"
	spec.Labels[LabelRecipe] = "sha256:" + strings.Repeat("b", 64)
	spec.Env = []string{"VALUE=changed"}
	id, err := rt.Create(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if rt.records[id].Workspace != legacy {
		t.Fatal("settings replacement abandoned legacy installation")
	}
	rt = upstreamTestWrapper(t, base, root)
	if rt.records[id].Workspace != legacy {
		t.Fatal("agent restart rejected inherited legacy workspace")
	}
	data, err := os.ReadFile(cache)
	if err != nil || string(data) != "downloaded" {
		t.Fatal("retained cache lost", err)
	}
}

func TestUpstreamEnvironmentOmissionRestoresAuthoredDefaults(t *testing.T) {
	working := t.TempDir()
	state := filepath.Join(working, "env-state.json")
	spec := upstreamTestSpec()
	spec.Upstream.EnvFile, spec.Upstream.EnvTemplate, spec.Upstream.EnvFormat = ".env", "env.template", "shell"
	original := "VALUE=upstream-default\n# retained comment\nUNRELATED=untouched\n"
	if err := os.WriteFile(filepath.Join(working, "env.template"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	spec.Env = []string{"VALUE=configured ' value", "EXTRA=temporary"}
	if err := configureUpstreamEnv(spec, working, state); err != nil {
		t.Fatal(err)
	}
	if err := configureUpstreamEnv(spec, working, state); err != nil {
		t.Fatal(err)
	}
	spec.Env = nil
	if err := configureUpstreamEnv(spec, working, state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(working, ".env"))
	if err != nil || string(data) != original {
		t.Fatalf("omission did not restore original environment: %q, %v", data, err)
	}
	spec.Env = []string{"VALUE=configured"}
	if err := configureUpstreamEnv(spec, working, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(working, ".env"), []byte("VALUE=changed-by-upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	spec.Env = nil
	if err := configureUpstreamEnv(spec, working, state); err == nil {
		t.Fatal("omission erased upstream environment edits")
	}
}

func TestUpstreamReconfigurationAcceptsAuthoredEnvironmentNormalization(t *testing.T) {
	repository, revision := upstreamFixture(t, "printf 'VALUE=%s\\n' \"$VALUE\" > .env\nprintf '%s\\n' \"$VALUE\" > prepared-value.txt")
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL, spec.Upstream.Revision = repository, revision
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	spec.Upstream.Start = []string{"./start.sh", "argument"}
	spec.Upstream.EnvFile, spec.Upstream.EnvFormat = ".env", "shell"
	spec.Env = []string{"VALUE=8"}
	workspace := t.TempDir()
	run := func(operation string) {
		t.Helper()
		if err := executeUpstreamJob(context.Background(), &upstreamTestRuntime{}, t.TempDir(), &upstreamRequest{Spec: *spec, Installation: workspace, Operation: operation}, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	run("start")
	run("stop")
	spec.Env = []string{"VALUE=9"}
	run("start")
	working := filepath.Join(workspace, "repository", "recipe")
	value, err := os.ReadFile(filepath.Join(working, "value.txt"))
	if err != nil || string(value) != "9\n" {
		t.Fatalf("restarted runtime did not receive changed value: %q, %v", value, err)
	}
	prepared, err := os.ReadFile(filepath.Join(working, "prepared-value.txt"))
	if err != nil || string(prepared) != "9\n" {
		t.Fatalf("preparation did not use changed runtime inputs: %q, %v", prepared, err)
	}
}
