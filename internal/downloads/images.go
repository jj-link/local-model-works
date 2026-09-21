package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jj-link/local-model-works/internal/recipe"
	registryauth "oras.land/oras-go/v2/registry/remote/auth"
)

func resourceHost(spec ResourceSpec) string {
	switch spec.Source.Type {
	case SourceHuggingFace:
		return "huggingface.co"
	case SourceOCI:
		host, _, _ := strings.Cut(spec.Source.Reference, "/")
		return host
	case SourceFile:
		u, _ := url.Parse(spec.Source.URL)
		if u != nil {
			return u.Host
		}
	}
	return ""
}
func (s *Service) credential(ctx context.Context, spec ResourceSpec, selections []CredentialSelection) (*CredentialMaterial, error) {
	for _, selection := range selections {
		if selection.Resource != spec.Identity {
			continue
		}
		host := resourceHost(spec)
		if selection.Host != host || host == "" {
			return nil, failure("download.credential_host_mismatch", "Credential host does not match the selected immutable resource", 422)
		}
		purpose := "registry"
		if spec.Source.Type == SourceHuggingFace {
			purpose = "huggingface"
		}
		if spec.Source.Type != SourceOCI && spec.Source.Type != SourceHuggingFace {
			return nil, failure("download.credential_format", "This source does not support scoped credential material", 422)
		}
		if s.secrets == nil {
			return nil, failure("download.credential_unavailable", "Secret store is unavailable", 422)
		}
		secret, err := s.q.GetSecret(ctx, selection.SecretID)
		if err != nil || secret.Purpose != purpose {
			return nil, failure("download.credential_unavailable", "Selected credential is unavailable for this purpose", 422)
		}
		value, err := s.secrets.Open(secret.ID, 1, secret.Nonce, secret.Ciphertext)
		if err != nil {
			return nil, failure("download.credential_unavailable", "Cannot open the selected credential", 422)
		}
		return &CredentialMaterial{Purpose: purpose, Host: host, Value: value}, nil
	}
	return nil, nil
}
func normalizeImage(ref string) string {
	ref = strings.TrimPrefix(ref, "oci://")
	ref = strings.Split(ref, "@")[0]
	lastSlash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > lastSlash {
		ref = ref[:colon]
	}
	first, _, slash := strings.Cut(ref, "/")
	if !slash {
		return "docker.io/library/" + ref
	}
	if !strings.ContainsAny(first, ".:") && first != "localhost" {
		return "docker.io/" + ref
	}
	if first == "registry-1.docker.io" || first == "index.docker.io" {
		return "docker.io" + ref[len(first):]
	}
	return ref
}

// CanonicalImagePlatform treats ARM64's optional v8 variant as its baseline.
// Other explicit variants remain distinct.
func CanonicalImagePlatform(os, architecture, variant string) string {
	if architecture == "arm64" && variant == "v8" {
		variant = ""
	}
	platform := os + "/" + architecture
	if variant != "" {
		platform += "/" + variant
	}
	return platform
}

func (s *Service) imageSpec(ctx context.Context, image recipe.Image, inv nodeInventory, node string, selections []CredentialSelection) (ResourceSpec, error) {
	spec := ResourceSpec{Kind: ResourceImage, Source: SourceSpec{Type: SourceOCI, Reference: normalizeImage(image.Reference), Digest: image.Digest}, Destination: inv.DownloadRoots.ImageRoot, Platform: inv.DownloadRoots.Platform, IndexDigest: image.Digest}
	if spec.Destination == "" || spec.Platform == "" {
		return spec, failure("download.image_storage_unknown", "Agent must report actual engine storage and platform", 422)
	}
	spec.Identity = spec.Source.Reference + "@" + image.Digest
	if err := spec.ValidateInspection(); err != nil {
		return spec, err
	}
	if s.nodes.Online(node) && hasFeature(inv) {
		observed, err := s.inspect(ctx, node, spec, selections)
		if err == nil && observed.State == ResourceAvailable {
			spec.ManifestDigest = observed.ManifestDigest
			spec.SizeBytes = observed.SizeBytes
			return spec, spec.Validate()
		}
	}
	// A previously reviewed index/platform mapping is immutable metadata, not an
	// availability claim. Reuse it without requiring the origin to be online.
	var retained string
	if err := s.db.QueryRowContext(ctx, `SELECT resource.value FROM runs, json_each(runs.input, '$.plan.resources') AS resource WHERE runs.module = 'library' AND runs.kind = 'recipe-download' AND runs.state = 'succeeded' AND json_extract(resource.value, '$.kind') = 'image' AND json_extract(resource.value, '$.identity') = ? AND json_extract(resource.value, '$.platform') = ? ORDER BY runs.created_at DESC LIMIT 1`, spec.Identity, spec.Platform).Scan(&retained); err == nil {
		var prior Resource
		if json.Unmarshal([]byte(retained), &prior) == nil && prior.ResourceSpec.Validate() == nil && prior.IndexDigest == spec.IndexDigest {
			spec.ManifestDigest = prior.ManifestDigest
			return spec, nil
		}
	}
	material, err := s.credential(ctx, spec, selections)
	if err != nil {
		return spec, err
	}
	spec.ManifestDigest = spec.IndexDigest
	raw, err := registryDocument(ctx, spec.Source.Reference, image.Digest, material, "manifests")
	if err != nil {
		return spec, err
	}
	type descriptor struct {
		Digest   string `json:"digest"`
		Size     int64  `json:"size"`
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	}
	var doc struct {
		MediaType string       `json:"mediaType"`
		Manifests []descriptor `json:"manifests"`
		Layers    []descriptor `json:"layers"`
		Config    descriptor   `json:"config"`
	}
	if err = json.Unmarshal(raw, &doc); err != nil {
		return spec, failure("download.image_metadata_invalid", "Image metadata is invalid", 422)
	}
	if len(doc.Manifests) > 0 {
		selected := ""
		for _, d := range doc.Manifests {
			platform := CanonicalImagePlatform(d.Platform.OS, d.Platform.Architecture, d.Platform.Variant)
			if platform == spec.Platform {
				if selected != "" && selected != d.Digest {
					return spec, failure("download.image_platform_ambiguous", "Pinned index has multiple matching platform manifests", 422)
				}
				selected = d.Digest
			}
		}
		if selected == "" {
			return spec, failure("download.image_platform_unavailable", "Pinned image has no manifest for "+spec.Platform, 422)
		}
		spec.ManifestDigest = selected
		raw, err = registryDocument(ctx, spec.Source.Reference, selected, material, "manifests")
		if err != nil {
			return spec, err
		}
		doc.Manifests = nil
		doc.Layers = nil
		if err = json.Unmarshal(raw, &doc); err != nil {
			return spec, err
		}
	}
	if len(doc.Layers) == 0 || !resourceDigestPattern.MatchString(doc.Config.Digest) {
		return spec, failure("download.image_metadata_invalid", "Pinned image has no bounded layer/config metadata", 422)
	}
	// The config independently proves the platform even for a single manifest.
	config, err := registryDocument(ctx, spec.Source.Reference, doc.Config.Digest, material, "blobs")
	if err != nil {
		return spec, err
	}
	var platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	}
	if json.Unmarshal(config, &platform) != nil {
		return spec, failure("download.image_metadata_invalid", "Image config cannot establish platform", 422)
	}
	actual := CanonicalImagePlatform(platform.OS, platform.Architecture, platform.Variant)
	if actual != spec.Platform {
		return spec, failure("download.image_platform_mismatch", "Image config does not match selected device platform", 422)
	}
	seen := map[string]bool{}
	for _, layer := range doc.Layers {
		if seen[layer.Digest] {
			continue
		}
		seen[layer.Digest] = true
		if !resourceDigestPattern.MatchString(layer.Digest) || layer.Size < 0 {
			return spec, failure("download.image_metadata_invalid", "Layer size/digest is invalid", 422)
		}
	}
	// Registry descriptors describe compressed transport bytes, not expanded
	// engine storage. Only a safe agent/engine estimate may establish capacity.
	spec.SizeBytes = nil
	return spec, spec.Validate()
}
func registryDocument(ctx context.Context, reference, digest string, material *CredentialMaterial, kind string) ([]byte, error) {
	if !resourceDigestPattern.MatchString(digest) {
		return nil, failure("download.image_unpinned", "Image metadata digest is not immutable", 422)
	}
	host, repository, ok := strings.Cut(reference, "/")
	if !ok {
		return nil, failure("download.image_invalid", "Image has no registry repository", 422)
	}
	networkHost := host
	if host == "docker.io" {
		networkHost = "registry-1.docker.io"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+networkHost+"/v2/"+repository+"/"+kind+"/"+digest, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	if material != nil {
		if material.Host != host || strings.ContainsAny(material.Value, "\r\n") || strings.HasPrefix(strings.TrimSpace(material.Value), "{") {
			return nil, failure("download.credential_format", "Registry requires a direct bearer credential in the saved registry format", 422)
		}
		request.Header.Set("Authorization", "Bearer "+material.Value)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		for _, ip := range addresses {
			if ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsUnspecified() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsMulticast() {
				return nil, failure("download.registry_destination_blocked", "Registry resolved to a nonpublic address", 422)
			}
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("registry address unavailable")
		}
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) > 10 || request.URL.Scheme != "https" || request.URL.User != nil {
			return fmt.Errorf("download.redirect_unsafe")
		}
		request.Header.Del("Authorization")
		return nil
	}}
	response, err := (&registryauth.Client{Client: client}).Do(request)
	if err != nil {
		return nil, failure("download.image_metadata_unavailable", "Registry metadata request failed", 422)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, failure("download.image_metadata_unavailable", fmt.Sprintf("Registry metadata returned HTTP %d", response.StatusCode), 422)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return nil, failure("download.image_metadata_invalid", "Registry metadata exceeds the bounded response size", 422)
	}
	sum := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, failure("download.image_digest_mismatch", "Registry metadata does not match the pinned content digest", 422)
	}
	return raw, nil
}
