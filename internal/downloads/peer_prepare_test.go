package downloads

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/db"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestSharedPeerPreparationBindsCanonicalDestinationTreeAndOperation(t *testing.T) {
	service, nodes := downloadHarness(t)
	ctx := context.Background()
	authority, err := ca.New()
	if err != nil {
		t.Fatal(err)
	}
	service.ca = authority
	sourceInventory := `{"protocol_features":["downloads-v1"],"cache_roots":[{"path":"/source-cache","writable":true}],"peer_listen":"[::]:9444"}`
	destinationInventory := `{"protocol_features":["downloads-v1"],"cache_roots":[{"path":"/cache","writable":true}]}`
	if err = service.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: "node-a", Inventory: ns(sourceInventory)}); err != nil {
		t.Fatal(err)
	}
	if err = service.q.SetNodeInventory(ctx, db.SetNodeInventoryParams{ID: "node-b", Inventory: ns(destinationInventory)}); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	sourcePath := "/source-cache/files/" + strings.Repeat("a", 64)
	destination := "/cache/files/" + strings.Repeat("a", 64)
	nodes.inspectPath = func(path string) ResourceState {
		if path == sourcePath {
			return ResourceAvailable
		}
		return ResourceMissing
	}
	request := PeerRequest{SourceNode: "node-a", DestinationNode: "node-b", Identity: "file://" + digest, SourcePath: sourcePath, PeerAddress: "10.0.0.1:9444", RunID: "transfer:attempt", TransferID: "attempt"}
	prepared, err := service.PreparePeer(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Command.Op != agentv1.TransferOp_TRANSFER_OP_START || prepared.Command.DestPath != destination {
		t.Fatalf("new peer preparation did not use explicit canonical START: %+v", prepared.Command)
	}
	raw, err := base64.StdEncoding.DecodeString(prepared.Command.Credential)
	if err != nil {
		t.Fatal(err)
	}
	var credential peerCredential
	if err = json.Unmarshal(raw, &credential); err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(credential.Signature)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := ca.ParseCertPEM(authority.PEMCert())
	if err != nil {
		t.Fatal(err)
	}
	publicKey := certificate.PublicKey.(*ecdsa.PublicKey)
	credential.Signature = ""
	if err = ca.VerifyECDSA(publicKey, []byte(encoded(credential)), signature); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*peerCredential){func(c *peerCredential) { c.DestPath = "/cache/arbitrary" }, func(c *peerCredential) { c.TreeDigest = "different" }, func(c *peerCredential) { c.Operation = "cancel" }} {
		changed := credential
		mutate(&changed)
		if ca.VerifyECDSA(publicKey, []byte(encoded(changed)), signature) == nil {
			t.Fatal("peer credential did not bind destination, tree and operation")
		}
	}
	for _, command := range nodes.commands {
		if command.Op != agentv1.DownloadOp_DOWNLOAD_OP_INSPECT {
			t.Fatal("preparing a peer credential acquired files before ownership was persisted")
		}
	}
	var rows int
	if err = service.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM transfers").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("preparation created transfer side effects: %d %v", rows, err)
	}
	request.Destination = "/cache/arbitrary"
	if _, err = service.PreparePeer(ctx, request); err == nil {
		t.Fatal("noncanonical standalone destination was accepted")
	}
}
