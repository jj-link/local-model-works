//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/sourceconfig"
)

func preservationGit(t *testing.T, repository string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repository}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	data, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, data, err)
	}
	return strings.TrimSpace(string(data))
}

func preservationWrite(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
}

func preservationBinding(t *testing.T, spec *ContainerSpec, original, value string) {
	t.Helper()
	start := strings.Index(original, "SETTING=") + len("SETTING=")
	if start < len("SETTING=") {
		t.Fatal("missing fixture setting")
	}
	end := start + strings.IndexByte(original[start:], '\n')
	spec.Upstream.Configuration = []sourceconfig.ResolvedFile{{Path: "settings.conf", SHA256: sourceBytesHash([]byte(original)), Edits: []sourceconfig.ResolvedEdit{{Start: start, End: end, Replacement: value}}}}
}

func preservationFixture(t *testing.T) (*upstreamRuntime, *upstreamRecord, *ContainerSpec, string) {
	t.Helper()
	installer := "sed -i 's/LOCAL=original/LOCAL=installed/; s/OVERLAP=original/OVERLAP=installed/' settings.conf\n" +
		"printf 'custom: yes\\n' > local.yaml\nprintf '# staged installation source\\n' > staged.asset\ngit add staged.asset\n" +
		"rm removed.py\nchmod 600 mode.conf\nmkdir -p .cache\nprintf '# disposable cache\\n' > .cache/generated.py\nprintf 'LOCAL_ENV=installed\\n' >> .env\n"
	repository, revision := upstreamFixture(t, installer)
	preservationGit(t, repository, "checkout", "--detach", revision)
	original := "SETTING=default\n# setting boundary\n# another boundary\nLOCAL=original\n# local boundary\n# another boundary\nOVERLAP=original\n# overlap boundary\n# another boundary\nUPSTREAM=original\n"
	preservationWrite(t, filepath.Join(repository, "recipe/settings.conf"), original, 0644)
	preservationWrite(t, filepath.Join(repository, "recipe/removed.py"), "# original removable source\n", 0644)
	preservationWrite(t, filepath.Join(repository, "recipe/mode.conf"), "mode=source\n", 0644)
	preservationGit(t, repository, "add", ".")
	preservationGit(t, repository, "-c", "commit.gpgsign=false", "commit", "-m", "installation fixture")
	spec := upstreamTestSpec()
	spec.Upstream.SourceURL = repository
	spec.Upstream.Revision = preservationGit(t, repository, "rev-parse", "HEAD")
	spec.Upstream.Install = [][]string{{"./install.sh"}}
	spec.Upstream.Start = []string{"./start.sh", "fixture"}
	spec.Env = []string{"VALUE=saved"}
	spec.Upstream.EnvFile, spec.Upstream.EnvTemplate, spec.Upstream.EnvFormat = ".env", "env.template", "shell"
	preservationBinding(t, spec, original, "saved")
	rt := upstreamTestWrapper(t, &upstreamTestRuntime{}, t.TempDir())
	id, err := rt.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	previous := rt.records[id]
	if err := executeUpstreamJob(context.Background(), &upstreamTestRuntime{}, t.TempDir(), &upstreamRequest{Spec: *spec, Installation: previous.Workspace, Operation: "start"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	incoming := "# new release header\n# offsets moved\n" + strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(original, "SETTING=default", "SETTING=new-default"), "OVERLAP=original", "OVERLAP=upstream"), "UPSTREAM=original", "UPSTREAM=new")
	preservationWrite(t, filepath.Join(repository, "recipe/settings.conf"), incoming, 0644)
	preservationWrite(t, filepath.Join(repository, "recipe/upstream.yaml"), "release: next\n", 0644)
	preservationWrite(t, filepath.Join(repository, "recipe/removed.py"), "# upstream changed removable source\n", 0644)
	preservationWrite(t, filepath.Join(repository, "recipe/env.template"), "VALUE=new-default\nUNCHANGED=new-upstream-default\n", 0755)
	preservationGit(t, repository, "add", ".")
	preservationGit(t, repository, "-c", "commit.gpgsign=false", "commit", "-m", "successor fixture")
	var next ContainerSpec
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	next.Name += "-next"
	next.Upstream.Revision = preservationGit(t, repository, "rev-parse", "HEAD")
	preservationBinding(t, &next, incoming, "saved")
	return rt, previous, &next, incoming
}

func TestUpstreamPreparationPreservesInstallerChangesAndNewBindings(t *testing.T) {
	rt, previous, next, incoming := preservationFixture(t)
	ctx := context.Background()
	oldRepository := filepath.Join(previous.Workspace, "repository")
	before, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareUpstreamSource(ctx, rt, next); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(rt.workspace(next), "repository")
	assertFile := func(name, want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(repository, "recipe", name))
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, want %q: %v", name, got, want, err)
		}
	}
	want := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(incoming, "SETTING=new-default", "SETTING=saved"), "LOCAL=original", "LOCAL=installed"), "OVERLAP=upstream", "OVERLAP=installed")
	assertFile("settings.conf", want)
	assertFile("local.yaml", "custom: yes\n")
	assertFile("staged.asset", "# staged installation source\n")
	assertFile("upstream.yaml", "release: next\n")
	assertFile(".env", "VALUE='saved'\nUNCHANGED=new-upstream-default\nLOCAL_ENV=installed\n")
	assertFile("tracked.py", "# exact pinned source\n# authored patch\n")
	for _, name := range []string{"removed.py", ".cache/generated.py"} {
		if _, err := os.Lstat(filepath.Join(repository, "recipe", name)); !os.IsNotExist(err) {
			t.Fatalf("unexpected preserved %s: %v", name, err)
		}
	}
	info, err := os.Stat(filepath.Join(repository, "recipe/mode.conf"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("installation mode lost: %v, %v", info, err)
	}
	// Reconfiguration uses the new offsets, retaining the authored patch even
	// though it shares a file with the owned setting and upstream modifications.
	preservationBinding(t, next, incoming, "changed-again")
	if err := configureUpstreamSource(ctx, next, repository); err != nil {
		t.Fatal(err)
	}
	assertFile("settings.conf", strings.ReplaceAll(want, "SETTING=saved", "SETTING=changed-again"))
	next.Upstream.Configuration = nil
	if err := configureUpstreamSource(ctx, next, repository); err != nil {
		t.Fatal(err)
	}
	assertFile("settings.conf", strings.ReplaceAll(want, "SETTING=saved", "SETTING=new-default"))
	after, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("old installation was modified: %v", err)
	}
	if err := verifyUpstreamSource(ctx, next, repository); err != nil {
		t.Fatal(err)
	}
	preservationWrite(t, filepath.Join(repository, "recipe/staged.asset"), "foreign modification", 0644)
	if err := verifyUpstreamSource(ctx, next, repository); err == nil {
		t.Fatal("carried tracked addition lost integrity coverage after reconfiguration")
	}
	base := rt.Runtime.(*upstreamTestRuntime)
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("source preparation changed container lifecycle")
	}
}

func TestUpstreamFailedPreparationRetainsPriorInstallation(t *testing.T) {
	rt, previous, next, _ := preservationFixture(t)
	ctx := context.Background()
	oldRepository := filepath.Join(previous.Workspace, "repository")
	before, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
	if err != nil {
		t.Fatal(err)
	}
	// The new checkout and merges succeed, then the new binding hash fails.
	// Nothing can have been published or modified in the prior installation.
	next.Upstream.Configuration[0].SHA256 = strings.Repeat("0", 64)
	if err := PrepareUpstreamSource(ctx, rt, next); err == nil {
		t.Fatal("invalid successor binding accepted")
	}
	after, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed staging changed prior source: %v", err)
	}
	if _, err := os.Stat(rt.workspace(next)); !os.IsNotExist(err) {
		t.Fatalf("failed staging published successor: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(previous.Workspace))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".prepare-") {
			t.Fatal("failed preparation left staged source")
		}
	}
}

func TestUpstreamPreparationRejectsUnauthenticatedAndEscapingChanges(t *testing.T) {
	for _, scenario := range []string{"untracked-tamper", "escaping-symlink", "binary-overlap"} {
		t.Run(scenario, func(t *testing.T) {
			rt, previous, next, _ := preservationFixture(t)
			ctx := context.Background()
			oldRepository := filepath.Join(previous.Workspace, "repository")
			switch scenario {
			case "untracked-tamper":
				preservationWrite(t, filepath.Join(oldRepository, "recipe/local.yaml"), "foreign: edit\n", 0644)
			case "escaping-symlink":
				if err := os.Symlink("../../../../outside.conf", filepath.Join(oldRepository, "recipe/escape.conf")); err != nil {
					t.Fatal(err)
				}
				if err := recordUpstreamSource(ctx, &previous.Spec, oldRepository); err != nil {
					t.Fatal(err)
				}
			case "binary-overlap":
				preservationWrite(t, filepath.Join(oldRepository, "recipe/settings.conf"), "local\x00binary", 0644)
				if err := recordUpstreamSource(ctx, &previous.Spec, oldRepository); err != nil {
					t.Fatal(err)
				}
			}
			before, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
			if err != nil {
				t.Fatal(err)
			}
			if err := PrepareUpstreamSource(ctx, rt, next); err == nil {
				t.Fatal("unsupported preservation accepted")
			}
			after, err := upstreamSourceFingerprint(ctx, &previous.Spec, oldRepository)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected preparation changed prior source: %v", err)
			}
		})
	}
}

func TestUpstreamTextMergeRetainsLocalOverlapAndIndependentUpstreamHunk(t *testing.T) {
	base := []byte("one=base\nlast=base\n")
	local := []byte("one=local\nlast=base\n")
	incoming := []byte("one=upstream\nlast=upstream\n")
	result, err := sourceconfig.MergeLocalChanges(context.Background(), t.TempDir(), base, local, incoming)
	if err != nil || !bytes.Equal(result, []byte("one=local\nlast=upstream\n")) {
		t.Fatalf("local-first merge lost independent changes: %q, %v", result, err)
	}
}

func TestUpstreamPreparationPreservesConfiguredFileDeletion(t *testing.T) {
	rt, previous, next, incoming := preservationFixture(t)
	ctx := context.Background()
	oldRepository := filepath.Join(previous.Workspace, "repository")
	if err := os.Remove(filepath.Join(oldRepository, "recipe/settings.conf")); err != nil {
		t.Fatal(err)
	}
	if err := recordUpstreamSource(ctx, &previous.Spec, oldRepository); err != nil {
		t.Fatal(err)
	}
	if err := PrepareUpstreamSource(ctx, rt, next); err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(rt.workspace(next), "repository")
	preservationBinding(t, next, incoming, "later-value")
	if err := configureUpstreamSource(ctx, next, repository); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "recipe/settings.conf")); !os.IsNotExist(err) {
		t.Fatalf("configuration resurrected an authored deletion: %v", err)
	}
}

func TestUpstreamDiscoveryRequiresHistoricalSourceEvidence(t *testing.T) {
	rt, previous, _, _ := preservationFixture(t)
	previous.Removed, previous.Superseded = true, true
	specs, err := RetainedUpstreamSpecs(rt, previous.Spec.Upstream.SourceURL, previous.Spec.Upstream.SourcePath)
	if err != nil || len(specs) != 1 || specs[0].Upstream.Revision != previous.Spec.Upstream.Revision {
		t.Fatalf("retained installation disappeared from discovery: %+v, %v", specs, err)
	}
	if err := os.Remove(filepath.Join(previous.Workspace, "source-state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainedUpstreamSpecs(rt, previous.Spec.Upstream.SourceURL, previous.Spec.Upstream.SourcePath); err == nil {
		t.Fatal("missing historical source evidence silently became an empty plan")
	}
}

func TestUpstreamPackagePreparationDoesNotReconfigureExistingSource(t *testing.T) {
	rt, previous, _, _ := preservationFixture(t)
	ctx := context.Background()
	repository := filepath.Join(previous.Workspace, "repository")
	previous.Armed = true
	previous.Observed["authored"] = "running-installation"
	var target ContainerSpec
	data, err := json.Marshal(previous.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &target); err != nil {
		t.Fatal(err)
	}
	original, err := pinnedConfigurationBytes(ctx, repository, target.Upstream.Revision, "recipe/settings.conf")
	if err != nil {
		t.Fatal(err)
	}
	preservationBinding(t, &target, string(original), "new-package-setting")
	target.Env = []string{"VALUE=new-package-environment"}
	before, err := upstreamSourceFingerprint(ctx, &previous.Spec, repository)
	if err != nil {
		t.Fatal(err)
	}
	metadata := make(map[string][]byte)
	for _, path := range []string{
		filepath.Join(repository, ".git/index"),
		filepath.Join(previous.Workspace, "source-state.json"),
		filepath.Join(previous.Workspace, "configuration-state.json"),
		filepath.Join(previous.Workspace, "env-state.json"),
		filepath.Join(previous.Workspace, "installed.json"),
		filepath.Join(previous.Workspace, "source-identity.json"),
		filepath.Join(rt.root, previous.ID, "record.json"),
	} {
		metadata[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareUpstreamSource(ctx, rt, &target); err != nil {
		t.Fatal(err)
	}
	after, err := upstreamSourceFingerprint(ctx, &previous.Spec, repository)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("package update changed an existing installation's source: %v", err)
	}
	for path, expected := range metadata {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("package update changed retained metadata %s: %v", path, err)
		}
	}
	base := rt.Runtime.(*upstreamTestRuntime)
	if len(base.started) != 0 || len(base.stopped) != 0 || len(base.removed) != 0 {
		t.Fatal("same-source package preparation changed container lifecycle")
	}
}
