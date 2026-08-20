package artifactidentity

import (
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	revision := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	cases := []struct {
		typeName, identity, revision, digest, want string
	}{
		{"huggingface", "hf://Qwen/Qwen3.8-27B", revision, "", "hf://Qwen/Qwen3.8-27B@" + revision},
		{"huggingface", "huggingface://Qwen/Qwen3.8-27B@" + revision, "", "", "hf://Qwen/Qwen3.8-27B@" + revision},
		{"oci", "ghcr.io/acme/model", "", digest, "ghcr.io/acme/model@" + digest},
		{"file", "ignored-name", "", digest, "file://" + digest},
	}
	for _, test := range cases {
		got, err := Canonical(test.typeName, test.identity, test.revision, test.digest)
		if err != nil || got != test.want {
			t.Fatalf("Canonical(%q, %q) = %q, %v; want %q", test.typeName, test.identity, got, err, test.want)
		}
	}
}

func TestCanonicalRejectsMutableAndConflictingSources(t *testing.T) {
	revision := strings.Repeat("a", 40)
	digest := "sha256:" + strings.Repeat("b", 64)
	for _, args := range [][4]string{
		{"huggingface", "hf://Qwen/Qwen3.8-27B", "main", ""},
		{"huggingface", "hf://Qwen/Qwen3.8-27B@" + revision, strings.Repeat("c", 40), ""},
		{"oci", "ghcr.io/acme/model:latest", "", ""},
		{"oci", "ghcr.io/acme/model@" + digest, "", "sha256:" + strings.Repeat("c", 64)},
		{"file", "/host/path", "", ""},
	} {
		if got, err := Canonical(args[0], args[1], args[2], args[3]); err == nil {
			t.Fatalf("mutable source accepted as %q: %v", got, args)
		}
	}
}
