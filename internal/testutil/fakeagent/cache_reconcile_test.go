package fakeagent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/recipe"
)

func seedCompletedSnapshot(t *testing.T, modelRoot, revision, identity string) {
	t.Helper()
	snapshot := filepath.Join(modelRoot, "snapshots", revision)
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("{}")
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	manifest, err := json.Marshal(map[string]any{
		"identity": identity,
		"files": []map[string]any{{
			"path": "config.json", "size": len(body), "digest": fmt.Sprintf("sha256:%x", sum),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(modelRoot, ".lmw", "snapshots", revision+".json")
	if err := os.MkdirAll(filepath.Dir(manifestPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentCachePlacementExistsBeforeRecipeInstall(t *testing.T) {
	server := NewServer(t, "", "127.0.0.1:0")
	defer server.Stop()
	cache := t.TempDir()
	revision := strings.Repeat("a", 40)
	modelRoot := filepath.Join(cache, "hub", "models--Acme--Model")
	identity := "hf://Acme/Model@" + revision
	seedCompletedSnapshot(t, modelRoot, revision, identity)
	agent := StartAgent(t, server, AgentOpts{
		Token: server.IssueToken(t), CacheRoots: []string{cache}, Hostname: "cache-node",
	})
	nodeID := agent.NodeID()
	server.ApproveNode(t, nodeID)
	server.WaitOnline(t, nodeID)
	Deadline(t, 20*time.Second, func() bool {
		artifact, err := server.Q.GetArtifactByIdentity(server.Ctx, identity)
		if err != nil {
			return false
		}
		placements, err := server.Q.ListPlacements(server.Ctx, artifact.ID)
		return err == nil && len(placements) == 1 && placements[0].Path == modelRoot && placements[0].State == "valid"
	}, "validated pre-recipe cache placement")
}

func TestRecipeInstallRequestsConnectedAgentCacheRescan(t *testing.T) {
	server := NewServer(t, "", "127.0.0.1:0")
	defer server.Stop()
	cache := t.TempDir()
	agent := StartAgent(t, server, AgentOpts{
		Token: server.IssueToken(t), CacheRoots: []string{cache}, Hostname: "rescan-node",
	})
	nodeID := agent.NodeID()
	server.ApproveNode(t, nodeID)
	server.WaitOnline(t, nodeID)

	revision := strings.Repeat("d", 40)
	modelRoot := filepath.Join(cache, "hub", "models--Acme--LateModel")
	identity := "hf://Acme/LateModel@" + revision
	seedCompletedSnapshot(t, modelRoot, revision, identity)
	fixture := FixtureRecipe{
		Name: "late-cache", Version: "1.0.0", NodeCount: 1,
		Artifacts: []recipe.Artifact{{
			Name: "model", Kind: "model", Mount: "/models/model",
			Source: &recipe.ArtSource{Type: "huggingface", Identity: "hf://Acme/LateModel", Revision: revision},
		}},
	}
	document, err := json.Marshal(fixture.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Srv.Env().Recipes.Store(
		server.Ctx, document, recipe.RecipeSource{Type: "local", Path: cache}, recipe.TrustLocal,
	); err != nil {
		t.Fatalf("install recipe: %v", err)
	}
	Deadline(t, 20*time.Second, func() bool {
		artifact, err := server.Q.GetArtifactByIdentity(server.Ctx, identity)
		if err != nil {
			return false
		}
		placements, err := server.Q.ListPlacements(server.Ctx, artifact.ID)
		return err == nil && len(placements) == 1 && placements[0].State == "valid"
	}, "post-install cache rescan")
}
