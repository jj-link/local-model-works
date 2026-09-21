package agent

import (
	"crypto/tls"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	"github.com/jj-link/local-model-works/internal/recipe"
	"github.com/jj-link/local-model-works/internal/runtime"
	"github.com/jj-link/local-model-works/internal/sourceconfig"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func installationUpdateFixture(t *testing.T) (*recipe.Manifest, *recipe.PackResult, recipe.InstallationUpdateSpec) {
	t.Helper()
	origin := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = origin
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "--quiet")
	if err := os.WriteFile(filepath.Join(origin, "upstream.txt"), []byte("pinned source\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--quiet", "-m", "source")
	revision := git("rev-parse", "HEAD")
	manifest := &recipe.Manifest{}
	manifest.Metadata.Source = &recipe.Source{URL: origin, Revision: revision}
	manifest.Compatibility.NodeCount = 1
	manifest.Workloads = []recipe.Workload{{Permissions: []string{"host.upstream-exec"}, Upstream: &recipe.UpstreamExecution{Install: [][]string{{"sh", "-c", "touch authored-install-ran"}}, Start: []string{"sh", "-c", "touch authored-start-ran"}, Stop: []string{"sh", "-c", "touch authored-stop-ran"}, Containers: []string{"authored-model"}}}}
	doc, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := recipe.PackManifest(doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	spec := runtime.ContainerSpec{Name: "logical-installation", Labels: runtime.ManagedLabels("deployment", "run", packed.ManifestDigest, "1", 0, "serving"), Upstream: &runtime.UpstreamSpec{SourceURL: origin, Revision: revision, Approved: true, Install: manifest.Workloads[0].Upstream.Install, Start: manifest.Workloads[0].Upstream.Start, Stop: manifest.Workloads[0].Upstream.Stop, Containers: []string{"authored-model"}}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, packed, recipe.InstallationUpdateSpec{Spec: raw, Workload: 0, Rank: 0}
}

func installationUpdateAgent(t *testing.T, packed *recipe.PackResult) (*Agent, *ownershipRuntime, string) {
	t.Helper()
	layout := t.TempDir()
	if err := recipe.WriteLayout(layout, packed); err != nil {
		t.Fatal(err)
	}
	layer, err := os.ReadFile(filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(packed.LayerDigest, "sha256:")))
	if err != nil {
		t.Fatal(err)
	}
	certificateAuthority, err := ca.New()
	if err != nil {
		t.Fatal(err)
	}
	cert, key, _, err := certificateAuthority.ServerCert([]string{"localhost"}, []net.IP{net.ParseIP("127.0.0.1")}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch filepath.Base(r.URL.Path) {
		case "manifest":
			body = packed.ManifestJSON
		case "config":
			body = packed.ConfigJSON
		case "layer":
			body = layer
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	server.StartTLS()
	t.Cleanup(server.Close)
	base := &ownershipRuntime{}
	runtimeRoot := t.TempDir()
	rt, err := runtime.WithUpstream(base, runtimeRoot, true)
	if err != nil {
		t.Fatal(err)
	}
	a := New(config.Agent{StateRoot: t.TempDir(), ServerURL: server.URL, CASha256: certificateAuthority.Fingerprint()}, "test", "test", rt, nil)
	a.caPEM = certificateAuthority.PEMCert()
	return a, base, runtimeRoot
}

func preparedSourcePaths(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), "authored-") && strings.HasSuffix(entry.Name(), "-ran") {
			t.Errorf("update executed authored lifecycle: %s", path)
		}
		if entry.Name() == "upstream.txt" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(data) != "pinned source\n" {
				t.Errorf("unverified source: %q", data)
			}
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestRecipeUpdateFetchesAndPreparesWithoutLifecycle(t *testing.T) {
	_, packed, input := installationUpdateFixture(t)
	payload, err := json.Marshal([]recipe.InstallationUpdateSpec{input})
	if err != nil {
		t.Fatal(err)
	}
	for _, cached := range []bool{false, true} {
		name := "fresh"
		if cached {
			name = "cached"
		}
		t.Run(name, func(t *testing.T) {
			a, base, root := installationUpdateAgent(t, packed)
			if cached {
				a.handleArtifact(t.Context(), &agentv1.ArtifactCommand{CommandId: "fetch", Op: agentv1.ArtifactOp_ARTIFACT_OP_FETCH, ArtifactIdentity: "recipe://" + packed.ManifestDigest})
				if result := awaitDownloadResult(t, a, "fetch"); !result.Ok {
					t.Fatal(result.Error)
				}
				if paths := preparedSourcePaths(t, root); len(paths) != 0 {
					t.Fatalf("ordinary fetch prepared source: %v", paths)
				}
			}
			a.handleArtifact(t.Context(), &agentv1.ArtifactCommand{CommandId: "update", Op: agentv1.ArtifactOp_ARTIFACT_OP_UPDATE_RECIPE, ArtifactIdentity: "recipe://" + packed.ManifestDigest, UpstreamSpecs: payload})
			if result := awaitDownloadResult(t, a, "update"); !result.Ok {
				t.Fatal(result.Error)
			}
			if paths := preparedSourcePaths(t, root); len(paths) != 1 {
				t.Fatalf("update did not eagerly prepare exactly one target checkout: %v", paths)
			}
			if len(base.calls) != 0 {
				t.Fatalf("update touched models: %v", base.calls)
			}
			path := filepath.Join(a.cfg.StateRoot, "recipes", strings.TrimPrefix(packed.ManifestDigest, "sha256:"))
			if installed, err := recipe.ReadLayout(path); err != nil || installed.ManifestDigest != packed.ManifestDigest {
				t.Fatalf("verified target package not installed: %v", err)
			}
		})
	}
}

func TestRecipeUpdateRejectsInjectedTargetContracts(t *testing.T) {
	manifest, packed, input := installationUpdateFixture(t)
	for name, mutate := range map[string]func(*runtime.ContainerSpec){
		"source":    func(s *runtime.ContainerSpec) { s.Upstream.Revision = strings.Repeat("f", 40) },
		"command":   func(s *runtime.ContainerSpec) { s.Upstream.Start = []string{"arbitrary-executable"} },
		"ownership": func(s *runtime.ContainerSpec) { s.Upstream.Containers = []string{"unrelated-model"} },
		"rank":      func(s *runtime.ContainerSpec) { s.Labels[runtime.LabelRank] = "1" },
		"configuration": func(s *runtime.ContainerSpec) {
			s.Upstream.Configuration = []sourceconfig.ResolvedFile{{Path: "upstream.txt", SHA256: strings.Repeat("f", 64), Edits: []sourceconfig.ResolvedEdit{{Start: 0, End: 1, Replacement: "arbitrary code"}}}}
		},
		"container-command": func(s *runtime.ContainerSpec) { s.Cmd = []string{"arbitrary-executable"} },
	} {
		t.Run(name, func(t *testing.T) {
			var spec runtime.ContainerSpec
			if err := json.Unmarshal(input.Spec, &spec); err != nil {
				t.Fatal(err)
			}
			mutate(&spec)
			changed := input
			var err error
			changed.Spec, err = json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validateInstallationUpdate(manifest, packed.ManifestDigest, []recipe.InstallationUpdateSpec{changed}); err == nil {
				t.Fatal("accepted contract not authenticated by target package")
			}
		})
	}
	// Exercise rejection through the real acquisition path as well: receiving
	// valid package bytes is not enough to acknowledge a malformed update.
	a, base, root := installationUpdateAgent(t, packed)
	input.Rank = 7
	payload, _ := json.Marshal([]recipe.InstallationUpdateSpec{input})
	a.handleArtifact(t.Context(), &agentv1.ArtifactCommand{CommandId: "bad-update", Op: agentv1.ArtifactOp_ARTIFACT_OP_UPDATE_RECIPE, ArtifactIdentity: "recipe://" + packed.ManifestDigest, UpstreamSpecs: payload})
	if result := awaitDownloadResult(t, a, "bad-update"); result.Ok {
		t.Fatal("invalid update reported success")
	}
	if len(preparedSourcePaths(t, root)) != 0 || len(base.calls) != 0 {
		t.Fatal("invalid update mutated source or models")
	}
}

func TestRecipeUpdateSupportsNativePackageWithoutUpstreamSpecs(t *testing.T) {
	manifest := &recipe.Manifest{Workloads: []recipe.Workload{{Image: recipe.Image{Reference: "native-image"}}}}
	doc, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := recipe.PackManifest(doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, base, root := installationUpdateAgent(t, packed)
	a.handleArtifact(t.Context(), &agentv1.ArtifactCommand{CommandId: "native-update", Op: agentv1.ArtifactOp_ARTIFACT_OP_UPDATE_RECIPE, ArtifactIdentity: "recipe://" + packed.ManifestDigest, UpstreamSpecs: []byte("[]")})
	if result := awaitDownloadResult(t, a, "native-update"); !result.Ok {
		t.Fatal(result.Error)
	}
	if len(preparedSourcePaths(t, root)) != 0 || len(base.calls) != 0 {
		t.Fatal("native package update performed model operations")
	}
	if _, err := recipe.ReadLayout(filepath.Join(a.cfg.StateRoot, "recipes", strings.TrimPrefix(packed.ManifestDigest, "sha256:"))); err != nil {
		t.Fatal(err)
	}
}
