// Package agonrunner supervises Agon dispatchers and nested model roles inside the isolated worker.
package agonrunner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Main executes one lmw-agon-runner subcommand.
func Main(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: lmw-agon-runner <preflight|supervise|agent>")
	}
	switch args[0] {
	case "preflight":
		return runPreflight(args[1:])
	case "supervise":
		return runSupervise(ctx, args[1:])
	case "agent":
		return runAgentCommand(ctx, args[1:])
	default:
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func runPreflight(args []string) error {
	flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
	sentinel := flags.String("sentinel", "", "shared-root sentinel path")
	expected := flags.String("expect", "", "expected sentinel contents")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *sentinel == "" && *expected == "" {
		return nil
	}
	contents, err := os.ReadFile(*sentinel)
	if err != nil || string(contents) != *expected {
		return errors.New("autoresearch.runner_not_colocated")
	}
	return nil
}

type projectConfig struct {
	Schema    int                       `json:"schema"`
	ProjectID string                    `json:"project_id"`
	RunID     string                    `json:"run_id"`
	Factory   string                    `json:"factory"`
	Project   map[string]any            `json:"project"`
	Input     map[string]any            `json:"input"`
	Roles     map[string]providerConfig `json:"roles"`
}

type providerConfig struct {
	Source   string `json:"source"`
	Backend  string `json:"backend"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
	Endpoint string `json:"endpoint"`
}

func loadProjectConfig(path string) (projectConfig, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return projectConfig{}, err
	}
	config := projectConfig{}
	if err := json.Unmarshal(contents, &config); err != nil {
		return projectConfig{}, err
	}
	if nested, ok := config.Project["roles"].(map[string]any); ok {
		encoded, _ := json.Marshal(nested)
		_ = json.Unmarshal(encoded, &config.Roles)
	}
	return config, nil
}

func commandForFactory(factory string) (role, prompt, task string, err error) {
	root := os.Getenv("CLAUDE_PLUGIN_ROOT")
	if root == "" {
		root = "/opt/agon"
	}
	switch factory {
	case "idea":
		return "idea-intake-dispatcher", filepath.Join(root, "commands", "idea-intake.md"), "Execute bounded idea intake from /project/.lmw/config.json and stop after candidates are written.", nil
	case "proposal":
		return "proposal-dispatcher", filepath.Join(root, "commands", "proposal-tick.md"), "Execute proposal-tick for selected project ideas.", nil
	case "deep_lit":
		return "deep-lit-dispatcher", filepath.Join(root, "commands", "deep-lit-tick.md"), "Execute deep-lit-tick to saturation for the current project scope.", nil
	case "experiment":
		return "experiment-dispatcher", filepath.Join(root, "commands", "experiment-tick.md"), "Execute experiment-tick for the current project workspace.", nil
	case "paper", "paper-edit":
		return "paper-dispatcher", filepath.Join(root, "commands", "paper-tick.md"), "Execute paper-tick for the current project. For paper-edit, apply the human writer request from /project/.lmw/config.json.", nil
	case "paper-compile":
		return "paper-compiler", filepath.Join(root, "skills_aris", "paper-compile.md"), "Compile the current paper deterministically and update PAPER_STATE.md build paths without changing scientific content.", nil
	default:
		return "", "", "", fmt.Errorf("unsupported factory %q", factory)
	}
}

func selectProvider(config projectConfig, role string) (providerConfig, error) {
	if provider, ok := config.Roles[role]; ok && provider.Backend != "" && provider.Model != "" {
		return provider, nil
	}
	if provider, ok := config.Roles["default"]; ok && provider.Backend != "" && provider.Model != "" {
		return provider, nil
	}
	return providerConfig{}, errors.New("autoresearch.provider_unavailable")
}

func runSupervise(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("supervise", flag.ContinueOnError)
	runID := flags.String("run-id", "", "durable run id")
	projectRoot := flags.String("project-root", "/project", "project root")
	scratch := flags.String("scratch", "/scratch", "run scratch")
	factory := flags.String("factory", "", "factory name")
	configPath := flags.String("config", "/project/.lmw/config.json", "project configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *runID == "" || *factory == "" {
		return errors.New("supervise requires --run-id and --factory")
	}
	if err := os.MkdirAll(*scratch, 0o700); err != nil {
		return err
	}
	config, err := loadProjectConfig(*configPath)
	if err != nil {
		return err
	}
	role, prompt, task, err := commandForFactory(*factory)
	if err != nil {
		return err
	}
	provider, err := selectProvider(config, role)
	if err != nil {
		return err
	}
	output := filepath.Join(*scratch, "dispatcher-output.txt")
	options := AgentOptions{
		RunID: *runID, Role: role, Backend: provider.Backend, Model: provider.Model,
		BaseURL: provider.BaseURL, WorkingDirectory: filepath.Join(*projectRoot, "artifacts"),
		PromptPath: prompt, OutputPath: output, Task: task,
	}
	if err := RunAgent(ctx, options, os.Stdout); err != nil {
		return err
	}
	contents, err := os.ReadFile(output)
	if err != nil || len(strings.TrimSpace(string(contents))) == 0 {
		return errors.New("autoresearch.dispatcher_output_missing")
	}
	return nil
}

// AgentOptions is one primary or advisor model invocation.
type AgentOptions struct {
	RunID            string
	InvocationID     string
	ParentInvocation string
	Role             string
	Backend          string
	Model            string
	BaseURL          string
	WorkingDirectory string
	PromptPath       string
	OutputPath       string
	ResumeSessionID  string
	Task             string
	Advisor          bool
}

func runAgentCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	options := AgentOptions{}
	flags.StringVar(&options.RunID, "run-id", os.Getenv("LMW_RUN_ID"), "durable run id")
	flags.StringVar(&options.InvocationID, "invocation-id", "", "invocation id")
	flags.StringVar(&options.ParentInvocation, "parent-invocation-id", "", "parent invocation id")
	flags.StringVar(&options.Role, "role", "", "Agon role")
	flags.StringVar(&options.Backend, "backend", "", "claude, codex, or claude-ds")
	flags.StringVar(&options.Model, "model", "", "provider model")
	flags.StringVar(&options.BaseURL, "base-url", "", "provider base URL")
	flags.StringVar(&options.WorkingDirectory, "working-directory", "", "workspace directory")
	flags.StringVar(&options.PromptPath, "prompt-path", "", "role prompt file")
	flags.StringVar(&options.OutputPath, "output-path", "", "final response path")
	flags.StringVar(&options.ResumeSessionID, "resume-session-id", "", "provider session id")
	flags.StringVar(&options.Task, "task", "", "task prompt")
	flags.BoolVar(&options.Advisor, "advisor", false, "advisor invocation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	return RunAgent(ctx, options, os.Stdout)
}

func providerCommand(ctx context.Context, options AgentOptions) (*exec.Cmd, error) {
	if options.Role == "" || options.Backend == "" || options.Model == "" || options.WorkingDirectory == "" || options.PromptPath == "" || options.OutputPath == "" {
		return nil, errors.New("agent requires role, backend, model, working-directory, prompt-path, and output-path")
	}
	if _, err := os.Stat(options.PromptPath); err != nil {
		return nil, err
	}
	var command *exec.Cmd
	switch options.Backend {
	case "claude", "claude-ds":
		binary := options.Backend
		arguments := []string{"--dangerously-skip-permissions", "--plugin-dir", os.Getenv("CLAUDE_PLUGIN_ROOT"), "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--model", options.Model, "--append-system-prompt-file", options.PromptPath}
		if options.ResumeSessionID != "" {
			arguments = append(arguments, "--resume", options.ResumeSessionID)
		}
		arguments = append(arguments, "-p", options.Task)
		command = exec.CommandContext(ctx, binary, arguments...)
	case "codex":
		arguments := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--json", "-m", options.Model, "--output-last-message", options.OutputPath}
		if options.ResumeSessionID != "" {
			arguments = append([]string{"exec", "resume", "--dangerously-bypass-approvals-and-sandbox", "--json", "-m", options.Model, "--output-last-message", options.OutputPath, options.ResumeSessionID}, options.Task)
		} else {
			arguments = append(arguments, options.Task)
		}
		command = exec.CommandContext(ctx, "codex", arguments...)
	default:
		return nil, fmt.Errorf("unsupported backend %q", options.Backend)
	}
	command.Dir = options.WorkingDirectory
	command.Env = append([]string{}, os.Environ()...)
	if options.BaseURL != "" && (options.Backend == "claude" || options.Backend == "claude-ds") {
		command.Env = append(command.Env, "ANTHROPIC_BASE_URL="+options.BaseURL)
	}
	return command, nil
}

// RunAgent launches one provider CLI and preserves its streamed output and final response.
func RunAgent(ctx context.Context, options AgentOptions, events io.Writer) error {
	command, err := providerCommand(ctx, options)
	if err != nil {
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
	if err := command.Start(); err != nil {
		return err
	}
	stderrDone := make(chan []byte, 1)
	go func() {
		contents, _ := io.ReadAll(io.LimitReader(stderr, 4<<20))
		stderrDone <- contents
	}()
	lastText := ""
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		_, _ = events.Write(append(line, '\n'))
		var payload map[string]any
		if json.Unmarshal(line, &payload) == nil {
			for _, key := range []string{"result", "text", "last_message"} {
				if value, ok := payload[key].(string); ok && value != "" {
					lastText = value
				}
			}
		}
	}
	scanErr := scanner.Err()
	waitErr := command.Wait()
	stderrText := <-stderrDone
	if scanErr != nil {
		return scanErr
	}
	if waitErr != nil {
		return fmt.Errorf("%s invocation failed: %w: %s", options.Backend, waitErr, strings.TrimSpace(string(stderrText)))
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
	return nil
}

