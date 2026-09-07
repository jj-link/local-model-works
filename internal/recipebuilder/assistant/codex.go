package assistant

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const maxCodexLine = 8 << 20

type rpcMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type codexLimitedWriter struct {
	data []byte
}

func (w *codexLimitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 64<<10 - len(w.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.data = append(w.data, p...)
	}
	return n, nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type Codex struct {
	binary    string
	root      string
	lifecycle context.Context

	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	nextID      int64
	pending     map[int64]chan rpcMessage
	notices     chan rpcMessage
	stateMu     sync.Mutex
	logins      map[string]LoginStatus
	activeLogin string
	noticeOnce  sync.Once
	turnMu      sync.Mutex
	turnNotices chan rpcMessage
	waitErr     error
}

func NewCodex(lifecycle context.Context, binary, stateRoot string) *Codex {
	if strings.TrimSpace(binary) == "" {
		binary = "codex"
	}
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	root := filepath.Join(stateRoot, "assistant")
	if absolute, err := filepath.Abs(root); err == nil {
		root = absolute
	}
	return &Codex{
		binary: binary, root: root, lifecycle: lifecycle, notices: make(chan rpcMessage, 64),
		turnNotices: make(chan rpcMessage, 128),
	}
}
func (c *Codex) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd != nil && c.cmd.ProcessState == nil {
		return nil
	}
	if _, err := exec.LookPath(c.binary); err != nil {
		return &Error{Code: "assistant.codex_missing", Message: "native Codex binary is unavailable", Retryable: false}
	}
	codexHome := filepath.Join(c.root, "codex")
	workspace := filepath.Join(c.root, "workspace")
	for _, directory := range []string{codexHome, workspace} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return err
		}
	}
	processCtx, cancel := context.WithCancel(c.lifecycle)
	args := []string{
		"-c", `approval_policy="never"`,
		"-c", `sandbox_mode="read-only"`,
		"-c", `web_search="disabled"`,
		"-c", `features.shell_tool=false`,
		"-c", `features.unified_exec=false`,
		"-c", `features.shell_snapshot=false`,
		"-c", `features.apps=false`,
		"-c", `features.multi_agent=false`,
		"app-server", "--listen", "stdio://",
	}
	cmd := exec.CommandContext(processCtx, c.binary, args...)
	cmd.Dir = workspace
	cmd.Env = codexEnvironment(codexHome)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return err
	}
	stderr := &codexLimitedWriter{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return &Error{Code: "assistant.codex_unavailable", Message: "native Codex app-server could not start", Retryable: true}
	}
	c.ctx, c.cancel, c.cmd, c.stdin = processCtx, cancel, cmd, stdin
	c.nextID = 0
	c.pending = make(map[int64]chan rpcMessage)
	c.waitErr = nil
	go c.readLoop(stdout)
	c.noticeOnce.Do(func() { go c.noticeLoop() })
	go c.waitLoop(cmd)
	c.mu.Unlock()
	var initialized json.RawMessage
	err = c.request(processCtx, "initialize", map[string]any{"clientInfo": map[string]string{
		"name": "local_model_works", "title": "Local Model Works", "version": "0.1.0",
	}}, &initialized)
	if err == nil {
		err = c.notify("initialized", map[string]any{})
	}
	c.mu.Lock()
	if err != nil {
		return &Error{Code: "assistant.codex_unsupported", Message: "Codex app-server handshake or required restrictions failed: " + strings.TrimSpace(string(stderr.data)), Retryable: false}
	}
	return nil
}

func (c *Codex) Close() error {
	c.mu.Lock()
	cancel, cmd := c.cancel, c.cmd
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	return nil
}

func (c *Codex) request(ctx context.Context, method string, params any, out any) error {
	c.mu.Lock()
	if c.cmd == nil || c.cmd.ProcessState != nil {
		c.mu.Unlock()
		return &Error{Code: "assistant.codex_unavailable", Message: "Codex app-server is not running", Retryable: true}
	}
	c.nextID++
	id := c.nextID
	response := make(chan rpcMessage, 1)
	c.pending[id] = response
	payload, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err == nil {
		_, err = c.stdin.Write(append(payload, '\n'))
	}
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return &Error{Code: "assistant.codex_unavailable", Message: "Codex app-server request failed", Retryable: true}
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case message := <-response:
		if message.Error != nil {
			return &Error{Code: "assistant.codex_request_failed", Message: message.Error.Message, Retryable: false}
		}
		if out != nil && len(message.Result) != 0 {
			return json.Unmarshal(message.Result, out)
		}
		return nil
	}
}

func (c *Codex) notify(method string, params any) error {
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return errors.New("Codex app-server is not running")
	}
	_, err = c.stdin.Write(append(payload, '\n'))
	return err
}

func (c *Codex) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxCodexLine)
	for scanner.Scan() {
		var message rpcMessage
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			c.failProcess(errors.New("invalid Codex JSONL response"))
			return
		}
		if message.Method != "" && message.ID != nil {
			c.failProcess(fmt.Errorf("unexpected Codex server request %s", message.Method))
			return
		}
		if message.ID != nil {
			c.mu.Lock()
			response := c.pending[*message.ID]
			delete(c.pending, *message.ID)
			c.mu.Unlock()
			if response != nil {
				response <- message
			}
			continue
		}
		select {
		case c.notices <- message:
		default:
			c.failProcess(errors.New("Codex notification queue exceeded"))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.failProcess(err)
	}
}

func (c *Codex) waitLoop(cmd *exec.Cmd) { c.failProcess(cmd.Wait()) }

func (c *Codex) failProcess(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.waitErr == nil {
		c.waitErr = err
	}
	for id, response := range c.pending {
		response <- rpcMessage{ID: &id, Error: &rpcError{Code: -1, Message: "Codex app-server stopped"}}
		delete(c.pending, id)
	}
	select {
	case c.turnNotices <- rpcMessage{Method: "process/stopped"}:
	default:
	}
}

func codexEnvironment(home string) []string {
	blocked := map[string]bool{"CODEX_HOME": true, "OPENAI_API_KEY": true, "CODEX_API_KEY": true, "ANTHROPIC_API_KEY": true}
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if blocked[strings.ToUpper(key)] || strings.HasPrefix(strings.ToUpper(key), "MCP_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CODEX_HOME="+home, "LMW_CODEX_RESTRICTED="+strconv.FormatBool(true))
}
