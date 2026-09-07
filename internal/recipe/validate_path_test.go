package recipe

import "testing"

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
