package agonrunner

import (
	"encoding/json"
	"fmt"
	"strings"
)

type normalizedAction struct {
	Type      string
	Payload   map[string]any
	Text      string
	SessionID string
	FinalText string
}

type toolReference struct {
	ID   string
	Name string
}

type providerNormalizer struct {
	backend string
	tools   map[string]toolReference
}

func newProviderNormalizer(backend string) *providerNormalizer {
	return &providerNormalizer{backend: backend, tools: map[string]toolReference{}}
}

func mapValue(value any) map[string]any {
	mapped, _ := value.(map[string]any)
	return mapped
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func usagePayload(value any) map[string]any {
	usage := mapValue(value)
	out := map[string]any{}
	for _, key := range []string{"input_tokens", "output_tokens", "cached_input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "total_tokens", "cost_usd"} {
		if number, ok := usage[key]; ok {
			out[key] = number
		}
	}
	return out
}

func (n *providerNormalizer) Parse(line []byte) []normalizedAction {
	payload := map[string]any{}
	if json.Unmarshal(line, &payload) != nil {
		return nil
	}
	if n.backend == "codex" {
		return n.parseCodex(payload)
	}
	return n.parseClaude(payload)
}

func (n *providerNormalizer) parseClaude(payload map[string]any) []normalizedAction {
	typeName := stringValue(payload["type"])
	switch typeName {
	case "system":
		if session := stringValue(payload["session_id"]); session != "" {
			return []normalizedAction{{SessionID: session}}
		}
	case "stream_event":
		event := mapValue(payload["event"])
		switch stringValue(event["type"]) {
		case "content_block_delta":
			delta := mapValue(event["delta"])
			if stringValue(delta["type"]) == "text_delta" {
				return []normalizedAction{{Text: stringValue(delta["text"])}}
			}
		case "content_block_start":
			block := mapValue(event["content_block"])
			if stringValue(block["type"]) == "tool_use" {
				toolID := stringValue(block["id"])
				name := stringValue(block["name"])
				reference := toolReference{ID: toolID, Name: name}
				n.tools[toolID] = reference
				n.tools[fmt.Sprint(event["index"])] = reference
				return []normalizedAction{{Type: "agent.tool.started", Payload: map[string]any{"tool_id": toolID, "name": name, "input": block["input"]}}}
			}
		case "content_block_stop":
			index := fmt.Sprint(event["index"])
			if reference, ok := n.tools[index]; ok {
				delete(n.tools, index)
				delete(n.tools, reference.ID)
				return []normalizedAction{{Type: "agent.tool.finished", Payload: map[string]any{"tool_id": reference.ID, "name": reference.Name, "ok": true}}}
			}
		}
	case "assistant":
		message := mapValue(payload["message"])
		var actions []normalizedAction
		if content, ok := message["content"].([]any); ok {
			for _, item := range content {
				block := mapValue(item)
				switch stringValue(block["type"]) {
				case "tool_result":
					toolID := stringValue(block["tool_use_id"])
					reference := n.tools[toolID]
					delete(n.tools, toolID)
					actions = append(actions, normalizedAction{Type: "agent.tool.finished", Payload: map[string]any{"tool_id": toolID, "name": reference.Name, "ok": block["is_error"] != true}})
				}
			}
		}
		return actions
	case "result":
		actions := []normalizedAction{}
		if usage := usagePayload(payload["usage"]); len(usage) > 0 {
			actions = append(actions, normalizedAction{Type: "agent.usage", Payload: usage})
		}
		actions = append(actions, normalizedAction{SessionID: stringValue(payload["session_id"]), FinalText: stringValue(payload["result"])})
		return actions
	}
	return nil
}

func (n *providerNormalizer) parseCodex(payload map[string]any) []normalizedAction {
	typeName := stringValue(payload["type"])
	if typeName == "thread.started" {
		return []normalizedAction{{SessionID: stringValue(payload["thread_id"])}}
	}
	if typeName == "turn.completed" {
		if usage := usagePayload(payload["usage"]); len(usage) > 0 {
			return []normalizedAction{{Type: "agent.usage", Payload: usage}}
		}
		return nil
	}
	if !strings.HasPrefix(typeName, "item.") {
		return nil
	}
	item := mapValue(payload["item"])
	itemType := stringValue(item["type"])
	if strings.Contains(itemType, "reasoning") {
		return nil
	}
	itemID := stringValue(item["id"])
	switch itemType {
	case "agent_message":
		text := stringValue(item["text"])
		return []normalizedAction{{Text: text, FinalText: text}}
	case "command_execution", "mcp_tool_call", "file_change", "web_search":
		name := itemType
		if command := stringValue(item["command"]); command != "" {
			name = command
		}
		if typeName == "item.started" {
			n.tools[itemID] = toolReference{ID: itemID, Name: name}
			return []normalizedAction{{Type: "agent.tool.started", Payload: map[string]any{"tool_id": itemID, "name": name}}}
		}
		if typeName == "item.completed" {
			delete(n.tools, itemID)
			return []normalizedAction{{Type: "agent.tool.finished", Payload: map[string]any{"tool_id": itemID, "name": name, "ok": stringValue(item["status"]) != "failed"}}}
		}
	}
	return nil
}
