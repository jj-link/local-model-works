package assistant

import "sort"

func schemaObject(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
}

func schemaArray(items any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func ProposalOutputSchema(mode string) map[string]any {
	text := map[string]any{"type": "string"}
	properties := map[string]any{
		"manifest":               map[string]any{"type": "object"},
		"files":                  schemaArray(schemaObject(map[string]any{"path": text, "content": text, "source_path": text}, "path", "content")),
		"selected_source_assets": schemaArray(schemaObject(map[string]any{"path": text, "sha256": text}, "path", "sha256")),
		"questions":              schemaArray(schemaObject(map[string]any{"id": text, "path": text, "question": text, "answer": text}, "id", "question")),
		"evidence": schemaArray(schemaObject(map[string]any{
			"path": text, "source_path": text, "sha256": text, "source_commit": text,
			"start_line": map[string]any{"type": "integer", "minimum": 1},
			"end_line":   map[string]any{"type": "integer", "minimum": 1},
		}, "path", "source_path", "sha256", "source_commit", "start_line", "end_line")),
		"summary":     text,
		"adaptations": schemaArray(schemaObject(map[string]any{"path": text, "description": text, "reason": text}, "path", "description", "reason")),
	}
	properties["files"].(map[string]any)["maxItems"] = MaxGeneratedFiles
	if mode == "add" {
		procedure := make(map[string]any, len(properties)+3)
		for key, value := range properties {
			procedure[key] = value
		}
		procedure["id"], procedure["name"], procedure["description"] = text, text, text
		procedures := schemaArray(schemaObject(procedure, "id", "name", "description", "manifest", "files", "selected_source_assets", "questions", "evidence", "summary", "adaptations"))
		procedures["minItems"], procedures["maxItems"] = 1, 32
		return schemaObject(map[string]any{"procedures": procedures, "summary": text}, "procedures", "summary")
	}
	return schemaObject(properties, "manifest", "files", "selected_source_assets", "questions", "evidence")
}

func investigationOutputSchema(allowed map[string]map[string]bool) map[string]any {
	text := map[string]any{"type": "string"}
	link := schemaObject(map[string]any{"url": text, "source_path": text, "reason": text}, "url", "source_path", "reason")
	links := schemaArray(link)
	links["maxItems"] = 0
	var choices []any
	paths := make([]string, 0, len(allowed))
	for source := range allowed {
		paths = append(paths, source)
	}
	sort.Strings(paths)
	for _, source := range paths {
		urls := make([]string, 0, len(allowed[source]))
		for url := range allowed[source] {
			urls = append(urls, url)
		}
		sort.Strings(urls)
		if len(urls) > 0 {
			choices = append(choices, schemaObject(map[string]any{
				"url":         map[string]any{"type": "string", "enum": urls},
				"source_path": map[string]any{"type": "string", "const": source},
				"reason":      text,
			}, "url", "source_path", "reason"))
		}
	}
	if len(choices) > 0 {
		links["items"] = map[string]any{"anyOf": choices}
		links["maxItems"] = 64
	}
	return schemaObject(map[string]any{
		"findings": map[string]any{"type": "string", "minLength": 1, "maxLength": 12000},
		"links":    links,
	}, "findings", "links")
}
