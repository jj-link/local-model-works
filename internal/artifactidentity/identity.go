// Package artifactidentity owns canonical immutable artifact identities.
package artifactidentity

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	revision40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256RE   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hfRepoRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// Canonical combines a source's base identity with its immutable revision or
// digest. Mutable or malformed sources are rejected instead of normalized.
func Canonical(sourceType, identity, revision, digest string) (string, error) {
	switch sourceType {
	case "huggingface":
		base := strings.TrimPrefix(strings.TrimPrefix(identity, "hf://"), "huggingface://")
		if before, after, ok := strings.Cut(base, "@"); ok {
			base = before
			if revision == "" {
				revision = after
			} else if revision != after {
				return "", fmt.Errorf("artifact revision conflicts with identity")
			}
		}
		if !hfRepoRE.MatchString(base) || !revision40.MatchString(revision) {
			return "", fmt.Errorf("Hugging Face identity requires owner/repo and a 40-hex revision")
		}
		return "hf://" + base + "@" + revision, nil
	case "oci":
		base := identity
		if before, after, ok := strings.Cut(identity, "@"); ok {
			base = before
			if digest == "" {
				digest = after
			} else if digest != after {
				return "", fmt.Errorf("artifact digest conflicts with identity")
			}
		}
		if base == "" || strings.ContainsAny(base, " \t\r\n") || !sha256RE.MatchString(digest) {
			return "", fmt.Errorf("OCI identity requires a repository and sha256 digest")
		}
		return base + "@" + digest, nil
	case "file", "local":
		identity = strings.TrimPrefix(identity, "file://")
		if digest == "" && sha256RE.MatchString(identity) {
			digest = identity
		}
		if !sha256RE.MatchString(digest) {
			return "", fmt.Errorf("file identity requires a sha256 digest")
		}
		return "file://" + digest, nil
	default:
		return "", fmt.Errorf("unsupported artifact source type %q", sourceType)
	}
}

type Parsed struct {
	Kind     string
	Revision string
	Digest   string
}

// Parse validates an already-canonical identity and returns persistence facts.
func Parse(identity string) (Parsed, error) {
	if strings.HasPrefix(identity, "hf://") {
		base, revision, ok := strings.Cut(strings.TrimPrefix(identity, "hf://"), "@")
		if !ok {
			return Parsed{}, fmt.Errorf("Hugging Face identity is not revisioned")
		}
		canonical, err := Canonical("huggingface", base, revision, "")
		if err != nil || canonical != identity {
			return Parsed{}, fmt.Errorf("invalid canonical Hugging Face identity")
		}
		return Parsed{Kind: "model", Revision: revision}, nil
	}
	if strings.HasPrefix(identity, "file://") {
		digest := strings.TrimPrefix(identity, "file://")
		if !sha256RE.MatchString(digest) {
			return Parsed{}, fmt.Errorf("invalid canonical file identity")
		}
		return Parsed{Kind: "file", Digest: digest}, nil
	}
	if strings.HasPrefix(identity, "recipe://") {
		digest := strings.TrimPrefix(identity, "recipe://")
		if !sha256RE.MatchString(digest) {
			return Parsed{}, fmt.Errorf("invalid canonical recipe identity")
		}
		return Parsed{Kind: "recipe", Digest: digest}, nil
	}
	base, digest, ok := strings.Cut(identity, "@")
	if ok && base != "" && sha256RE.MatchString(digest) {
		return Parsed{Kind: "oci", Digest: digest}, nil
	}
	return Parsed{}, fmt.Errorf("unsupported canonical artifact identity")
}

// Revision extracts an HF revision from a canonical identity.
func Revision(identity string) string {
	if strings.HasPrefix(identity, "hf://") {
		_, revision, _ := strings.Cut(identity, "@")
		return revision
	}
	return ""
}
