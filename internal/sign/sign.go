// Package sign verifies key-signed Sigstore bundles for recipe packages and catalogs.
package sign

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"time"

	sigbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/signature"
)

// NewKeyPEM generates a fresh Ed25519 verification key. Production catalogs
// and recipe publishers retain the corresponding private key outside LMW.
func NewKeyPEM() ([]byte, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// VerifyBundle verifies a Sigstore JSON bundle over artifact with the
// configured PEM public key. It is deliberately offline: key-signed bundles
// need neither Fulcio nor Rekor and must not trigger network access.
func VerifyBundle(bundleJSON, artifact, keyPEM []byte) error {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return fmt.Errorf("trust key: no PEM block found")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("trust key: %w", err)
	}
	keyVerifier, err := signature.LoadVerifier(publicKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("trust key: %w", err)
	}
	expiringKey := root.NewExpiringKey(keyVerifier, time.Time{}, time.Time{})
	trusted := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return expiringKey, nil
	})
	verifier, err := verify.NewVerifier(trusted, verify.WithNoObserverTimestamps())
	if err != nil {
		return fmt.Errorf("sigstore verifier: %w", err)
	}
	var bundle sigbundle.Bundle
	if err := bundle.UnmarshalJSON(bundleJSON); err != nil {
		return fmt.Errorf("sigstore bundle: %w", err)
	}
	_, err = verifier.Verify(&bundle, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(artifact)),
		verify.WithKey(),
	))
	if err != nil {
		return fmt.Errorf("sigstore verification: %w", err)
	}
	return nil
}
