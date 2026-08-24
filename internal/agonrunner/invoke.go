package agonrunner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var credentialFilename = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func credentialValue(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	directory := os.Getenv("LMW_CREDENTIAL_DIR")
	filename := credentialFilename.ReplaceAllString(name, "_")
	if directory == "" || filename == "" {
		return "", errors.New("autoresearch.provider_credential_missing")
	}
	value, err := os.ReadFile(filepath.Join(directory, filename))
	if err != nil || len(value) == 0 {
		return "", errors.New("autoresearch.provider_credential_missing")
	}
	return string(value), nil
}

func providerCommand(ctx context.Context, options AgentOptions) (*exec.Cmd, error) {
	if options.Role == "" || options.Backend == "" || options.Model == "" || options.WorkingDirectory == "" || options.PromptPath == "" || options.OutputPath == "" {
		return nil, errors.New("agent requires role, backend, model, working-directory, prompt-path, and output-path")
	}
	if _, err := os.Stat(options.PromptPath); err != nil {
		return nil, err
	}
	credential, err := credentialValue(options.SecretName)
	if err != nil {
		return nil, err
	}
	var command *exec.Cmd
	switch options.Backend {
	case "claude", "claude-ds":
		binary := options.Backend
		arguments := []string{"--plugin-dir", os.Getenv("CLAUDE_PLUGIN_ROOT"), "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--model", options.Model, "--append-system-prompt-file", options.PromptPath}
		if options.Advisor {
			arguments = append(arguments, "--allowedTools", "Read,Grep,Glob")
		} else {
			arguments = append(arguments, "--dangerously-skip-permissions")
		}
		if options.ResumeSessionID != "" {
			arguments = append(arguments, "--resume", options.ResumeSessionID)
		}
		arguments = append(arguments, "-p", options.Task)
		command = exec.CommandContext(ctx, binary, arguments...)
	case "codex":
		permissions := []string{"--dangerously-bypass-approvals-and-sandbox"}
		if options.Advisor {
			permissions = []string{"--sandbox", "read-only", "--ask-for-approval", "never"}
		}
		if options.ResumeSessionID != "" {
			arguments := append([]string{"exec", "resume"}, permissions...)
			arguments = append(arguments, "--json", "-m", options.Model, "--output-last-message", options.OutputPath, options.ResumeSessionID, options.Task)
			command = exec.CommandContext(ctx, "codex", arguments...)
		} else {
			arguments := append([]string{"exec"}, permissions...)
			arguments = append(arguments, "--json", "-m", options.Model, "--output-last-message", options.OutputPath, options.Task)
			command = exec.CommandContext(ctx, "codex", arguments...)
		}
	default:
		return nil, fmt.Errorf("unsupported backend %q", options.Backend)
	}
	command.Dir = options.WorkingDirectory
	command.Env = append([]string{}, os.Environ()...)
	command.Env = append(command.Env, "LMW_RUN_ID="+options.RunID, "LMW_PARENT_INVOCATION_ID="+options.InvocationID)
	if options.BaseURL != "" {
		switch options.Backend {
		case "claude", "claude-ds":
			command.Env = append(command.Env, "ANTHROPIC_BASE_URL="+options.BaseURL)
		case "codex":
			command.Env = append(command.Env, "OPENAI_BASE_URL="+strings.TrimRight(options.BaseURL, "/"))
		}
	}
	if credential != "" {
		switch options.Backend {
		case "claude":
			command.Env = append(command.Env, "ANTHROPIC_API_KEY="+credential)
		case "claude-ds":
			command.Env = append(command.Env, "ANTHROPIC_AUTH_TOKEN="+credential)
		case "codex":
			command.Env = append(command.Env, "OPENAI_API_KEY="+credential)
		}
	} else if options.Backend == "codex" && options.BaseURL != "" {
		command.Env = append(command.Env, "OPENAI_API_KEY=lmw-local")
	}
	return command, nil
}

type scanResult struct {
	line []byte
	err  error
}

func consumeProviderStream(stdout io.Reader, normalizer *providerNormalizer, emitter *Emitter, advisor bool) (string, string, error) {
	lines := make(chan scanResult, 16)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), 8<<20)
		for scanner.Scan() {
			lines <- scanResult{line: append([]byte(nil), scanner.Bytes()...)}
		}
		if err := scanner.Err(); err != nil {
			lines <- scanResult{err: err}
		}
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var text strings.Builder
	lastText, sessionID := "", ""
	flush := func() error {
		if text.Len() == 0 {
			return nil
		}
		value := text.String()
		text.Reset()
		if advisor {
			return nil
		}
		return emitter.Emit("agent.text.delta", map[string]any{"text": value})
	}
	for {
		select {
		case result, ok := <-lines:
			if !ok {
				return lastText, sessionID, flush()
			}
			if result.err != nil {
				return lastText, sessionID, result.err
			}
			for _, action := range normalizer.Parse(result.line) {
				if action.SessionID != "" {
					sessionID = action.SessionID
				}
				if action.FinalText != "" {
					lastText = action.FinalText
				}
				if action.Text != "" {
					text.WriteString(action.Text)
					if text.Len() >= 256 {
						if err := flush(); err != nil {
							return lastText, sessionID, err
						}
					}
				}
				if action.Type != "" {
					if advisor {
						action.Payload["advisor"] = true
					}
					if err := emitter.Emit(action.Type, action.Payload); err != nil {
						return lastText, sessionID, err
					}
				}
			}
		case <-ticker.C:
			if err := flush(); err != nil {
				return lastText, sessionID, err
			}
		}
	}
}

func gitHead(directory string) string {
	output, err := exec.Command("git", "-C", directory, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func gitChangedPaths(directory, before, after string) []string {
	if before == "" || after == "" || before == after {
		return nil
	}
	output, err := exec.Command("git", "-C", directory, "diff", "--name-only", before, after).Output()
	if err != nil {
		return nil
	}
	var paths []string
	for _, path := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

// RunAgent launches one provider CLI and emits only normalized, redacted public events.
func RunAgent(ctx context.Context, options AgentOptions, emitter *Emitter) error {
	command, err := providerCommand(ctx, options)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(options.OutputPath), 0o700); err != nil {
		return err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		return err
	}
	startedType, finishedType := "agent.started", "agent.finished"
	if options.Advisor {
		startedType, finishedType = "advisor.started", "advisor.finished"
	}
	if err := emitter.Emit(startedType, map[string]any{"role": options.Role, "backend": options.Backend, "model": options.Model, "resume_session_id": options.ResumeSessionID}); err != nil {
		return err
	}
	beforeHead := gitHead(options.WorkingDirectory)
	if err := command.Start(); err != nil {
		return err
	}
	stderrDone := make(chan []byte, 1)
	go func() {
		contents, _ := io.ReadAll(io.LimitReader(stderr, 4<<20))
		stderrDone <- contents
	}()
	lastText, sessionID, streamErr := consumeProviderStream(stdout, newProviderNormalizer(options.Backend), emitter, options.Advisor)
	waitErr := command.Wait()
	stderrText := <-stderrDone
	if streamErr != nil {
		return streamErr
	}
	if waitErr != nil {
		message := strings.TrimSpace(string(stderrText))
		_ = emitter.Emit("error", map[string]any{"code": "autoresearch.provider_failed", "message": message, "backend": options.Backend, "model": options.Model})
		return fmt.Errorf("%s invocation failed: %w: %s", options.Backend, waitErr, message)
	}
	if options.Backend != "codex" {
		if lastText == "" {
			lastText = strings.TrimSpace(string(stderrText))
		}
		if err := os.WriteFile(options.OutputPath, []byte(lastText+"\n"), 0o600); err != nil {
			return err
		}
	}
	if info, err := os.Stat(options.OutputPath); err != nil || info.Size() == 0 {
		return errors.New("autoresearch.agent_output_missing")
	}
	afterHead := gitHead(options.WorkingDirectory)
	for _, path := range gitChangedPaths(options.WorkingDirectory, beforeHead, afterHead) {
		_ = emitter.Emit("artifact.changed", map[string]any{"path": path, "commit": afterHead})
	}
	return emitter.Emit(finishedType, map[string]any{"role": options.Role, "backend": options.Backend, "model": options.Model, "session_id": sessionID, "ok": true})
}
