package assistant

import (
	"strings"
	"testing"
)

func TestCodexReadLoopRoutesInterleavedMessagesWithoutLateReplay(t *testing.T) {
	response := make(chan rpcMessage, 1)
	codex := &Codex{
		pending:     map[int64]chan rpcMessage{7: response},
		notices:     make(chan rpcMessage, 4),
		turnNotices: make(chan rpcMessage, 1),
	}
	codex.readLoop(strings.NewReader("{\"method\":\"turn/progress\",\"params\":{}}\n{\"id\":7,\"result\":{\"ok\":true}}\n{\"id\":8,\"result\":{\"stale\":true}}\n"))
	if message := <-response; message.ID == nil || *message.ID != 7 {
		t.Fatalf("response routed incorrectly: %+v", message)
	}
	if notice := <-codex.notices; notice.Method != "turn/progress" {
		t.Fatalf("notification routed incorrectly: %+v", notice)
	}
	if len(codex.pending) != 0 || len(codex.notices) != 0 {
		t.Fatalf("late response was replayed: pending=%d notices=%d", len(codex.pending), len(codex.notices))
	}
}

func TestCodexReadLoopRefusesUnexpectedServerRequests(t *testing.T) {
	pending := make(chan rpcMessage, 1)
	codex := &Codex{
		pending:     map[int64]chan rpcMessage{3: pending},
		notices:     make(chan rpcMessage, 1),
		turnNotices: make(chan rpcMessage, 1),
	}
	codex.readLoop(strings.NewReader("{\"id\":91,\"method\":\"tool/call\",\"params\":{}}\n"))
	if codex.waitErr == nil || !strings.Contains(codex.waitErr.Error(), "unexpected Codex server request") {
		t.Fatalf("unexpected request was not refused: %v", codex.waitErr)
	}
	message := <-pending
	if message.Error == nil || message.Error.Message != "Codex app-server stopped" {
		t.Fatalf("pending request did not fail closed: %+v", message)
	}
}
