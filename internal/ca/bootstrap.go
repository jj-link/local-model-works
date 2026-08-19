package ca

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

// BootstrapConfig builds the first-connection TLS config for an agent that
// has only pinned the CA SHA-256 fingerprint at install time and does not
// yet hold the CA certificate. The server must present the CA in its TLS
// chain (leaf first); the verifier finds the presented certificate whose
// SHA-256 DER matches the pinned fingerprint, re-verifies the leaf against
// it as a standalone trust anchor, and persists its PEM to *sink so later
// connections can use ClientTLSConfig.
//
// The controller's hostname (serverName) is bound to the leaf via SAN
// verification, so an enrolled node certificate — which also chains to the
// CA and carries ServerAuth for peer transfer — cannot impersonate the
// controller. No system roots are used: the pinned fingerprint is the only
// trust input, and InsecureSkipVerify defers all validation to
// VerifyConnection (chain, key usage, and hostname).
func BootstrapConfig(serverName, fingerprintHex string, sink *[]byte) (*tls.Config, error) {
	want, err := hex.DecodeString(fingerprintHex)
	if err != nil || len(want) != sha256.Size {
		return nil, fmt.Errorf("bootstrap: invalid CA fingerprint %q", fingerprintHex)
	}
	if serverName == "" {
		return nil, fmt.Errorf("bootstrap: controller hostname required")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// System-root verification is disabled so that only the chain
		// validation below — anchored at the pinned fingerprint — decides.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("bootstrap: no peer certificate presented")
			}
			leaf := cs.PeerCertificates[0]
			var anchor *x509.Certificate
			for _, c := range cs.PeerCertificates {
				sum := sha256.Sum256(c.Raw)
				if bytes.Equal(sum[:], want) {
					anchor = c
					break
				}
			}
			if anchor == nil {
				return fmt.Errorf("bootstrap: pinned CA %s not presented in chain", fingerprintHex)
			}
			pool := x509.NewCertPool()
			pool.AddCert(anchor)
			if _, err := leaf.Verify(x509.VerifyOptions{
				Roots:     pool,
				DNSName:   serverName,
				KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}); err != nil {
				return fmt.Errorf("bootstrap: leaf does not chain to pinned CA for %s: %w", serverName, err)
			}
			*sink = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: anchor.Raw})
			return nil
		},
	}, nil
}
