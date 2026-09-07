package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

const APITokenPrefix = "lmwapi_"

const (
	ScopeDeploymentsRead = "deployments:read"
	ScopeBenchmarksRead  = "benchmarks:read"
	ScopeBenchmarksWrite = "benchmarks:write"
)

var validAPIScopes = map[string]struct{}{
	ScopeDeploymentsRead: {},
	ScopeBenchmarksRead:  {},
	ScopeBenchmarksWrite: {},
}

// APITokenPrincipal is the authenticated service identity attached to a request.
type APITokenPrincipal struct {
	ID     string
	Name   string
	Scopes map[string]struct{}
}

func (p *APITokenPrincipal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	_, ok := p.Scopes[scope]
	return ok
}

type apiTokenContextKey struct{}

func ContextWithAPITokenPrincipal(ctx context.Context, principal *APITokenPrincipal) context.Context {
	return context.WithValue(ctx, apiTokenContextKey{}, principal)
}

func APITokenPrincipalFromContext(ctx context.Context) *APITokenPrincipal {
	principal, _ := ctx.Value(apiTokenContextKey{}).(*APITokenPrincipal)
	return principal
}

// ValidateAPIScopes returns a sorted, duplicate-free scope set.
func ValidateAPIScopes(scopes []string) ([]string, error) {
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if _, ok := validAPIScopes[scope]; !ok {
			return nil, fmt.Errorf("unknown API token scope %q", scope)
		}
		seen[scope] = struct{}{}
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("at least one API token scope is required")
	}
	out := make([]string, 0, len(seen))
	for scope := range seen {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out, nil
}

func NewAPIToken() (string, error) {
	random, err := NewToken()
	if err != nil {
		return "", err
	}
	return APITokenPrefix + random, nil
}

func ValidateAPIToken(token string) bool {
	if len(token) != len(APITokenPrefix)+64 || !strings.HasPrefix(token, APITokenPrefix) {
		return false
	}
	decoded, err := hex.DecodeString(token[len(APITokenPrefix):])
	if err != nil || len(decoded) != 32 {
		return false
	}
	canonical := APITokenPrefix + hex.EncodeToString(decoded)
	return subtle.ConstantTimeCompare([]byte(token), []byte(canonical)) == 1
}

func APITokenHash(token string) string {
	return fmt.Sprintf("%x", SHA256([]byte(token)))
}
