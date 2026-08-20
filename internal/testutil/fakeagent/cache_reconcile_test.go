package fakeagent

import (
	"encoding/json"
	"github.com/jj-link/local-model-works/internal/recipe"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentCachePlacementExistsBeforeRecipeInstall(t *testing.T) {
	server := NewServer(t, "", "127.0.0.1:0")
	defer server.Stop()
	cache := t.TempDir()
	revision := strings.Repeat("a", 40)
	modelRoot := filepath.Join(cache, "hub", "models--Acme--Model")
	snapshot := filepath.Join(modelRoot, "snapshots", revision)
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := StartAgent(t, server, AgentOpts{
		Token: server.IssueToken(t), CacheRoots: []string{cache}, Hostname: "cache-node",
	})
	nodeID := agent.NodeID()
	server.ApproveNode(t, nodeID)
	server.WaitOnline(t, nodeID)
	identity := "hf://Acme/Model@" + revision
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
	snapshot := filepath.Join(modelRoot, "snapshots", revision)
	if err := os.MkdirAll(snapshot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshot, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture := FixtureRecipe{
		Name: "late-cache", Version: "1.0.0", NodeCount: 1,
		Artifacts: []recipe.Artifact{{
			Name: "model", Kind: "model", Mount: "/models/model",
			Source: recipe.ArtSource{Type: "huggingface", Identity: "hf://Acme/LateModel", Revision: revision},
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
	identity := "hf://Acme/LateModel@" + revision
	Deadline(t, 20*time.Second, func() bool {
		artifact, err := server.Q.GetArtifactByIdentity(server.Ctx, identity)
		if err != nil {
			return false
		}
		placements, err := server.Q.ListPlacements(server.Ctx, artifact.ID)
		return err == nil && len(placements) == 1 && placements[0].State == "valid"
	}, "post-install cache rescan")
}
