package recipe

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidatorReturnsStructuredSchemaInstancePointers(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := validator.Validate([]byte(`{"apiVersion":7}`))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range findings {
		if finding.Code == "recipe.schema" && finding.Path == "/apiVersion" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("schema pointer missing: %+v", findings)
	}
}

func TestValidatorIdentifiesMissingNestedImageDigest(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := validator.Validate([]byte(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"missing-image-digest","version":"1.0.0","description":"Nested diagnostic regression","license":"MIT"},"compatibility":{"nodeCount":1},"artifacts":[],"workloads":[{"image":{"reference":"docker.io/library/busybox:latest"},"command":["/bin/true"],"args":[],"resources":{"pids":64}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		if finding.Path == "/workloads/0/image" && strings.Contains(finding.Message, "digest") {
			return
		}
	}
	t.Fatalf("schema failure did not identify the missing image digest: %+v", findings)
}

func TestValidatorAllowsHTTPSOriginsOnlyForChecksumPinnedFiles(t *testing.T) {
	validator, err := NewValidator()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		name     string
		kind     string
		identity string
		digest   string
		valid    bool
	}{
		{"https-file", "file", "https://huggingface.co/owner/model/resolve/" + strings.Repeat("a", 40) + "/weights.gguf", "sha256:" + strings.Repeat("b", 64), true},
		{"https-query", "file", "https://example.com/weights.gguf?download=true", "sha256:" + strings.Repeat("b", 64), true},
		{"legacy-file", "file", "example.com/weights.gguf", "sha256:" + strings.Repeat("b", 64), true},
		{"http-file", "file", "http://example.com/weights.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"credentials", "file", "https://user:secret@example.com/weights.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"fragment", "file", "https://example.com/weights.gguf#fragment", "sha256:" + strings.Repeat("b", 64), false},
		{"missing-host", "file", "https:///weights.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"empty-host-with-port", "file", "https://:443/weights.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"invalid-escape", "file", "https://example.com/%zz", "sha256:" + strings.Repeat("b", 64), false},
		{"nul-control", "file", "https://example.com/weights\x00.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"unicode-control", "file", "https://example.com/weights\u009f.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"whitespace", "file", "https://example.com/weights file.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"control", "file", "https://example.com/weights\n.gguf", "sha256:" + strings.Repeat("b", 64), false},
		{"unpinned", "file", "https://example.com/weights.gguf", "", false},
		{"https-huggingface", "huggingface", "https://huggingface.co/owner/model", "sha256:" + strings.Repeat("b", 64), false},
		{"https-oci", "oci", "https://example.com/image", "sha256:" + strings.Repeat("b", 64), false},
	} {
		t.Run(item.name, func(t *testing.T) {
			var document map[string]any
			err := json.Unmarshal([]byte(`{"apiVersion":"localmodelworks/v1alpha1","kind":"Recipe","metadata":{"name":"file-origin","version":"1.0.0","description":"Pinned file origin","license":"MIT","source":{"url":"https://example.com/recipe","revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","path":"."}},"compatibility":{"nodeCount":1},"workloads":[{"image":{"reference":"example.com/runtime","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"command":["serve"],"args":[],"resources":{"pids":64}}]}`), &document)
			if err != nil {
				t.Fatal(err)
			}
			source := map[string]any{"type": item.kind, "identity": item.identity, "revision": strings.Repeat("a", 40)}
			if item.digest != "" {
				source["digest"] = item.digest
			}
			document["artifacts"] = []any{map[string]any{"name": "weights", "kind": "file", "source": source, "mount": "/models/weights.gguf", "validation": "file"}}
			encoded, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			findings, err := validator.Validate(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if (len(findings) == 0) != item.valid {
				t.Fatalf("valid = %t, findings: %+v", item.valid, findings)
			}
		})
	}
}
