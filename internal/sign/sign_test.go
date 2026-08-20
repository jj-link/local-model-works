package sign

import (
	"testing"

	sigsign "github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"
)

func signedBundle(t *testing.T, artifact []byte) ([]byte, []byte) {
	t.Helper()
	keypair, err := sigsign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := sigsign.Bundle(&sigsign.PlainData{Data: artifact}, keypair, sigsign.BundleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := keypair.GetPublicKeyPem()
	if err != nil {
		t.Fatal(err)
	}
	return bundleJSON, []byte(publicKey)
}

func TestVerifyBundleOfflineKeySignature(t *testing.T) {
	artifact := []byte("immutable OCI manifest")
	bundleJSON, publicKey := signedBundle(t, artifact)
	if err := VerifyBundle(bundleJSON, artifact, publicKey); err != nil {
		t.Fatalf("valid bundle: %v", err)
	}
	if err := VerifyBundle(bundleJSON, []byte("tampered"), publicKey); err == nil {
		t.Fatal("tampered artifact accepted")
	}
	_, wrongKey := signedBundle(t, artifact)
	if err := VerifyBundle(bundleJSON, artifact, wrongKey); err == nil {
		t.Fatal("wrong trust key accepted")
	}
}
