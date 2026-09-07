package recipebuilder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry"
)

const referenceResponseLimit = 1 << 20

var fullCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)
var fullDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ResolveCredential struct {
	Path     string `json:"path"`
	Host     string `json:"host"`
	SecretID string `json:"secret_id"`
}

type FileChecksum struct {
	Path         string `json:"path"`
	URL          string `json:"url"`
	SHA256       string `json:"sha256"`
	EvidenceNote string `json:"evidence_note"`
}

type ResolveRequest struct {
	Credentials   []ResolveCredential `json:"credentials,omitempty"`
	FileChecksums []FileChecksum      `json:"file_checksums,omitempty"`
}

type ReferenceSecretResolver func(context.Context, string, string) (string, error)

type ReferenceResolver struct {
	ResolveSecret ReferenceSecretResolver
	Client        *http.Client
	validateHost  func(context.Context, string) error
}

func (r *ReferenceResolver) ResolveImage(ctx context.Context, identity, expectedHost, secretID string) (string, error) {
	normalized, err := normalizeImageReference(identity)
	if err != nil {
		return "", err
	}
	ref, err := registry.ParseReference(normalized)
	if err != nil {
		return "", newError("recipe.reference_invalid", "container reference is invalid", false)
	}
	host := ref.Registry
	if host == "" || (expectedHost != "" && !strings.EqualFold(host, expectedHost)) {
		return "", newError("recipe.reference_host_mismatch", "credential host does not match the container reference", false)
	}
	networkHost := host
	if host == "docker.io" {
		networkHost = "registry-1.docker.io"
	}
	if err := r.validateDestination(ctx, networkHost); err != nil {
		return "", err
	}
	endpoint := (&url.URL{Scheme: "https", Host: networkHost, Path: path.Join("/v2", ref.Repository, "manifests", ref.Reference)}).String()
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodHead, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json",
	}, ", "))
	if secretID != "" {
		if r.ResolveSecret == nil {
			return "", newError("recipe.reference_credential_unavailable", "selected credential resolver is unavailable", false)
		}
		secret, err := r.ResolveSecret(requestCtx, secretID, "registry")
		if err != nil {
			return "", newError("recipe.reference_credential_unavailable", "selected registry credential is unavailable", false)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := r.client().Do(request)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return "", newError("recipe.reference_timeout", "registry lookup timed out", true)
		}
		return "", newError("recipe.reference_unavailable", "registry lookup failed", true)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") != "" {
		return "", newError("recipe.reference_auth_challenge_unsupported", "registry token challenges are not followed; choose a direct registry credential", false)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", newError("recipe.reference_auth", "registry rejected the selected credential", false)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", newError("recipe.reference_unavailable", fmt.Sprintf("registry returned HTTP %d", response.StatusCode), response.StatusCode >= 500)
	}
	digest := strings.ToLower(strings.TrimSpace(response.Header.Get("Docker-Content-Digest")))
	if !fullDigestRE.MatchString(digest) {
		return "", newError("recipe.reference_invalid", "registry did not return a verified manifest digest", false)
	}
	if fullDigestRE.MatchString(ref.Reference) && ref.Reference != digest {
		return "", newError("recipe.reference_digest_mismatch", "registry returned a different digest than the immutable reference", false)
	}
	return digest, nil
}

func (r *ReferenceResolver) ResolveHuggingFace(ctx context.Context, identity, revision, secretID string) (string, error) {
	model := strings.TrimPrefix(strings.TrimSpace(identity), "hf://")
	parts := strings.Split(model, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", newError("recipe.reference_invalid", "Hugging Face identity must be hf://namespace/repository", false)
	}
	if revision == "" {
		revision = "main"
	}
	endpoint := "https://huggingface.co/api/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1]) + "/revision/" + url.PathEscape(revision) + "?expand[]=sha"
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := r.validateDestination(requestCtx, "huggingface.co"); err != nil {
		return "", err
	}
	request, _ := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if secretID != "" {
		if r.ResolveSecret == nil {
			return "", newError("recipe.reference_credential_unavailable", "selected credential resolver is unavailable", false)
		}
		secret, err := r.ResolveSecret(requestCtx, secretID, "huggingface")
		if err != nil {
			return "", newError("recipe.reference_credential_unavailable", "selected Hugging Face credential is unavailable", false)
		}
		request.Header.Set("Authorization", "Bearer "+secret)
	}
	response, err := r.client().Do(request)
	if err != nil {
		if errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			return "", newError("recipe.reference_timeout", "Hugging Face lookup timed out", true)
		}
		return "", newError("recipe.reference_unavailable", "Hugging Face lookup failed", true)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return "", newError("recipe.reference_auth", "Hugging Face rejected the selected credential", false)
	}
	if response.StatusCode == http.StatusNotFound {
		return "", newError("recipe.reference_unknown", "Hugging Face model or revision does not exist", false)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", newError("recipe.reference_unavailable", fmt.Sprintf("Hugging Face returned HTTP %d", response.StatusCode), response.StatusCode >= 500)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, referenceResponseLimit+1))
	if err != nil || len(body) > referenceResponseLimit {
		return "", newError("recipe.reference_invalid", "Hugging Face metadata response exceeded the 1 MiB limit", false)
	}
	var metadata struct {
		ID  string `json:"id"`
		SHA string `json:"sha"`
	}
	if json.Unmarshal(body, &metadata) != nil || !strings.EqualFold(metadata.ID, model) || !fullCommitRE.MatchString(strings.ToLower(metadata.SHA)) {
		return "", newError("recipe.reference_invalid", "Hugging Face returned mismatched or invalid model metadata", false)
	}
	if fullCommitRE.MatchString(strings.ToLower(revision)) && !strings.EqualFold(revision, metadata.SHA) {
		return "", newError("recipe.reference_digest_mismatch", "Hugging Face returned a different revision than the supplied immutable commit", false)
	}
	return strings.ToLower(metadata.SHA), nil
}

func (r *ReferenceResolver) client() *http.Client {
	if r.Client != nil {
		client := *r.Client
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		return &client
	}
	return &http.Client{Transport: &http.Transport{DialContext: safeDialContext}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func safeDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			return nil, newError("recipe.reference_destination_forbidden", "reference host resolved to a private or local address", false)
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("reference host resolved no addresses")
	}
	return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
}

func (r *ReferenceResolver) validateDestination(ctx context.Context, host string) error {
	if r.validateHost != nil {
		return r.validateHost(ctx, host)
	}
	return validatePublicHost(ctx, host)
}

func validatePublicHost(ctx context.Context, host string) error {
	if strings.EqualFold(host, "localhost") {
		return newError("recipe.reference_destination_forbidden", "local reference destinations are forbidden", false)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return newError("recipe.reference_destination_forbidden", "private or local reference destinations are forbidden", false)
		}
		return nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return newError("recipe.reference_unavailable", "reference host could not be resolved", true)
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			return newError("recipe.reference_destination_forbidden", "reference host resolved to a private or local address", false)
		}
	}
	return nil
}

func publicIP(ip net.IP) bool {
	return !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified())
}

func checksumEvidence(input FileChecksum) (ResolvedReference, error) {
	parsed, err := url.Parse(input.URL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || !fullDigestRE.MatchString("sha256:"+strings.ToLower(input.SHA256)) || strings.TrimSpace(input.EvidenceNote) == "" {
		return ResolvedReference{}, newError("recipe.reference_operator_invalid", "operator checksum requires an exact HTTPS URL, SHA-256, and evidence note", false)
	}
	return ResolvedReference{Path: input.Path, InputIdentity: input.URL + "#sha256=" + strings.ToLower(input.SHA256), ResolvedValue: "sha256:" + strings.ToLower(input.SHA256), Origin: "operator", EvidenceNote: input.EvidenceNote, VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func referenceCredential(pathValue string, credentials []ResolveCredential, host string) (string, error) {
	for _, credential := range credentials {
		if credential.Path == pathValue {
			if !strings.EqualFold(credential.Host, host) {
				return "", newError("recipe.reference_host_mismatch", "credential host does not match the reference", false)
			}
			return credential.SecretID, nil
		}
	}
	return "", nil
}

func referenceIdentityHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func normalizeImageReference(identity string) (string, error) {
	identity = strings.TrimSpace(identity)
	first, _, hasSlash := strings.Cut(identity, "/")
	if !hasSlash {
		identity = "docker.io/library/" + identity
	} else if !strings.ContainsAny(first, ".:") && first != "localhost" {
		identity = "docker.io/" + identity
	}
	if strings.HasPrefix(identity, "docker.io/") && !strings.Contains(strings.TrimPrefix(identity, "docker.io/"), "/") {
		identity = "docker.io/library/" + strings.TrimPrefix(identity, "docker.io/")
	}
	ref, err := registry.ParseReference(identity)
	if err != nil {
		return "", newError("recipe.reference_invalid", "container reference is invalid", false)
	}
	if ref.Reference == "" {
		identity += ":latest"
	}
	return identity, nil
}
