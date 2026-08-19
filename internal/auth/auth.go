// Package auth implements single-operator authentication: Argon2id
// password storage, 12-hour sessions with per-session CSRF tokens, and
// AES-256-GCM secret encryption with a generated 0600 key.
package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters mandated for admin credentials.
const (
	argonMemory      = 64 * 1024 // 64 MiB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltSize    = 16
	argonKeyLength   = 32
)

// HashPassword derives an Argon2id hash with a fresh 16-byte salt.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltSize)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2 salt: %w", err)
	}
	dk := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	raw := append(salt, dk...)
	return base64.RawStdEncoding.EncodeToString(raw), nil
}

// VerifyPassword checks a password against a HashPassword output.
func VerifyPassword(password, encoded string) bool {
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil || len(raw) < argonSaltSize+argonKeyLength {
		return false
	}
	salt, dk := raw[:argonSaltSize], raw[argonSaltSize:]
	want := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLength)
	return subtle.ConstantTimeCompare(want, dk) == 1
}

// SHA256 is the one-way transform used for session tokens, CSRF tokens,
// enrollment tokens, and transfer credentials at rest.
func SHA256(b []byte) [32]byte { return sha256.Sum256(b) }

// NewToken returns 32 random bytes as lowercase hex.
func NewToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// SecretBox seals and opens operator secrets with AES-256-GCM. The secret ID
// plus version (purpose) is bound as authenticated data so ciphertexts cannot
// be replayed under a different identity.
type SecretBox struct {
	gcm cipher.AEAD
}

// NewSecretBox loads or generates the 0600 32-byte key at keyPath.
func NewSecretBox(keyPath string) (*SecretBox, error) {
	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read secret key: %w", err)
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate secret key: %w", err)
		}
		if err := os.MkdirAll(dirOf(keyPath), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(keyPath, key, 0o600); err != nil {
			return nil, fmt.Errorf("write secret key: %w", err)
		}
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("secret key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{gcm: gcm}, nil
}

// Seal encrypts value, binding it to (secretID, version).
func (b *SecretBox) Seal(secretID string, value string, version int) (nonce, ciphertext []byte, err error) {
	nonce = make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("nonce: %w", err)
	}
	aad := []byte(fmt.Sprintf("%s|%d", secretID, version))
	ciphertext = b.gcm.Seal(nil, nonce, []byte(value), aad)
	return nonce, ciphertext, nil
}

// Open reverses Seal.
func (b *SecretBox) Open(secretID string, version int, nonce, ciphertext []byte) (string, error) {
	aad := []byte(fmt.Sprintf("%s|%d", secretID, version))
	plain, err := b.gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return "", fmt.Errorf("decrypt secret %s: %w", secretID, err)
	}
	return string(plain), nil
}

// Sessions stores and validates operator sessions. The token presented in the
// cookie is opaque; only its SHA-256 hash is persisted.
type Sessions struct {
	// Store/Create/Delete are supplied by the persistence layer.
	Create func(tokenHash, username, csrfHash, expiresAt string) error
	Get    func(tokenHash string) (username, csrfHash, expiresAt string, err error)
	Delete func(tokenHash string) error
	TTL    time.Duration
}

type Session struct {
	Username string
	Token    string // opaque cookie value
	CSRF     string // token that must be echoed on mutations
	Expires  time.Time
}

// Login creates a session for username.
func (s *Sessions) Login(username string) (*Session, error) {
	token, err := NewToken()
	if err != nil {
		return nil, err
	}
	csrf, err := NewToken()
	if err != nil {
		return nil, err
	}
	expires := time.Now().UTC().Add(s.TTL)
	if err := s.Create(
		fmt.Sprintf("%x", SHA256([]byte(token))),
		username,
		fmt.Sprintf("%x", SHA256([]byte(csrf))),
		expires.Format(time.RFC3339Nano),
	); err != nil {
		return nil, err
	}
	return &Session{Username: username, Token: token, CSRF: csrf, Expires: expires}, nil
}

// Validate checks the session token; when requireCSRF is true it also checks
// the presented CSRF token against the per-session value.
func (s *Sessions) Validate(token, csrf string, requireCSRF bool) (*Session, error) {
	if token == "" {
		return nil, fmt.Errorf("missing session token")
	}
	username, csrfHash, expiresStr, err := s.Get(fmt.Sprintf("%x", SHA256([]byte(token))))
	if err != nil {
		return nil, fmt.Errorf("unknown session")
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresStr)
	if err != nil {
		return nil, fmt.Errorf("bad session expiry")
	}
	if time.Now().UTC().After(expires) {
		s.Delete(fmt.Sprintf("%x", SHA256([]byte(token))))
		return nil, fmt.Errorf("session expired")
	}
	if requireCSRF {
		want := fmt.Sprintf("%x", SHA256([]byte(csrf)))
		if subtle.ConstantTimeCompare([]byte(want), []byte(csrfHash)) != 1 {
			return nil, fmt.Errorf("csrf token mismatch")
		}
	}
	return &Session{Username: username, Token: token, CSRF: csrf, Expires: expires}, nil
}

// Logout deletes the session.
func (s *Sessions) Logout(token string) error {
	return s.Delete(fmt.Sprintf("%x", SHA256([]byte(token))))
}

// MAC signs short server-issued credentials (transfer tokens) with a
// per-process HMAC key so the agent can statelessly verify them.
type MAC struct{ key []byte }

func NewMAC(key []byte) *MAC { return &MAC{key: key} }

func (m *MAC) Sign(payload string) string {
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func (m *MAC) Verify(payload, sig string) bool {
	return hmac.Equal([]byte(m.Sign(payload)), []byte(sig))
}

func dirOf(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
