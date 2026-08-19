package ca

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// SignECDSA signs msg with the CA (or node) key: SHA-256 digest, ASN.1
// encoding. Returns (digest, sig) so callers can carry the digest.
func SignECDSA(priv *ecdsa.PrivateKey, msg []byte) (digest, sig []byte, err error) {
	d := sha256.Sum256(msg)
	sig, err = ecdsa.SignASN1(rand.Reader, priv, d[:])
	return d[:], sig, err
}

// VerifyECDSA checks an ASN.1 ECDSA signature over msg with the public key.
func VerifyECDSA(pub *ecdsa.PublicKey, msg, sig []byte) error {
	d := sha256.Sum256(msg)
	if !ecdsa.VerifyASN1(pub, d[:], sig) {
		return errBadSignature
	}
	return nil
}

// SignCA signs msg with the CA key and returns the base64 std-encoding of
// the ASN.1 signature — the wire form carried in peer credentials.
func (c *CA) SignCA(msg []byte) (string, error) {
	_, sig, err := SignECDSA(c.key, msg)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

type errString string

func (e errString) Error() string { return string(e) }

var errBadSignature = errString("ecdsa signature mismatch")
