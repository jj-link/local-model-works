package agent

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"

	"golang.org/x/net/http2"
)

// certSnapshot returns the live node certificate (nil before enrollment).
func (a *Agent) certSnapshot() *tls.Certificate {
	a.certMu.RLock()
	defer a.certMu.RUnlock()
	return a.nodeCert
}

// caPool builds the CA pool for TLS verification.
func (a *Agent) caPool() *x509.CertPool {
	pool := x509.NewCertPool()
	a.mu.Lock()
	caPEM := a.caPEM
	a.mu.Unlock()
	pool.AppendCertsFromPEM(caPEM)
	return pool
}

// caPubFromPEM extracts the CA's ECDSA public key from a certificate PEM.
func caPubFromPEM(caPEM []byte) (*ecdsa.PublicKey, bool) {
	block, _ := pem.Decode(caPEM)
	if block == nil {
		return nil, false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, false
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	return pub, ok
}

// tlsX509KeyPair builds a tls.Certificate from PEM-encoded cert and key.
func tlsX509KeyPair(certPEM, keyPEM []byte) (*tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	return &cert, err
}

// concatPEM joins the DER chain into PEM blocks.
func concatPEM(der [][]byte) []byte {
	var out []byte
	for _, d := range der {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: d})...)
	}
	return out
}

// keyPEM extracts the private key PEM from a loaded tls.Certificate.
func keyPEM(pair *tls.Certificate) ([]byte, error) {
	switch k := pair.PrivateKey.(type) {
	case *ecdsa.PrivateKey:
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
	default:
		return nil, errUnsupportedKeyType
	}
}

var errUnsupportedKeyType = errString("unsupported private key type for TLS client")

type errString string

func (e errString) Error() string { return string(e) }

// serialOfPEM returns the certificate's serial number as a hex string.
func serialOfPEM(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errNoPEM
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", err
	}
	return cert.SerialNumber.Text(16), nil
}

var errNoPEM = errString("no PEM block found")

// httpClient wraps an http2.Transport in an *http.Client with a bounded
// default timeout (streams cancel via context).
func httpClient(tr *http2.Transport) *http.Client {
	return &http.Client{Transport: tr}
}
