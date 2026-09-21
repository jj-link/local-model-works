package recipe

import (
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
