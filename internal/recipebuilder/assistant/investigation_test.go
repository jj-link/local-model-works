package assistant

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestEvidenceChunksRetainLongUTF8LinesAndProvenance(t *testing.T) {
	content := strings.Repeat("界", evidenceChunkBytes) + "\nnext line\n" + strings.Repeat("é", evidenceChunkBytes)
	file := ContextFile{Path: "launch.py", Content: content, SHA256: "whole-file-hash", StartLine: 7}
	var recovered strings.Builder
	line := 7
	for _, part := range splitEvidence(file) {
		if !utf8.ValidString(part.Content) || len(part.Content) > evidenceChunkBytes {
			t.Fatal("bounded context split invalidated UTF-8 or exceeded the provider bound")
		}
		end := line + strings.Count(part.Content, "\n")
		if strings.HasSuffix(part.Content, "\n") {
			end--
		}
		if part.StartLine != line || part.EndLine != end || part.SHA256 != file.SHA256 {
			t.Fatalf("chunk lost original evidence identity/range: %#v", part)
		}
		recovered.WriteString(part.Content)
		line += strings.Count(part.Content, "\n")
	}
	if recovered.String() != content {
		t.Fatal("provider context lost or duplicated source bytes")
	}
}

func TestInvestigationRejectsTruncatedPinnedSource(t *testing.T) {
	original := "documented command\nrequired launch argument\n"
	hash := sha256.Sum256([]byte(original))
	digest := hex.EncodeToString(hash[:])
	_, err := investigate(context.Background(), Request{
		Mode:            "add",
		SourceInventory: []SourceFile{{Path: "run.sh", SHA256: digest, Size: int64(len(original)), Readable: true}},
		ReadSource: func(context.Context, string) (ContextFile, error) {
			return ContextFile{Path: "run.sh", SHA256: digest, Content: "documented command\n"}, nil
		},
	}, nil, func(context.Context, Request, string, map[string]any, func(string)) ([]byte, error) {
		t.Fatal("unverified source was sent to the provider")
		return nil, nil
	})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "assistant.source_changed" {
		t.Fatalf("truncated pinned instructions were not rejected: %v", err)
	}
}

func TestInvestigationSchemaBindsLinksToTheirActualSource(t *testing.T) {
	raw, err := json.Marshal(investigationOutputSchema(map[string]map[string]bool{
		"README.md": {"https://example.com/launch": true},
		"BUILD.md":  {"https://example.com/build": true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("investigation-output", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("investigation-output")
	if err != nil {
		t.Fatal(err)
	}
	link := map[string]any{"url": "https://example.com/launch", "source_path": "README.md", "reason": "Documented launch prerequisites"}
	result := map[string]any{"findings": "Documented procedure.", "links": []any{link}}
	if err := schema.Validate(result); err != nil {
		t.Fatalf("explicit source link was rejected: %v", err)
	}
	link["url"] = "https://example.com/invented"
	if err := schema.Validate(result); err == nil {
		t.Fatal("invented instruction URL was allowed")
	}
	link["url"] = "https://example.com/build"
	if err := schema.Validate(result); err == nil {
		t.Fatal("instruction URL attributed to the wrong source was allowed")
	}
}

func TestAddOutputSchemaRejectsMixedSingleRecipeEnvelope(t *testing.T) {
	raw, err := json.Marshal(ProposalOutputSchema("add"))
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("add-output", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("add-output")
	if err != nil {
		t.Fatal(err)
	}
	procedure := map[string]any{
		"id": "single", "name": "Single-node procedure", "description": "Documented procedure",
		"manifest": map[string]any{}, "files": []any{}, "selected_source_assets": []any{},
		"questions": []any{}, "evidence": []any{}, "summary": "One procedure", "adaptations": []any{},
	}
	result := map[string]any{"procedures": []any{procedure}, "summary": "Documented procedures"}
	if err := schema.Validate(result); err != nil {
		t.Fatalf("add-only response was rejected: %v", err)
	}
	result["manifest"] = map[string]any{}
	if err := schema.Validate(result); err == nil {
		t.Fatal("provider could mix a single-recipe manifest into a multi-procedure response")
	}
}
