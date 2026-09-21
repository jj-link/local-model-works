package assistant

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateResultRejectsUnsafeAndOversizedFiles(t *testing.T) {
	valid := Result{Manifest: json.RawMessage(`{}`), Files: []File{{Path: "scripts/start.sh", Content: "echo safe\n"}}}
	if err := ValidateResult(valid); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		file File
		code string
	}{
		{name: "traversal", file: File{Path: "../secret", Content: "x"}, code: "assistant.output_path_invalid"},
		{name: "reserved", file: File{Path: ".codex/config", Content: "x"}, code: "assistant.output_path_forbidden"},
		{name: "oversized", file: File{Path: "large.txt", Content: strings.Repeat("x", MaxGeneratedFileBytes+1)}, code: "assistant.output_too_large"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResult(Result{Manifest: json.RawMessage(`{}`), Files: []File{test.file}})
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != test.code {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestValidateResultRejectsDuplicatePathsAndMalformedManifest(t *testing.T) {
	if err := ValidateResult(Result{Manifest: json.RawMessage(`{`)}); err == nil {
		t.Fatal("malformed manifest was accepted")
	}
	err := ValidateResult(Result{Manifest: json.RawMessage(`{}`), Files: []File{
		{Path: "same.txt", Content: "one"}, {Path: "same.txt", Content: "two"},
	}})
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != "assistant.output_path_duplicate" {
		t.Fatalf("error = %v", err)
	}
}

func TestSourceOwnedProposalsRejectManagedComposition(t *testing.T) {
	manifest := json.RawMessage(`{"artifacts":[],"workloads":[{"upstream":{"start":["./start.sh"],"stop":["./stop.sh"],"containers":["upstream"]}}]}`)
	tests := []struct {
		name   string
		result Result
	}{
		{"generated launcher", Result{Manifest: manifest, Files: []File{{Path: "start.sh", Content: "docker run substitute"}}}},
		{"selected source patch", Result{Manifest: manifest, SelectedSourceAssets: []AssetSelection{{Path: "patch.sh", SHA256: "hash"}}}},
		{"model artifact", Result{Manifest: json.RawMessage(`{"artifacts":[{"name":"replacement"}],"workloads":[{"upstream":{}}]}`)}},
		{"selected serving image", Result{Manifest: json.RawMessage(`{"workloads":[{"upstream":{},"image":{"reference":"replacement:latest"}}]}`)}},
		{"managed companion workload", Result{Manifest: json.RawMessage(`{"workloads":[{"upstream":{}},{"command":["docker","run"]}]}`)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var typed *Error
			if err := ValidateResult(test.result); !errors.As(err, &typed) || typed.Code != "assistant.upstream_composition" {
				t.Fatalf("unsafe source-owned composition accepted: %v", err)
			}
		})
	}
	nested := Result{Procedures: []Procedure{{ID: "source", Name: "Source", Description: "Documented source", Manifest: manifest, SelectedSourceAssets: []AssetSelection{{Path: "patch.sh", SHA256: "hash"}}}}}
	if err := ValidateResult(nested); err == nil {
		t.Fatal("nested procedure bypassed source ownership")
	}
	if err := ValidateResult(Result{Manifest: manifest}); err != nil {
		t.Fatalf("unaltered source execution rejected: %v", err)
	}
}
