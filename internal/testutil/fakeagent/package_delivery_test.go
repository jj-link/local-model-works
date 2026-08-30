package fakeagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func TestRecipeAssetsDeliveredBeforeMountedWorkload(t *testing.T) {
	server := NewServer(t, "", "127.0.0.1:0")
	defer server.Stop()
	agent := StartAgent(t, server, AgentOpts{Token: server.IssueToken(t), Hostname: "asset-node"})
	nodeID := agent.NodeID()
	server.ApproveNode(t, nodeID)
	server.WaitOnline(t, nodeID)

	source := t.TempDir()
	manifest := `apiVersion: localmodelworks/v1alpha1
kind: Recipe
metadata:
  name: package-assets
  version: 1.0.0
  description: Package delivery integration fixture.
  license: Apache-2.0
  source:
    url: https://fixtures.local/package-assets
    revision: "0000000000000000000000000000000000000000"
    path: .
compatibility:
  nodeCount: 1
artifacts: []
workloads:
  - image:
      reference: ghcr.io/localmodelworks/package-assets:1.0.0
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    command: [/lmw/assets/serve.sh]
    args: []
    resources: {cpu: 1, memoryBytes: 16777216, pids: 64}
prepare:
  image:
    reference: ghcr.io/localmodelworks/helper:1.0.0
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  command: [helper]
  args: []
  outputSchema:
    $schema: https://json-schema.org/draft/2020-12/schema
    type: object
    properties:
      version: {type: integer}
    required: [version]
verify:
  image:
    reference: ghcr.io/localmodelworks/helper:1.0.0
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  command: [helper]
  args: []
  outputSchema:
    $schema: https://json-schema.org/draft/2020-12/schema
    type: object
    properties:
      version: {type: integer}
    required: [version]
assets: [serve.sh]
`
	if err := os.WriteFile(filepath.Join(source, "recipe.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "serve.sh"), []byte("#!/bin/sh\necho served\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stored, err := server.Srv.Env().Recipes.Import(server.Ctx, recipe.RecipeSource{Type: "local", Path: source})
	if err != nil {
		t.Fatal(err)
	}
	deployment := createDep(t, server, stored.Digest)
	waitDep(t, server, deployment.ID, "healthy")
	containers := containersOf(agent.RT, depGet(t, server, deployment.ID))
	container := containers[0]
	if container == nil {
		t.Fatal("workload container missing")
	}
	var packageMountSource string
	for _, mount := range container.Spec.Mounts {
		if mount.Dest == "/lmw/assets" && mount.ReadOnly {
			packageMountSource = mount.Source
		}
	}
	if packageMountSource == "" {
		t.Fatalf("package mount missing: %+v", container.Spec.Mounts)
	}
	data, err := os.ReadFile(filepath.Join(packageMountSource, "serve.sh"))
	if err != nil || !strings.Contains(string(data), "echo served") {
		t.Fatalf("delivered asset = %q, err=%v", data, err)
	}
}
