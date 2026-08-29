package agent

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/jj-link/local-model-works/internal/ca"
	"github.com/jj-link/local-model-works/internal/config"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func transferTestService(t *testing.T) (*transferService, *transferCred, context.Context) {
	t.Helper()
	certificateAuthority, err := ca.New()
	if err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "model.bin"), []byte("model bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := &Agent{cfg: config.Agent{StateRoot: t.TempDir()}, nodeID: "source-node", caPEM: certificateAuthority.PEMCert()}
	credential := &transferCred{
		TransferID: "transfer-1", RunID: "run-1", SourceNode: "source-node", DestNode: "dest-node",
		ArtifactID: "file://sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SrcPath:    source, DestPath: "model", ExpUnix: time.Now().Add(time.Minute).Unix(),
	}
	signature, err := certificateAuthority.SignCA(credential.canonicalJSON())
	if err != nil {
		t.Fatal(err)
	}
	credential.Signature = signature
	certPEM, _, _, err := certificateAuthority.NodeCert("dest-node", "dest", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certPEM)
	peer, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), peerCertContextKey{}, peer)
	return &transferService{a: agent, active: map[string]*transferSession{}, used: map[string]bool{}}, credential, ctx
}

func encodedCredential(t *testing.T, credential *transferCred) string {
	t.Helper()
	raw, err := json.Marshal(credential)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestTransferCredentialIsOneUse(t *testing.T) {
	service, credential, ctx := transferTestService(t)
	encoded := encodedCredential(t, credential)
	manifest, err := service.Manifest(ctx, connect.NewRequest(&agentv1.ManifestRequest{
		TransferId: credential.TransferID, Credential: encoded, Path: credential.SrcPath,
	}))
	if err != nil {
		t.Fatal(err)
	}
	file := manifest.Msg.GetFiles()[0]
	if _, err := service.ReadChunk(ctx, connect.NewRequest(&agentv1.ReadChunkRequest{
		TransferId: credential.TransferID, Credential: encoded, Path: file.GetPath(), Offset: 0, Length: uint32(file.GetSize()),
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Manifest(ctx, connect.NewRequest(&agentv1.ManifestRequest{
		TransferId: credential.TransferID, Credential: encoded, Path: credential.SrcPath,
	})); err == nil {
		t.Fatal("used transfer credential replayed")
	}
}

func TestTransferCredentialRejectsExpiryAndWrongPeer(t *testing.T) {
	service, credential, ctx := transferTestService(t)
	credential.ExpUnix = time.Now().Add(-time.Minute).Unix()
	if _, err := service.Manifest(ctx, connect.NewRequest(&agentv1.ManifestRequest{
		TransferId: credential.TransferID, Credential: encodedCredential(t, credential), Path: credential.SrcPath,
	})); err == nil {
		t.Fatal("expired credential accepted")
	}
	service, credential, _ = transferTestService(t)
	if _, err := service.Manifest(context.Background(), connect.NewRequest(&agentv1.ManifestRequest{
		TransferId: credential.TransferID, Credential: encodedCredential(t, credential), Path: credential.SrcPath,
	})); err == nil {
		t.Fatal("credential accepted without destination peer certificate")
	}
}

func TestCollectManifestRegularFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "weights.bin")
	if err := os.WriteFile(root, []byte("weights"), 0o640); err != nil {
		t.Fatal(err)
	}
	entries, total, digest, err := collectManifest(context.Background(), root, filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != transferRootFile || entries[0].Size != 7 || total != 7 || digest == "" {
		t.Fatalf("manifest = %+v total=%d digest=%q", entries, total, digest)
	}
}

func TestMakeTransferredArtifactMountable(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifact")
	nested := filepath.Join(root, "snapshots", "revision")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(nested, "model.safetensors")
	if err := os.WriteFile(file, []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "snapshots"), nested} {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := makeTransferredArtifactMountable(root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, filepath.Join(root, "snapshots"), nested} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode = %v; want 0755", path, info.Mode().Perm())
		}
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %v; want 0644", info.Mode().Perm())
	}
}
