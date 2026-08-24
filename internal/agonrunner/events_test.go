package agonrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type captureSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *captureSink) Emit(event Event) error {
	s.mu.Lock()
	s.events = append(s.events, event)
	s.mu.Unlock()
	return nil
}

func TestObfuscatorRemovesSecretsAndReasoning(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "provider"), []byte("top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	emitter := &Emitter{Sink: sink, RunID: "run", Obfuscator: LoadObfuscator(directory)}
	if err := emitter.Emit("agent.tool.started", map[string]any{"argument": "Bearer top-secret", "reasoning": "private"}); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(sink.events)
	if bytes.Contains(encoded, []byte("top-secret")) || bytes.Contains(encoded, []byte("private")) || !bytes.Contains(encoded, []byte("[REDACTED]")) {
		t.Fatalf("redacted events = %s", encoded)
	}
}

func TestFramedSocketPreservesConcurrentEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.sock")
	capture := &captureSink{}
	server, err := StartSocketServer(path, capture)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 4
	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			sink, closer, err := newFramedSink(path)
			if err != nil {
				t.Errorf("connect: %v", err)
				return
			}
			defer closer.Close()
			for sequence := range 10 {
				if err := sink.Emit(Event{Version: 1, EventID: strings.Repeat("a", 8), RunID: "run", Type: "phase.changed", Payload: map[string]any{"writer": writer, "sequence": sequence}}); err != nil {
					t.Errorf("emit: %v", err)
				}
			}
		}(writer)
	}
	wg.Wait()
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	if len(capture.events) != writers*10 {
		t.Fatalf("events = %d", len(capture.events))
	}
	last := map[int]int{}
	for _, event := range capture.events {
		writer := int(event.Payload["writer"].(float64))
		sequence := int(event.Payload["sequence"].(float64))
		if previous, ok := last[writer]; ok && sequence != previous+1 {
			t.Fatalf("writer %d sequence %d after %d", writer, sequence, previous)
		}
		last[writer] = sequence
	}
}

func TestProviderNormalizersFilterReasoning(t *testing.T) {
	claude := newProviderNormalizer("claude")
	actions := claude.Parse([]byte(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}}`))
	if len(actions) != 1 || actions[0].Text != "hello" {
		t.Fatalf("claude actions = %#v", actions)
	}
	codex := newProviderNormalizer("codex")
	if actions := codex.Parse([]byte(`{"type":"item.completed","item":{"type":"reasoning","text":"private"}}`)); len(actions) != 0 {
		t.Fatalf("reasoning leaked: %#v", actions)
	}
	actions = codex.Parse([]byte(`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"done"}}`))
	if len(actions) != 1 || actions[0].FinalText != "done" {
		t.Fatalf("codex actions = %#v", actions)
	}
}

func writeFakeCLI(t *testing.T, name, script string) string {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestRunAgentNormalizesCodexFixture(t *testing.T) {
	directory := writeFakeCLI(t, "codex", `
out=""
previous=""
for arg in "$@"; do
  if [ "$previous" = "--output-last-message" ]; then out="$arg"; fi
  previous="$arg"
done
printf '%s\n' '{"type":"thread.started","thread_id":"session-1"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"reasoning","text":"private thought"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"public answer"}}'
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":3,"output_tokens":2}}'
printf 'public answer\n' > "$out"
`)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	workspace := t.TempDir()
	prompt := filepath.Join(workspace, "prompt.md")
	output := filepath.Join(workspace, "output.txt")
	if err := os.WriteFile(prompt, []byte("system"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	emitter := &Emitter{Sink: sink, RunID: "run", InvocationID: "invocation", Obfuscator: &Obfuscator{}}
	err := RunAgent(context.Background(), AgentOptions{
		RunID: "run", InvocationID: "invocation", Role: "paper-writer", Backend: "codex", Model: "fixture",
		WorkingDirectory: workspace, PromptPath: prompt, OutputPath: output, Task: "task",
	}, emitter)
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(output)
	if string(contents) != "public answer\n" {
		t.Fatalf("output = %q", contents)
	}
	encoded, _ := json.Marshal(sink.events)
	if bytes.Contains(encoded, []byte("private thought")) || !bytes.Contains(encoded, []byte("public answer")) || !bytes.Contains(encoded, []byte("session-1")) {
		t.Fatalf("events = %s", encoded)
	}
}

func TestRunAgentNormalizesClaudeFixtureAndRedacts(t *testing.T) {
	directory := writeFakeCLI(t, "claude", `
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-session"}'
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"token-value answer"}}}'
printf '%s\n' '{"type":"result","session_id":"claude-session","result":"token-value answer","usage":{"input_tokens":4,"output_tokens":2}}'
`)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	credentials := t.TempDir()
	if err := os.WriteFile(filepath.Join(credentials, "provider"), []byte("token-value"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LMW_CREDENTIAL_DIR", credentials)
	workspace := t.TempDir()
	prompt := filepath.Join(workspace, "prompt.md")
	output := filepath.Join(workspace, "output.txt")
	if err := os.WriteFile(prompt, []byte("system"), 0o600); err != nil {
		t.Fatal(err)
	}
	sink := &captureSink{}
	emitter := &Emitter{Sink: sink, RunID: "run", InvocationID: "invocation", Obfuscator: LoadObfuscator(credentials)}
	err := RunAgent(context.Background(), AgentOptions{
		RunID: "run", InvocationID: "invocation", Role: "paper-writer", Backend: "claude", Model: "fixture",
		SecretName: "provider", WorkingDirectory: workspace, PromptPath: prompt, OutputPath: output, Task: "task",
	}, emitter)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(sink.events)
	if bytes.Contains(encoded, []byte("token-value")) || !bytes.Contains(encoded, []byte("[REDACTED]")) || !bytes.Contains(encoded, []byte("claude-session")) {
		t.Fatalf("events = %s", encoded)
	}
	contents, _ := os.ReadFile(output)
	if !bytes.Contains(contents, []byte("token-value answer")) {
		t.Fatalf("final output was not preserved privately: %q", contents)
	}
}

func TestAdvisorNoteContract(t *testing.T) {
	note, ok := parseAdvisorNote([]byte(`{"severity":"concern","text":"Check the denominator."}`))
	if !ok || note.Severity != "concern" || note.Text == "" {
		t.Fatalf("note = %#v %v", note, ok)
	}
	if _, ok := parseAdvisorNote([]byte(`{"severity":"veto","text":"stop"}`)); ok {
		t.Fatal("accepted advisor veto")
	}
	for input, expected := range map[any]int{"off": 0, float64(1): 1, float64(3): 3, float64(5): 5, nil: 1} {
		if got := advisorBacklog(input); got != expected {
			t.Fatalf("backlog %#v = %d, want %d", input, got, expected)
		}
	}
}
