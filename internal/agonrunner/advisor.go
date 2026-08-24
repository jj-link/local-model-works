package agonrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jj-link/local-model-works/internal/id"
)

type advisorNote struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
}

func advisorBacklog(value any) int {
	switch typed := value.(type) {
	case string:
		if typed == "off" {
			return 0
		}
	case float64:
		if typed == 1 || typed == 3 || typed == 5 {
			return int(typed)
		}
	case int:
		if typed == 1 || typed == 3 || typed == 5 {
			return typed
		}
	}
	return 1
}

func parseAdvisorNote(contents []byte) (advisorNote, bool) {
	note := advisorNote{}
	if json.Unmarshal(contents, &note) == nil {
		note.Severity = strings.ToLower(strings.TrimSpace(note.Severity))
		note.Text = strings.TrimSpace(note.Text)
		if (note.Severity == "nit" || note.Severity == "concern" || note.Severity == "blocker") && note.Text != "" {
			return note, true
		}
	}
	return advisorNote{}, false
}

// AdvisorWatcher reviews secret-obfuscated primary deltas without mutation authority.
type AdvisorWatcher struct {
	runID      string
	parentID   string
	role       string
	provider   providerConfig
	workspace  string
	scratch    string
	advicePath string
	promptPath string
	sink       EventSink
	obfuscator *Obfuscator
	updates    chan string
	done       chan struct{}
	seenMu     sync.Mutex
	seen       map[string]struct{}
}

func NewAdvisorWatcher(runID, parentID, role, workspace, scratch string, config advisorConfig, sink EventSink, obfuscator *Obfuscator) (*AdvisorWatcher, error) {
	if !config.Enabled {
		return nil, nil
	}
	backlog := advisorBacklog(config.Backlog)
	if backlog == 0 {
		return nil, nil
	}
	provider, err := normalizeProvider(config.Provider)
	if err != nil {
		return nil, err
	}
	advisorDir := filepath.Join(scratch, "advisor")
	if err := os.MkdirAll(advisorDir, 0o700); err != nil {
		return nil, err
	}
	promptPath := filepath.Join(advisorDir, "advisor-prompt.md")
	prompt := `You are an advice-only continuous reviewer. You may inspect the artifact workspace only with read, grep, and glob. Never execute the primary task, edit files, run mutation tools, change state, approve, veto, or expose private reasoning. Review only the new primary transcript delta. Return exactly one JSON object {"severity":"nit|concern|blocker","text":"concise actionable advice"}, or {"severity":"","text":""} when no new advice is warranted. Do not repeat prior advice.`
	if err := os.WriteFile(promptPath, []byte(prompt+"\n"), 0o600); err != nil {
		return nil, err
	}
	watcher := &AdvisorWatcher{
		runID: runID, parentID: parentID, role: role, provider: provider,
		workspace: workspace, scratch: advisorDir, advicePath: filepath.Join(advisorDir, "accepted-notes.md"),
		promptPath: promptPath, sink: sink, obfuscator: obfuscator,
		updates: make(chan string, backlog), done: make(chan struct{}), seen: map[string]struct{}{},
	}
	go watcher.run()
	return watcher, nil
}

func (w *AdvisorWatcher) Push(delta string) {
	if w == nil || strings.TrimSpace(delta) == "" {
		return
	}
	delta = w.obfuscator.String(delta)
	select {
	case w.updates <- delta:
	default:
		select {
		case <-w.updates:
		default:
		}
		select {
		case w.updates <- delta:
		default:
		}
	}
}

func (w *AdvisorWatcher) run() {
	defer close(w.done)
	for delta := range w.updates {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		invocationID, err := id.New()
		if err != nil {
			cancel()
			continue
		}
		output := filepath.Join(w.scratch, invocationID+".txt")
		options := AgentOptions{
			RunID: w.runID, InvocationID: invocationID, ParentInvocation: w.parentID,
			Role: w.role + "-advisor", Backend: w.provider.Backend, Model: w.provider.Model,
			BaseURL: w.provider.BaseURL, SecretName: w.provider.SecretName,
			WorkingDirectory: w.workspace, PromptPath: w.promptPath, OutputPath: output,
			Task: "New secret-obfuscated primary transcript delta:\n\n" + delta, Advisor: true,
		}
		emitter := &Emitter{
			Sink: w.sink, RunID: w.runID, InvocationID: invocationID, ParentInvocationID: w.parentID,
			NodeID: "advisor:" + w.role, Obfuscator: w.obfuscator,
		}
		err = RunAgent(ctx, options, emitter)
		cancel()
		if err != nil {
			continue
		}
		contents, err := os.ReadFile(output)
		_ = os.Remove(output)
		if err != nil {
			continue
		}
		note, ok := parseAdvisorNote(contents)
		if !ok {
			continue
		}
		key := note.Severity + "\x00" + strings.ToLower(strings.Join(strings.Fields(note.Text), " "))
		w.seenMu.Lock()
		_, duplicate := w.seen[key]
		if !duplicate {
			w.seen[key] = struct{}{}
		}
		w.seenMu.Unlock()
		if duplicate {
			continue
		}
		line := fmt.Sprintf("- [%s] %s\n", note.Severity, note.Text)
		file, err := os.OpenFile(w.advicePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString(line)
			_ = file.Close()
		}
		_ = emitter.Emit("advisor.note", map[string]any{"severity": note.Severity, "text": note.Text, "role": w.role})
	}
}

func (w *AdvisorWatcher) Close() {
	if w == nil {
		return
	}
	close(w.updates)
	select {
	case <-w.done:
	case <-time.After(30 * time.Second):
	}
}

func (w *AdvisorWatcher) AdvicePath() string {
	if w == nil {
		return ""
	}
	return w.advicePath
}

type watchingSink struct {
	base    EventSink
	watcher *AdvisorWatcher
	parent  string
}

func (s *watchingSink) Emit(event Event) error {
	if event.Type == "agent.text.delta" && event.InvocationID == s.parent {
		if text, ok := event.Payload["text"].(string); ok {
			s.watcher.Push(text)
		}
	}
	return s.base.Emit(event)
}
