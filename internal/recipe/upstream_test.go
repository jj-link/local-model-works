package recipe

import (
	"encoding/json"
	"strings"
	"testing"
)

func upstreamValidationManifest() *Manifest {
	return &Manifest{
		APIVersion: APIVersion,
		Kind:       "Recipe",
		Metadata: Metadata{
			Name:        "upstream-example",
			Version:     "1.0.0",
			Description: "Authored upstream lifecycle",
			License:     "MIT",
			Source: &Source{
				URL:      "https://github.com/example/inference",
				Revision: strings.Repeat("a", 40),
				Path:     "examples/server",
			},
		},
		Compatibility: Compatibility{NodeCount: 1},
		Artifacts:     []Artifact{},
		Parameters:    []Parameter{{Name: "port", Type: "int", Default: 8000}},
		Workloads: []Workload{{
			Upstream: &UpstreamExecution{
				Install:     [][]string{{"bash", "setup.sh"}},
				Start:       []string{"bash", "start.sh", "--port", "${setting.port}"},
				Stop:        []string{"bash", "stop.sh"},
				Containers:  []string{"inference-${node.rank}"},
				EnvFile:     ".env",
				EnvTemplate: ".env.example",
				EnvFormat:   "shell",
				LogFile:     "logs/server.log",
			},
			Env:         map[string]string{"PORT": "${setting.port}"},
			Permissions: []string{"host.upstream-exec"},
			Ports:       []Port{{Container: 8000}},
			Readiness:   &Probe{HTTPGet: &HTTPGet{Path: "/health", Port: 8000}},
		}},
	}
}

func TestUpstreamManifestRoundTrip(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	manifest := upstreamValidationManifest()
	doc, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, findings, err := validator.ValidateStrict(doc)
	if err != nil || len(findings) != 0 {
		t.Fatalf("upstream serialization must validate: %v, %+v", err, findings)
	}
	start := parsed.Workloads[0].Upstream.Start
	rendered, err := parsed.Render(start[3], RenderContext{Settings: map[string]any{"port": 9000}})
	if err != nil || rendered != "9000" {
		t.Fatalf("reviewed setting did not resolve: %q, %v", rendered, err)
	}
	name, err := parsed.Render(parsed.Workloads[0].Upstream.Containers[0], RenderContext{NodeRank: 2})
	if err != nil || name != "inference-2" {
		t.Fatalf("authored container identity did not resolve: %q, %v", name, err)
	}
	// Host authority must remain visible even before a missing acknowledgment
	// has been rejected by validation.
	parsed.Workloads[0].Permissions = nil
	permissions := parsed.HighRiskPermissions()
	if len(permissions) != 1 || permissions[0] != "host.upstream-exec" {
		t.Fatalf("upstream host authority not surfaced: %v", permissions)
	}

	// Container recipes still serialize an explicitly empty args array and
	// retain their original required image/command/resource contract.
	manifest.Workloads[0] = Workload{
		Image:     Image{Reference: "docker.io/example/server:1", Digest: "sha256:" + strings.Repeat("b", 64)},
		Command:   []string{"server"},
		Args:      []string{},
		Resources: Resources{Pids: 64},
	}
	doc, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, findings, err := validator.ValidateStrict(doc); err != nil || len(findings) != 0 {
		t.Fatalf("container serialization no longer validates: %v, %+v", err, findings)
	}
}

func TestUpstreamRejectsUnsafeOrMixedLifecycle(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any, map[string]any, map[string]any)
	}{
		{"missing-env-format", func(_, _ map[string]any, u map[string]any) { delete(u, "envFormat") }},
		{"invalid-env-format", func(_, _ map[string]any, u map[string]any) { u["envFormat"] = "dotenv" }},
		{"auxiliary-container-option", func(_, _ map[string]any, u map[string]any) { u["auxiliaryContainers"] = []string{"--all"} }},
		{"override-container-option", func(_, _ map[string]any, u map[string]any) {
			u["containersByRank"] = map[string]any{"0": []string{"--all"}}
		}},
		{"noncanonical-rank-key", func(_, _ map[string]any, u map[string]any) {
			u["containersByRank"] = map[string]any{"01": []string{"worker"}}
		}},
		{"coordinator-start-order", func(_ map[string]any, w, u map[string]any) {
			u["coordinatorRank"] = 0
			w["startOrder"] = "workers-first"
		}},
		{"rank-install-empty-command", func(_, _ map[string]any, u map[string]any) { u["installByRank"] = map[string]any{"0": [][]string{{}}} }},
		{"rank-install-traversal", func(_, _ map[string]any, u map[string]any) {
			u["installByRank"] = map[string]any{"0": [][]string{{"bash", "../setup.sh"}}}
		}},
		{"rank-install-noncanonical-key", func(_, _ map[string]any, u map[string]any) { u["installByRank"] = map[string]any{"00": [][]string{}} }},
		{"container-image", func(_ map[string]any, w, _ map[string]any) {
			w["image"] = map[string]any{"reference": "server:1", "digest": "sha256:" + strings.Repeat("a", 64)}
		}},
		{"container-command", func(_ map[string]any, w, _ map[string]any) { w["command"] = []string{"server"} }},
		{"empty-container-args", func(_ map[string]any, w, _ map[string]any) { w["args"] = []string{} }},
		{"container-resources", func(_ map[string]any, w, _ map[string]any) { w["resources"] = map[string]any{"pids": 64} }},
		{"container-devices", func(_ map[string]any, w, _ map[string]any) { w["devices"] = map[string]any{} }},
		{"container-network", func(_ map[string]any, w, _ map[string]any) { w["networkMode"] = "host" }},
		{"host-preparation", func(_ map[string]any, w, _ map[string]any) {
			w["hostPreparation"] = map[string]any{"requireSwap": true}
		}},
		{"missing-permission", func(_ map[string]any, w, _ map[string]any) { delete(w, "permissions") }},
		{"wrong-permission", func(_ map[string]any, w, _ map[string]any) { w["permissions"] = []string{"network.host"} }},
		{"missing-stop", func(_, _ map[string]any, u map[string]any) { delete(u, "stop") }},
		{"empty-start", func(_, _ map[string]any, u map[string]any) { u["start"] = []string{} }},
		{"blank-executable", func(_, _ map[string]any, u map[string]any) { u["start"] = []string{" "} }},
		{"missing-containers", func(_, _ map[string]any, u map[string]any) { delete(u, "containers") }},
		{"container-option", func(_, _ map[string]any, u map[string]any) { u["containers"] = []string{"--all"} }},
		{"duplicate-container", func(_, _ map[string]any, u map[string]any) { u["containers"] = []string{"server", "server"} }},
		{"command-control", func(_, _ map[string]any, u map[string]any) { u["stop"] = []string{"stop.sh\x00"} }},
		{"command-traversal", func(_, _ map[string]any, u map[string]any) { u["start"] = []string{"bash", "../start.sh"} }},
		{"install-traversal", func(_, _ map[string]any, u map[string]any) {
			u["install"] = [][]string{{"bash", "--config=../setup.sh"}}
		}},
		{"env-injection", func(_ map[string]any, w, _ map[string]any) { w["env"] = map[string]string{"PORT": "8000\nOTHER=value"} }},
		{"absolute-config", func(_, _ map[string]any, u map[string]any) { u["envFile"] = "/etc/environment" }},
		{"config-traversal", func(_, _ map[string]any, u map[string]any) { u["envTemplate"] = "config/../.env" }},
		{"log-backslash", func(_, _ map[string]any, u map[string]any) { u["logFile"] = "..\\server.log" }},
		{"templated-path", func(_, _ map[string]any, u map[string]any) { u["logFile"] = "${setting.port}.log" }},
		{"template-without-config", func(_, _ map[string]any, u map[string]any) { delete(u, "envFile") }},
		{"artifact-substitution", func(_, _ map[string]any, u map[string]any) {
			u["start"] = []string{"${artifact.package.path}/start.sh"}
		}},
		{"exec-probe", func(_ map[string]any, w, _ map[string]any) { w["readiness"] = map[string]any{"exec": []string{"true"}} }},
		{"missing-source", func(d, _, _ map[string]any) { delete(d["metadata"].(map[string]any), "source") }},
		{"mutable-source", func(d, _, _ map[string]any) {
			d["metadata"].(map[string]any)["source"].(map[string]any)["revision"] = "main"
		}},
		{"source-traversal", func(d, _, _ map[string]any) {
			d["metadata"].(map[string]any)["source"].(map[string]any)["path"] = "examples/../../other"
		}},
		{"artifact-acquisition", func(d, _, _ map[string]any) {
			d["artifacts"] = []any{map[string]any{"name": "weights", "kind": "file", "source": map[string]any{"type": "file", "identity": "example.com/model", "digest": "sha256:" + strings.Repeat("a", 64)}, "mount": "/models/weights"}}
		}},
		{"prepare-extension", func(d, _, _ map[string]any) { d["prepare"] = upstreamTestExtension() }},
		{"verify-extension", func(d, _, _ map[string]any) { d["verify"] = upstreamTestExtension() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := json.Marshal(upstreamValidationManifest())
			if err != nil {
				t.Fatal(err)
			}
			var raw map[string]any
			if err := json.Unmarshal(doc, &raw); err != nil {
				t.Fatal(err)
			}
			workload := raw["workloads"].([]any)[0].(map[string]any)
			tt.mutate(raw, workload, workload["upstream"].(map[string]any))
			if err := validator.schema.Validate(raw); err == nil {
				t.Fatal("schema accepted an unsafe or mixed upstream lifecycle")
			}
		})
	}
}

func upstreamTestExtension() map[string]any {
	return map[string]any{
		"image":        map[string]any{"reference": "server:1", "digest": "sha256:" + strings.Repeat("a", 64)},
		"command":      []string{"true"},
		"args":         []string{},
		"outputSchema": map[string]any{},
	}
}

func TestUpstreamTemplatesRequireDeclaredContext(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"install", "installByRank", "start", "stop", "containers", "containersByRank", "auxiliaryContainers", "env"} {
		t.Run(field, func(t *testing.T) {
			manifest := upstreamValidationManifest()
			w := &manifest.Workloads[0]
			value := "${setting.undeclared}"
			switch field {
			case "install":
				w.Upstream.Install = [][]string{{"bash", value}}
			case "installByRank":
				w.Upstream.InstallByRank = map[int][][]string{0: {{"bash", value}}}
			case "start":
				w.Upstream.Start = []string{"bash", value}
			case "stop":
				w.Upstream.Stop = []string{"bash", value}
			case "containers":
				w.Upstream.Containers = []string{value}
			case "containersByRank":
				w.Upstream.ContainersByRank = map[int][]string{0: {value}}
			case "auxiliaryContainers":
				w.Upstream.AuxiliaryContainers = []string{value}
			case "env":
				w.Env["PORT"] = value
			}
			doc, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := validator.Validate(doc)
			if err != nil {
				t.Fatal(err)
			}
			for _, finding := range findings {
				if finding.Code == "recipe.template" {
					return
				}
			}
			t.Fatalf("undeclared setting not rejected: %+v", findings)
		})
	}
}

func TestUpstreamCoordinatorRanks(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	clusterManifest := func() *Manifest {
		m := upstreamValidationManifest()
		m.Compatibility.NodeCount = 2
		m.Workloads[0].Ranks = []int{0, 1}
		coordinator := 0
		u := m.Workloads[0].Upstream
		u.CoordinatorRank = &coordinator
		u.ContainersByRank = map[int][]string{1: {"worker"}}
		u.AuxiliaryContainers = []string{"setup-helper"}
		u.EnvFormat = "literal"
		m.Workloads[0].Env["WORKER"] = "${cluster.node.1.address}"
		return m
	}
	tests := []struct {
		name   string
		mutate func(*Manifest)
		valid  bool
	}{
		{"declared-coordinator-and-worker", func(*Manifest) {}, true},
		{"observer-authored-install", func(m *Manifest) {
			m.Workloads[0].Upstream.InstallByRank = map[int][][]string{1: {{"docker", "pull", "upstream/server:1"}}}
		}, true},
		{"observer-explicit-empty-install", func(m *Manifest) { m.Workloads[0].Upstream.InstallByRank = map[int][][]string{1: {}} }, true},
		{"install-rank-outside-cluster", func(m *Manifest) { m.Workloads[0].Upstream.InstallByRank = map[int][][]string{2: {}} }, false},
		{"install-rank-not-selected", func(m *Manifest) {
			m.Workloads[0].Ranks = []int{0}
			m.Workloads[0].Upstream.ContainersByRank = nil
			delete(m.Workloads[0].Env, "WORKER")
			m.Workloads[0].Upstream.InstallByRank = map[int][][]string{1: {}}
		}, false},
		{"coordinator-outside-cluster", func(m *Manifest) { *m.Workloads[0].Upstream.CoordinatorRank = 2 }, false},
		{"coordinator-not-selected", func(m *Manifest) { m.Workloads[0].Ranks = []int{1} }, false},
		{"override-outside-cluster", func(m *Manifest) { m.Workloads[0].Upstream.ContainersByRank[2] = []string{"other"} }, false},
		{"override-not-selected", func(m *Manifest) { m.Workloads[0].Ranks = []int{0}; delete(m.Workloads[0].Env, "WORKER") }, false},
		{"rank-outside-node-count", func(m *Manifest) { m.Workloads[0].Ranks = []int{0, 1, 2} }, false},
		{"peer-outside-cluster", func(m *Manifest) { m.Workloads[0].Env["WORKER"] = "${cluster.node.2.address}" }, false},
		{"peer-not-selected", func(m *Manifest) { m.Workloads[0].Ranks = []int{0}; m.Workloads[0].Upstream.ContainersByRank = nil }, false},
		{"peer-fabric-without-requirement", func(m *Manifest) { m.Workloads[0].Env["WORKER"] = "${cluster.node.1.fabric.node_addr}" }, false},
		{"peer-fabric-with-requirement", func(m *Manifest) {
			m.Compatibility.Fabric = &FabricCompat{}
			m.Workloads[0].Env["WORKER"] = "${cluster.node.1.fabric.node_addr}"
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := clusterManifest()
			tt.mutate(m)
			doc, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := validator.Validate(doc)
			if err != nil {
				t.Fatal(err)
			}
			if (len(findings) == 0) != tt.valid {
				t.Fatalf("valid=%v: %+v", tt.valid, findings)
			}
		})
	}
}

func TestClusterTemplateRequiresReviewedPeerValues(t *testing.T) {
	m := upstreamValidationManifest()
	ctx := RenderContext{Nodes: map[int]RenderNode{
		1: {
			NodeID:           "worker",
			NodeAddress:      "192.0.2.11",
			FabricNodeAddr:   "192.0.2.12",
			FabricInterface:  "eth1",
			FabricRDMADevice: "mlx5_0",
			FabricGIDIndex:   "0",
		},
	}}
	input := "${cluster.node.1.id} ${cluster.node.1.address} ${cluster.node.1.fabric.node_addr} ${cluster.node.1.fabric.interface} ${cluster.node.1.fabric.rdma_device} ${cluster.node.1.fabric.gid_index}"
	got, err := m.Render(input, ctx)
	if err != nil || got != "worker 192.0.2.11 192.0.2.12 eth1 mlx5_0 0" {
		t.Fatalf("peer command input did not render from reviewed placement: %q, %v", got, err)
	}
	for _, unresolved := range []string{"${cluster.node.0.address}", "${cluster.node.01.address}", "${cluster.node.1.fabric.address}"} {
		if _, err := m.Render(unresolved, ctx); err == nil {
			t.Fatalf("unresolved peer input accepted: %s", unresolved)
		}
	}
	ctx.Nodes[1] = RenderNode{}
	for _, unresolved := range TemplateVars(input) {
		if _, err := m.Render(unresolved, ctx); err == nil {
			t.Fatalf("missing peer value silently rendered empty: %s", unresolved)
		}
	}
}
