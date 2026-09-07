package recipebuilder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestReferenceResolverVerifiesRegistryDigestAtSelectedHost(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	secretCalls := 0
	resolver := &ReferenceResolver{
		validateHost: func(context.Context, string) error { return nil },
		ResolveSecret: func(_ context.Context, id, purpose string) (string, error) {
			secretCalls++
			if id != "registry-secret" || purpose != "registry" {
				t.Fatalf("secret id=%q purpose=%q", id, purpose)
			}
			return "credential", nil
		},
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != "registry.example" || request.URL.Path != "/v2/team/model/manifests/v1" || request.Header.Get("Authorization") != "Bearer credential" {
				t.Fatalf("request URL=%s authorization=%q", request.URL, request.Header.Get("Authorization"))
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Docker-Content-Digest": []string{digest}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})},
	}
	got, err := resolver.ResolveImage(context.Background(), "registry.example/team/model:v1", "registry.example", "registry-secret")
	if err != nil || got != digest || secretCalls != 1 {
		t.Fatalf("digest=%q calls=%d err=%v", got, secretCalls, err)
	}
}

func TestReferenceResolverRejectsHostMismatchAndAuthChallenge(t *testing.T) {
	secretCalls := 0
	resolver := &ReferenceResolver{validateHost: func(context.Context, string) error { return nil }, ResolveSecret: func(context.Context, string, string) (string, error) { secretCalls++; return "secret", nil }}
	if _, err := resolver.ResolveImage(context.Background(), "registry.example/team/model:v1", "other.example", "id"); errorCode(err) != "recipe.reference_host_mismatch" || secretCalls != 0 {
		t.Fatalf("mismatch err=%v secretCalls=%d", err, secretCalls)
	}
	resolver.Client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		header := make(http.Header)
		header.Set("WWW-Authenticate", `Bearer realm="https://auth.other/token"`)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if _, err := resolver.ResolveImage(context.Background(), "registry.example/team/model:v1", "registry.example", "id"); errorCode(err) != "recipe.reference_auth_challenge_unsupported" || secretCalls != 1 {
		t.Fatalf("challenge err=%v secretCalls=%d", err, secretCalls)
	}
}

func TestReferenceResolverVerifiesHuggingFaceIdentityAndCommit(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	resolver := &ReferenceResolver{
		validateHost: func(context.Context, string) error { return nil },
		Client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/api/models/openai-community/gpt2/revision/main" {
				t.Fatalf("path=%q", request.URL.Path)
			}
			body, _ := json.Marshal(map[string]string{"id": "openai-community/gpt2", "sha": commit})
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
		})},
	}
	got, err := resolver.ResolveHuggingFace(context.Background(), "hf://openai-community/gpt2", "", "")
	if err != nil || got != commit {
		t.Fatalf("commit=%q err=%v", got, err)
	}
}

func TestReferenceResolverRejectsPrivateDestinations(t *testing.T) {
	if err := validatePublicHost(context.Background(), "127.0.0.1"); errorCode(err) != "recipe.reference_destination_forbidden" {
		t.Fatalf("private destination err=%v", err)
	}
	if _, err := checksumEvidence(FileChecksum{Path: "/artifacts/0/source", URL: "http://example.com/file", SHA256: strings.Repeat("a", 64), EvidenceNote: "operator verified"}); errorCode(err) != "recipe.reference_operator_invalid" {
		t.Fatalf("operator evidence err=%v", err)
	}
}

func TestPinnedHuggingFaceRevisionCannotResolveToDifferentBytes(t *testing.T) {
	resolver := &ReferenceResolver{validateHost: func(context.Context, string) error { return nil }, Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":"team/model","sha":"` + strings.Repeat("b", 40) + `"}`)), Header: make(http.Header)}, nil
	})}}
	if _, err := resolver.ResolveHuggingFace(context.Background(), "hf://team/model", strings.Repeat("a", 40), ""); errorCode(err) != "recipe.reference_digest_mismatch" {
		t.Fatalf("immutable mismatch accepted: %v", err)
	}
}

func TestBareDockerImageUsesCanonicalIdentityAndRegistryNetworkHost(t *testing.T) {
	resolver := &ReferenceResolver{validateHost: func(_ context.Context, host string) error {
		if host != "registry-1.docker.io" {
			t.Fatalf("validated host %s", host)
		}
		return nil
	}, Client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "registry-1.docker.io" || r.URL.Path != "/v2/library/ubuntu/manifests/latest" {
			t.Fatalf("incorrect Docker request %s", r.URL)
		}
		header := make(http.Header)
		header.Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: header}, nil
	})}}
	if _, err := resolver.ResolveImage(context.Background(), "ubuntu", "docker.io", ""); err != nil {
		t.Fatal(err)
	}
}
