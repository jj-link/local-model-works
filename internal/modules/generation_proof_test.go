package modules_test

// TestModuleGenerationProof proves the first-party module contract end to
// end against a fixture tree, without touching the real modules/ directory:
//
//  1. Generating with a new module tree (modules/zz-fixture) makes the
//     fixture appear in the generated registry
//     (internal/modules/registry_gen.go) — the exact slice that
//     /api/v1/modules serves (internal/server/handlers_system.go serves
//     modules.Registry directly; there is no core switch or list) — with its
//     id, title, route, apiVersion, jobKinds, and settingsSchema.
//  2. The generated backends list (backends_gen.go) registers the fixture's
//     New constructor. The generated Go compiles: the test builds and runs
//     the generated registry + constructors in an isolated module (stub
//     moduleapi) and asserts the fixture's descriptor in the served value.
//  3. The fixture's frozen descriptor (modules/zz-fixture/backend/
//     descriptor_gen.go) is emitted.
//  4. The oapi generator merges the fixture's api.yaml fragment into the
//     public contract; the fixture's operation appears in the merged doc.
//  5. Deleting the module dir and regenerating removes every surface.
//  6. A manifest violating schemas/module/v1alpha1.schema.json (or a missing
//     API fragment) fails generation with a nonzero exit and a clear error.
//
// Frontend surface: web/ derives its module routes and navigation at
// runtime from GET /api/v1/modules (web/app/lib/api/index.ts
// listModules -> /modules), so a fixture present in the served registry is
// present in the frontend without any web/ regeneration or edit.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureManifest = `apiVersion: localmodelworks/v1alpha1
kind: Module
id: zz-fixture
title: ZZ Fixture
route: /zz-fixture
nav:
  label: ZZ Fixture
  order: 999
  icon: zz-fixture
settingsSchema:
  $schema: https://json-schema.org/draft/2020-12/schema
  type: object
  additionalProperties: false
  properties:
    demo_int:
      type: integer
      minimum: 1
      maximum: 10
jobKinds:
  - zz-job
apiFragment: api.yaml
capabilities:
  - events.publish
`

const alphaManifest = `apiVersion: localmodelworks/v1alpha1
kind: Module
id: zz-alpha
title: ZZ Alpha
route: /zz-alpha
nav:
  label: ZZ Alpha
  order: 998
  icon: zz-alpha
settingsSchema:
  $schema: https://json-schema.org/draft/2020-12/schema
  type: object
  additionalProperties: false
  properties: {}
apiFragment: api.yaml
capabilities:
  - events.publish
`

const fixtureFragment = `openapi: 3.1.0
info:
  title: Local Model Works zz-fixture fragment
  version: 1.0.0
tags:
  - name: zz-fixture
paths:
  /zz-fixture/echo:
    get:
      tags: [zz-fixture]
      operationId: zzFixtureEcho
      responses:
        '200':
          description: Echo
          content:
            application/json:
              schema:
                type: object
                additionalProperties: true
`

const alphaFragment = `openapi: 3.1.0
info:
  title: Local Model Works zz-alpha fragment
  version: 1.0.0
tags:
  - name: zz-alpha
paths:
  /zz-alpha/ping:
    get:
      tags: [zz-alpha]
      operationId: zzAlphaPing
      responses:
        '200':
          description: Ping
          content:
            application/json:
              schema:
                type: string
`

// fixtureBackend is the minimal Go backend a first-party module ships; the
// generator's registration contract is exactly "export New".
const fixtureBackend = `// Package backend implements the %s test-fixture module.
package backend

import "github.com/jj-link/local-model-works/internal/moduleapi"

// Module is a minimal first-party module.
type Module struct{ env *moduleapi.Env }

// New is the constructor the generated backends list registers.
func New(env *moduleapi.Env) moduleapi.Module { return &Module{env: env} }
`

// moduleapiStub stands in for the real internal/moduleapi in the isolated
// probe module: only the types the generated files reference.
const moduleapiStub = `// Package moduleapi is a minimal stand-in for the real internal/moduleapi,
// providing exactly the types the generated registry/descriptor files use.
package moduleapi

import "encoding/json"

// Nav is the sidebar entry for a module.
type Nav struct {
	Label string ` + "`" + `json:"label"` + "`" + `
	Order int    ` + "`" + `json:"order"` + "`" + `
	Icon  string ` + "`" + `json:"icon,omitempty"` + "`" + `
}

// Descriptor is the frozen module manifest.
type Descriptor struct {
	APIVersion     string          ` + "`" + `json:"apiVersion"` + "`" + `
	Kind           string          ` + "`" + `json:"kind"` + "`" + `
	ID             string          ` + "`" + `json:"id"` + "`" + `
	Title          string          ` + "`" + `json:"title"` + "`" + `
	Route          string          ` + "`" + `json:"route"` + "`" + `
	Nav            Nav             ` + "`" + `json:"nav"` + "`" + `
	SettingsSchema json.RawMessage ` + "`" + `json:"settingsSchema,omitempty"` + "`" + `
	JobKinds       []string        ` + "`" + `json:"jobKinds,omitempty"` + "`" + `
	ArtifactKinds  []string        ` + "`" + `json:"artifactKinds,omitempty"` + "`" + `
	APIFragment    string          ` + "`" + `json:"apiFragment,omitempty"` + "`" + `
	Capabilities   []string        ` + "`" + `json:"capabilities,omitempty"` + "`" + `
}

// Env is the (stubbed) core service surface handed to modules.
type Env struct{}

// Module is the first-party module contract.
type Module interface{}
`

const probeMain = `// Command modprobe prints the generated module registry and constructor
// count as JSON — the same value internal/server serves at /api/v1/modules.
package main

import (
	"encoding/json"
	"fmt"

	"github.com/jj-link/local-model-works/internal/modules"
)

func main() {
	out, err := json.Marshal(map[string]any{
		"registry":     modules.Registry,
		"constructors": len(modules.Constructors),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(string(out))
}
`

func TestModuleGenerationProof(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	root := t.TempDir()

	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	copyFile := func(rel, repoRel string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repoRoot, repoRel))
		if err != nil {
			t.Fatalf("read %s: %v", repoRel, err)
		}
		write(rel, string(data))
	}

	// Isolated module tree mirroring the repo layout the generator expects.
	write("go.mod", "module github.com/jj-link/local-model-works\n\ngo 1.26\n")
	write("cmd/modprobe/main.go", probeMain)
	write("internal/moduleapi/stub.go", moduleapiStub)
	copyFile("internal/modules/manifests.go", "internal/modules/manifests.go")
	copyFile("api/openapi.yaml", "api/openapi.yaml")
	copyFile("schemas/module/v1alpha1.schema.json", "schemas/module/v1alpha1.schema.json")
	addModule := func(id, manifest, fragment string) {
		t.Helper()
		write(filepath.Join("modules", id, "module.yaml"), manifest)
		write(filepath.Join("modules", id, "api.yaml"), fragment)
		write(filepath.Join("modules", id, "backend", "module.go"),
			fmt.Sprintf(fixtureBackend, id))
	}
	addModule("zz-alpha", alphaManifest, alphaFragment)
	addModule("zz-fixture", fixtureManifest, fixtureFragment)

	gen := func(t *testing.T, bin string) (string, error) {
		t.Helper()
		cmd := exec.Command("go", "run", bin, "gen", root)
		cmd.Dir = repoRoot
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("generation-adds-fixture", func(t *testing.T) {
		out, err := gen(t, "./internal/generate/modules")
		if err != nil {
			t.Fatalf("modules gen: %v\n%s", err, out)
		}
		reg, err := os.ReadFile(filepath.Join(root, "internal", "modules", "registry_gen.go"))
		if err != nil {
			t.Fatalf("registry: %v", err)
		}
		for _, want := range []string{
			`ID: "zz-fixture"`,
			`Title: "ZZ Fixture"`,
			`Route: "/zz-fixture"`,
			`APIVersion: "localmodelworks/v1alpha1"`,
			`JobKinds: []string{"zz-job"}`,
			"demo_int",
		} {
			if !strings.Contains(string(reg), want) {
				t.Fatalf("registry_gen.go missing %s", want)
			}
		}
		bk, err := os.ReadFile(filepath.Join(root, "internal", "modules", "backends_gen.go"))
		if err != nil {
			t.Fatalf("backends: %v", err)
		}
		for _, want := range []string{
			`zz_fixture "github.com/jj-link/local-model-works/modules/zz-fixture/backend"`,
			"zz_fixture.New",
		} {
			if !strings.Contains(string(bk), want) {
				t.Fatalf("backends_gen.go missing %s", want)
			}
		}
		desc, err := os.ReadFile(filepath.Join(root, "modules", "zz-fixture", "backend", "descriptor_gen.go"))
		if err != nil {
			t.Fatalf("descriptor: %v", err)
		}
		if !strings.Contains(string(desc), `ID: "zz-fixture"`) || !strings.Contains(string(desc), `zz-job`) {
			t.Fatalf("descriptor_gen.go missing fixture manifest: %s", desc)
		}
	})

	t.Run("generated-registry-serves-fixture", func(t *testing.T) {
		// Compile and run the generated registry + constructors: this is
		// the exact data /api/v1/modules returns (writeJSON(modules.Registry)).
		cmd := exec.Command("go", "run", "./cmd/modprobe")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe: %v\n%s", err, out)
		}
		var probe struct {
			Registry []struct {
				ID             string          `json:"id"`
				Title          string          `json:"title"`
				Route          string          `json:"route"`
				APIVersion     string          `json:"apiVersion"`
				JobKinds       []string        `json:"jobKinds"`
				SettingsSchema json.RawMessage `json:"settingsSchema"`
			} `json:"registry"`
			Constructors int `json:"constructors"`
		}
		if err := json.Unmarshal(out, &probe); err != nil {
			t.Fatalf("probe output: %v\n%s", err, out)
		}
		if probe.Constructors != len(probe.Registry) || probe.Constructors != 2 {
			t.Fatalf("constructors %d != registry %d (want 2 aligned)", probe.Constructors, len(probe.Registry))
		}
		var found bool
		for _, m := range probe.Registry {
			if m.ID != "zz-fixture" {
				continue
			}
			found = true
			if m.Title != "ZZ Fixture" || m.Route != "/zz-fixture" || m.APIVersion != "localmodelworks/v1alpha1" {
				t.Fatalf("served fixture descriptor wrong: %+v", m)
			}
			if len(m.JobKinds) != 1 || m.JobKinds[0] != "zz-job" {
				t.Fatalf("served fixture jobKinds wrong: %v", m.JobKinds)
			}
			if !strings.Contains(string(m.SettingsSchema), "demo_int") {
				t.Fatalf("served fixture settingsSchema wrong: %s", m.SettingsSchema)
			}
		}
		if !found {
			t.Fatalf("zz-fixture not in served registry: %+v", probe.Registry)
		}
	})

	t.Run("oapi-merges-fixture-fragment", func(t *testing.T) {
		out, err := gen(t, "./internal/generate/oapi")
		if err != nil {
			t.Fatalf("oapi gen: %v\n%s", err, out)
		}
		pub, err := os.ReadFile(filepath.Join(root, "api", "openapi.public.yaml"))
		if err != nil {
			t.Fatalf("public contract: %v", err)
		}
		for _, want := range []string{"zzFixtureEcho", "/zz-fixture/echo", "zz-fixture"} {
			if !strings.Contains(string(pub), want) {
				t.Fatalf("merged public contract missing %s", want)
			}
		}
	})

	t.Run("removal-clears-all-surfaces", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Join(root, "modules", "zz-fixture")); err != nil {
			t.Fatalf("remove fixture: %v", err)
		}
		out, err := gen(t, "./internal/generate/modules")
		if err != nil {
			t.Fatalf("modules gen after removal: %v\n%s", err, out)
		}
		reg, _ := os.ReadFile(filepath.Join(root, "internal", "modules", "registry_gen.go"))
		bk, _ := os.ReadFile(filepath.Join(root, "internal", "modules", "backends_gen.go"))
		if strings.Contains(string(reg), "zz-fixture") {
			t.Fatalf("registry still references zz-fixture after removal")
		}
		if strings.Contains(string(bk), "zz_fixture") || strings.Contains(string(bk), "zz-fixture") {
			t.Fatalf("backends still reference zz-fixture after removal")
		}
		if _, err := os.Stat(filepath.Join(root, "modules", "zz-fixture", "backend", "descriptor_gen.go")); !os.IsNotExist(err) {
			t.Fatalf("descriptor_gen.go must be gone with the module")
		}
		cmd := exec.Command("go", "run", "./cmd/modprobe")
		cmd.Dir = root
		pout, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe after removal: %v\n%s", err, pout)
		}
		if strings.Contains(string(pout), "zz-fixture") || strings.Contains(string(pout), "zz_fixture") {
			t.Fatalf("served registry still contains zz-fixture after removal")
		}
		out, err = gen(t, "./internal/generate/oapi")
		if err != nil {
			t.Fatalf("oapi gen after removal: %v\n%s", err, out)
		}
		pub, _ := os.ReadFile(filepath.Join(root, "api", "openapi.public.yaml"))
		if strings.Contains(string(pub), "zzFixtureEcho") || strings.Contains(string(pub), "/zz-fixture/echo") {
			t.Fatalf("merged contract still contains the fixture fragment")
		}
	})

	t.Run("schema-violation-fails-generation", func(t *testing.T) {
		bad := strings.Replace(fixtureManifest, "apiVersion: localmodelworks/v1alpha1", "apiVersion: localmodelworks/v9", 1)
		bad = strings.Replace(bad, "id: zz-fixture", "id: zz-bad", 1)
		addModule("zz-bad", bad, fixtureFragment)
		out, err := gen(t, "./internal/generate/modules")
		if err == nil {
			t.Fatalf("generation must fail on a schema-violating manifest")
		}
		if !strings.Contains(out, "zz-bad") || !strings.Contains(strings.ToLower(out), "invalid") {
			t.Fatalf("expected a clear zz-bad validation error, got: %s", out)
		}
	})

	t.Run("missing-fragment-fails-generation", func(t *testing.T) {
		if err := os.RemoveAll(filepath.Join(root, "modules", "zz-bad")); err != nil {
			t.Fatalf("clean zz-bad: %v", err)
		}
		write(filepath.Join("modules", "zz-bad", "module.yaml"), strings.Replace(fixtureManifest, "id: zz-fixture", "id: zz-bad", 1))
		out, err := gen(t, "./internal/generate/modules")
		if err == nil {
			t.Fatalf("generation must fail when the declared API fragment is missing")
		}
		if !strings.Contains(out, "zz-bad") || !strings.Contains(out, "api fragment") {
			t.Fatalf("expected a clear missing-fragment error, got: %s", out)
		}
	})
}
