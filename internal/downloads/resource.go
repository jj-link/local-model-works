package downloads

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/jj-link/local-model-works/internal/artifactidentity"
)

var (
	resourceDigestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	resourcePlatformPattern = regexp.MustCompile(`^[a-z0-9]+/[a-z0-9_]+(?:/[a-z0-9_.-]+)?$`)
	ociComponentPattern     = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
)

// DecodeResourceSpec is the shared protocol boundary. It performs structural
// and immutable-identity checks only: the agent must still authorize the exact
// configured destination, source transport and platform before inspecting or
// acquiring anything. No JSON extension point can carry a command or script.
func DecodeResourceSpec(data []byte) (ResourceSpec, error) {
	return decodeResourceSpec(data, false)
}

// DecodeInspectionSpec additionally permits discovering an image's platform
// manifest from an exact immutable index already present in the device engine.
// This exception is read-only: FETCH must always use DecodeResourceSpec.
func DecodeInspectionSpec(data []byte) (ResourceSpec, error) {
	return decodeResourceSpec(data, true)
}

func decodeResourceSpec(data []byte, inspect bool) (ResourceSpec, error) {
	var spec ResourceSpec
	if len(data) == 0 || len(data) > MaxResourceJSONBytes {
		return spec, fmt.Errorf("download.resource_invalid: resource JSON must be 1..%d bytes", MaxResourceJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return ResourceSpec{}, fmt.Errorf("download.resource_invalid: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return ResourceSpec{}, fmt.Errorf("download.resource_invalid: expected one JSON resource")
	}
	validate := spec.Validate
	if inspect {
		validate = spec.ValidateInspection
	}
	if err := validate(); err != nil {
		return ResourceSpec{}, err
	}
	return spec, nil
}

func (s ResourceSpec) ValidateInspection() error {
	if s.Kind == ResourceImage && s.ManifestDigest == "" {
		s.ManifestDigest = s.IndexDigest
	}
	return s.Validate()
}

// Validate does not establish file availability. The pinned source metadata and
// actual bytes must be verified independently, including all recipe layers and
// the exact image platform manifest. A locally computed hash alone is not proof.
func (s ResourceSpec) Validate() error {
	invalid := func(reason string) error {
		return fmt.Errorf("download.resource_invalid: %s", reason)
	}
	if !boundedText(s.Identity, MaxIdentityBytes) || !boundedText(s.Destination, MaxDestinationBytes) {
		return invalid("identity or destination is empty, oversized or contains control characters")
	}
	if !path.IsAbs(s.Destination) || s.Destination == "/" || path.Clean(s.Destination) != s.Destination || strings.Contains(s.Destination, `\`) {
		return invalid("destination must be an exact clean absolute path, not a filesystem root")
	}
	if s.SizeBytes != nil && *s.SizeBytes < 0 {
		return invalid("size_bytes cannot be negative; omit unknown sizes")
	}
	if len(s.Source.Reference) > MaxIdentityBytes || len(s.Source.URL) > MaxSourceURLBytes || len(s.Source.Revision) > 40 || len(s.Source.Digest) > 71 {
		return invalid("source exceeds protocol bounds")
	}
	for _, text := range []string{s.Source.Reference, s.Source.URL, s.Source.Revision, s.Source.Digest} {
		if strings.IndexFunc(text, unicode.IsControl) >= 0 {
			return invalid("source contains control characters")
		}
	}

	var canonical string
	var err error
	switch s.Source.Type {
	case SourceRecipe:
		if s.Kind != ResourceRecipe || !resourceDigestPattern.MatchString(s.Source.Digest) || s.Source.Reference != "" || s.Source.URL != "" || s.Source.Revision != "" {
			return invalid("recipe source requires only its immutable package digest")
		}
		canonical = "recipe://" + s.Source.Digest
	case SourceHuggingFace:
		if s.Kind != ResourceArtifact || strings.ContainsAny(s.Source.Reference, ":@") || s.Source.URL != "" || s.Source.Digest != "" {
			return invalid("Hugging Face artifact requires repository and immutable revision only")
		}
		canonical, err = artifactidentity.Canonical(string(s.Source.Type), s.Source.Reference, s.Source.Revision, "")
	case SourceOCI:
		if (s.Kind != ResourceArtifact && s.Kind != ResourceImage) || !validOCIRepository(s.Source.Reference) || s.Source.URL != "" || s.Source.Revision != "" {
			return invalid("OCI source requires a canonical registry/repository and digest only")
		}
		canonical, err = artifactidentity.Canonical(string(s.Source.Type), s.Source.Reference, "", s.Source.Digest)
	case SourceFile, SourceLocal:
		if s.Kind != ResourceArtifact || s.Source.Reference != "" || s.Source.Revision != "" || (s.Source.Type == SourceLocal && s.Source.URL != "") {
			return invalid("file/local artifact requires its checksum and optional file HTTPS URL only")
		}
		if s.Source.URL != "" {
			u, parseErr := url.Parse(s.Source.URL)
			if parseErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
				return invalid("file URL must be absolute HTTPS without embedded credentials or fragment")
			}
		}
		canonical, err = artifactidentity.Canonical(string(s.Source.Type), "", "", s.Source.Digest)
	default:
		return invalid("unsupported source type")
	}
	if err != nil || canonical != s.Identity {
		return invalid("identity does not equal the canonical immutable source identity")
	}
	if s.Kind == ResourceImage {
		if len(s.Platform) > 128 || !resourcePlatformPattern.MatchString(s.Platform) || !resourceDigestPattern.MatchString(s.IndexDigest) || !resourceDigestPattern.MatchString(s.ManifestDigest) || s.IndexDigest != s.Source.Digest {
			return invalid("image requires platform and matching index/platform-manifest digests")
		}
	} else if s.Platform != "" || s.IndexDigest != "" || s.ManifestDigest != "" {
		return invalid("non-image resource cannot declare image platform or manifests")
	}
	return nil
}

func boundedText(text string, limit int) bool {
	return text != "" && len(text) <= limit && strings.IndexFunc(text, unicode.IsControl) < 0
}

func validOCIRepository(reference string) bool {
	registry, repository, ok := strings.Cut(reference, "/")
	if !ok || registry == "" || repository == "" || strings.ContainsAny(reference, "@?# \\ \t\r\n") || registry != strings.ToLower(registry) {
		return false
	}
	// Require an explicit registry host. Docker Hub aliases are normalized by
	// reference verification, not silently changed by the acquisition receiver.
	u, err := url.Parse("https://" + registry)
	if err != nil || u.Host != registry || u.User != nil || u.Hostname() == "" || (!strings.ContainsAny(registry, ".:") && registry != "localhost") || registry == "registry-1.docker.io" || registry == "index.docker.io" {
		return false
	}
	for _, component := range strings.Split(repository, "/") {
		if !ociComponentPattern.MatchString(component) {
			return false
		}
	}
	return true
}
