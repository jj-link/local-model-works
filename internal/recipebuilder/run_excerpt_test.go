package recipebuilder

import (
	"strings"
	"testing"
)

func TestRedactRunExcerptRemovesCredentialForms(t *testing.T) {
	input := []byte("Authorization: Bearer visible-token\napi_key=visible-key\nghp_abcdefghijklmnopqrstuvwxyz\nkeep this diagnostic\n")
	redacted := RedactRunExcerpt(input)
	for _, secret := range []string{"visible-token", "visible-key", "ghp_abcdefghijklmnopqrstuvwxyz"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redacted excerpt contains %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "keep this diagnostic") || strings.Count(redacted, "[REDACTED]") != 3 {
		t.Fatalf("redacted excerpt = %q", redacted)
	}
}
