// Package ca owns the private server CA: key/cert generation, node client
// certificates (90-day lifetime, 30-day rotation threshold), enrollment
// tokens (32 random bytes, one-use, 10-minute expiry), and SHA-256
// certificate fingerprints.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path"
	"time"
)

const (
	CertValidity    = 90 * 24 * time.Hour
	RotateThreshold = 30 * 24 * time.Hour
	TokenBytes      = 32
	TokenTTL        = 10 * time.Minute
	CAValidity      = 10 * 365 * 24 * time.Hour
)

// CA is a self-signed ECDSA P-256 certificate authority.
type CA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
}

// New creates a fresh CA.
func New() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("ca key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Local Model Works CA", Organization: []string{"Local Model Works"}},
		NotBefore:             time.Now().UTC().Add(-time.Hour),
		NotAfter:              time.Now().UTC().Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("ca cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{key: key, cert: cert}, nil
}

// Fingerprint is the SHA-256 hex of the CA's DER — the value the operator
// pins at agent install time.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// PEMCert returns the PEM-encoded CA certificate.
func (c *CA) PEMCert() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

// PEMKey returns the PEM-encoded PKCS#8 CA key.
func (c *CA) PEMKey() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// NodeCert mints a node certificate usable both as the controller's client
// certificate and as the peer-transfer TLS server certificate on :9444.
// The node ID is carried as the certificate subject CN and as a DNS SAN so
// peers can authenticate the exact node over mTLS.
func (c *CA) NodeCert(nodeID, hostname string, validity time.Duration) (certPEM, keyPEM []byte, expires time.Time, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("node key: %w", err)
	}
	certPEM, expires, err = c.NodeCertFor(nodeID, hostname, &key.PublicKey, validity)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), expires, nil
}

// SerialOf returns the decimal serial of a PEM certificate.
func SerialOf(certPEM []byte) (string, error) {
	cert, err := ParseCertPEM(certPEM)
	if err != nil {
		return "", err
	}
	return cert.SerialNumber.String(), nil
}

// ParseCertPEM decodes a single PEM certificate.
func ParseCertPEM(data []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ParseKeyPEM decodes a PKCS#8 PEM private key.
func ParseKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("pkcs8: %w", err)
	}
	ec, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("not an ECDSA key")
	}
	return ec, nil
}

// VerifyChain verifies a presented certificate chain against the CA and
// returns an error unless the chain terminates at exactly this CA,
// identified by fingerprint. Callers pin the fingerprint at install time.
func (c *CA) VerifyChain(leaf *x509.Certificate, chains [][]*x509.Certificate) error {
	if len(chains) == 0 {
		return fmt.Errorf("no certificate chain")
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.cert)
	roots := make([]*x509.Certificate, 0, len(chains))
	seen := make(map[string]bool)
	for _, ch := range chains {
		if len(ch) == 0 {
			continue
		}
		root := ch[len(ch)-1]
		rootSum := sha256.Sum256(root.Raw)
		fp := hex.EncodeToString(rootSum[:])
		if !seen[fp] {
			seen[fp] = true
			roots = append(roots, root)
		}
	}
	for _, root := range roots {
		rootSum := sha256.Sum256(root.Raw)
		if hex.EncodeToString(rootSum[:]) == c.Fingerprint() {
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
				return fmt.Errorf("chain verification: %w", err)
			}
			return nil
		}
	}
	return fmt.Errorf("chain does not terminate at pinned CA")
}

// PinnedVerifier builds a tls.Config-usable check from a hex CA fingerprint
// and a CA certificate PEM loaded at startup.
func PinnedVerifier(caPEM []byte, fingerprintHex string) (func(leaf *x509.Certificate, chains [][]*x509.Certificate) error, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, fmt.Errorf("no CA PEM")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	caSum := sha256.Sum256(caCert.Raw)
	if hex.EncodeToString(caSum[:]) != fingerprintHex {
		return nil, fmt.Errorf("CA fingerprint mismatch")
	}
	c := &CA{cert: caCert}
	return c.VerifyChain, nil
}

// SaveKeyCert writes key and cert PEM files with 0600/0644 modes.
func SaveKeyCert(dir, keyPath, certPath string, keyPEM, certPEM []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, certPEM, 0o644)
}

// LoadKeyCert reads a CA from disk, creating a new one when absent.
func LoadKeyCert(keyPath, certPath string) (*CA, error) {
	keyPEM, err1 := os.ReadFile(keyPath)
	certPEM, err2 := os.ReadFile(certPath)
	if err1 == nil && err2 == nil {
		key, err := ParseKeyPEM(keyPEM)
		if err != nil {
			return nil, err
		}
		cert, err := ParseCertPEM(certPEM)
		if err != nil {
			return nil, err
		}
		return &CA{key: key, cert: cert}, nil
	}
	c, err := New()
	if err != nil {
		return nil, err
	}
	k, err := c.PEMKey()
	if err != nil {
		return nil, err
	}
	if err := SaveKeyCert(path.Dir(keyPath), keyPath, certPath, k, c.PEMCert()); err != nil {
		return nil, err
	}
	return c, nil
}

// NewToken returns a fresh enrollment token (hex).
func NewToken() (string, error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("enrollment token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
