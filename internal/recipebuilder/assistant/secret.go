package assistant

import (
	"context"

	"github.com/jj-link/local-model-works/internal/auth"
	"github.com/jj-link/local-model-works/internal/db"
)

const secretVersion = 1

type SecretStore interface {
	GetSecret(context.Context, string) (db.Secret, error)
}

// EncryptedSecretResolver opens only secrets explicitly created for recipe
// assistance. Callers retain the plaintext only for the outbound request.
func EncryptedSecretResolver(store SecretStore, box *auth.SecretBox) SecretResolver {
	return func(ctx context.Context, id string) (string, error) {
		secret, err := store.GetSecret(ctx, id)
		if err != nil {
			return "", err
		}
		if secret.Purpose != "recipe-assistant" {
			return "", &Error{Code: "assistant.secret_purpose", Message: "selected secret is not a recipe-assistant credential", Retryable: false}
		}
		value, err := box.Open(secret.ID, secretVersion, secret.Nonce, secret.Ciphertext)
		if err != nil {
			return "", &Error{Code: "assistant.secret_unavailable", Message: "selected provider credential cannot be decrypted", Retryable: false}
		}
		return value, nil
	}
}
