// Package sign verifies recipe package signatures. A signature blob is
// JSON: {"alg": "Ed25519", "key_fingerprint": "sha256:<hex>", "signature":
// "<base64>"} where the signature covers the canonical recipe document
// (the package config blob) and key_fingerprint identifies the signing
// key for audit.
package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
)

// SignatureBlob is one detached signature.
type SignatureBlob struct {
	Alg            string `json:"alg"`
	KeyFingerprint string `json:"key_fingerprint"`
	Signature      string `json:"signature"`
}

// NewKeyPEM generates a fresh Ed25519 key pair and returns the PEM PKIX
// public key for recipe/catalog signature verification. The private key is
// discarded: server-side verification only needs the public half. Products
// that sign recipes keep their own key; this materializes a key for a new
// control plane whose operator has not yet configured one.
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

// VerifyEd25519 checks sigJSON over doc against the PEM public key.
func VerifyEd25519(sigJSON, doc, keyPEM []byte) error {
	var sig SignatureBlob
	if err := unmarshal(sigJSON, &sig); err != nil {
		return fmt.Errorf("signature blob: %w", err)
	}
	if sig.Alg != "Ed25519" {
		return fmt.Errorf("unsupported algorithm %q", sig.Alg)
	}
	raw, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		return fmt.Errorf("signature encoding: %w", err)
	}
	pub, err := parsePublicKey(keyPEM)
	if err != nil {
		return fmt.Errorf("trust key: %w", err)
	}
	fp := sha256.Sum256(pub)
	want := "sha256:" + hex.EncodeToString(fp[:])
	if sig.KeyFingerprint != "" && sig.KeyFingerprint != want {
		return fmt.Errorf("key fingerprint mismatch: blob %s, configured %s", sig.KeyFingerprint, want)
	}
	if !ed25519.Verify(pub, doc, raw) {
		return fmt.Errorf("signature does not validate")
	}
	return nil
}

func parsePublicKey(pemBytes []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	pk, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := pk.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want ed25519", pk)
	}
	return ed, nil
}

func unmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
