package backend

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/deploy"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/moduleapi"
	"github.com/jj-link/local-model-works/internal/runs"
)

func deploymentLogFixture(t *testing.T) (http.Handler, *runs.Service, string) {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	queries := db.New(database)
	bus := events.NewEventBus(queries)
	runService := runs.New(database, queries, bus, t.TempDir())
	runID, err := runService.Create(ctx, "serving", "serve", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := runService.SetState(ctx, runID, runs.Running, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := runService.SetState(ctx, runID, runs.Verifying, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := runService.SetState(ctx, runID, runs.Succeeded, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateRecipe(ctx, db.CreateRecipeParams{
		Digest: "sha256:logs", Name: "logs", Version: "1.0.0", Source: "{}", Manifest: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := queries.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: "dep", RecipeDigest: "sha256:logs", Parameters: "{}", Placement: "{}",
		RunID: sql.NullString{String: runID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	module := &Module{env: &moduleapi.Env{
		Deploy: deploy.New(database, queries, bus, runService, nil), Runs: runService,
	}}
	return Handler(module), runService, runID
}

type deploymentLogFrame struct {
	cursor string
	event  string
	data   string
}

func deploymentLogRequest(t *testing.T, handler http.Handler, cursor string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, "/deployments/dep/logs", nil).WithContext(ctx)
	if cursor != "" {
		request.Header.Set("Last-Event-ID", cursor)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func deploymentLogFrames(t *testing.T, response *httptest.ResponseRecorder) []deploymentLogFrame {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var frames []deploymentLogFrame
	for _, raw := range strings.Split(response.Body.String(), "\n\n") {
		var frame deploymentLogFrame
		for line := range strings.SplitSeq(raw, "\n") {
			if value, ok := strings.CutPrefix(line, "id: "); ok {
				frame.cursor = value
			}
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				frame.event = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				frame.data = value
			}
		}
		if frame.data != "" {
			frames = append(frames, frame)
		}
	}
	return frames
}

func deploymentLogOutput(t *testing.T, frames []deploymentLogFrame) (map[string]string, bool, bool) {
	t.Helper()
	output := map[string]string{"stdout": "", "stderr": ""}
	var snapshot, ended bool
	for _, frame := range frames {
		switch frame.event {
		case "end":
			ended = true
		case "snapshot":
			snapshot = true
			if err := json.Unmarshal([]byte(frame.data), &output); err != nil {
				t.Fatal(err)
			}
		default:
			var chunk struct {
				Stream string `json:"stream"`
				Data   string `json:"data"`
			}
			if err := json.Unmarshal([]byte(frame.data), &chunk); err != nil {
				t.Fatal(err)
			}
			if chunk.Stream != "stdout" && chunk.Stream != "stderr" {
				t.Fatalf("invalid stream: %q", chunk.Stream)
			}
			output[chunk.Stream] += chunk.Data
		}
	}
	return output, snapshot, ended
}

func TestDeploymentLogsSnapshotAndExactResume(t *testing.T) {
	handler, runService, runID := deploymentLogFixture(t)
	var lines []string
	for i := range 250 {
		lines = append(lines, fmt.Sprintf("line %03d\n", i))
	}
	history := strings.Join(lines, "")
	stderr := "llama loading\nllama ready\n"
	for stream, text := range map[string]string{"stdout": history, "stderr": stderr} {
		if err := runService.AppendLog(runID, "dep", 0, stream, []byte(text)); err != nil {
			t.Fatal(err)
		}
		runService.MarkLogEnd(runID, "dep", 0, stream, uint64(len(text)))
	}
	finalCursor := fmt.Sprintf("%d:%d", len(history), len(stderr))
	for _, test := range []struct {
		name, cursor, stdout, stderr string
		snapshot                     bool
	}{
		{name: "new connection", stdout: strings.Join(lines[50:], ""), stderr: stderr, snapshot: true},
		{name: "explicit zero", cursor: "0", stdout: history, stderr: stderr},
		{name: "reconnect", cursor: fmt.Sprintf("%d:%d", len(strings.Join(lines[:240], "")), len("llama loading\n")), stdout: strings.Join(lines[240:], ""), stderr: "llama ready\n"},
		{name: "fully consumed", cursor: finalCursor},
	} {
		t.Run(test.name, func(t *testing.T) {
			frames := deploymentLogFrames(t, deploymentLogRequest(t, handler, test.cursor))
			output, snapshot, ended := deploymentLogOutput(t, frames)
			if output["stdout"] != test.stdout || output["stderr"] != test.stderr || snapshot != test.snapshot || !ended {
				t.Fatalf("stream mismatch: output=%q snapshot=%v ended=%v", output, snapshot, ended)
			}
			if len(frames) == 0 || frames[len(frames)-1].cursor != finalCursor {
				t.Fatalf("missing final cursor %s: %+v", finalCursor, frames)
			}
		})
	}

	// Disconnect after each individual message, including between stdout and
	// stderr. Every frame must acknowledge only bytes already delivered.
	frames := deploymentLogFrames(t, deploymentLogRequest(t, handler, "0"))
	for i, frame := range frames {
		if frame.event == "end" {
			continue
		}
		delivered, _, _ := deploymentLogOutput(t, frames[:i+1])
		resumed, snapshot, ended := deploymentLogOutput(t, deploymentLogFrames(t, deploymentLogRequest(t, handler, frame.cursor)))
		if snapshot || !ended || delivered["stdout"]+resumed["stdout"] != history || delivered["stderr"]+resumed["stderr"] != stderr {
			t.Fatalf("reconnect at %q lost or repeated bytes: delivered=%q resumed=%q", frame.cursor, delivered, resumed)
		}
	}
}

func TestDeploymentLogsWaitForBothEOFs(t *testing.T) {
	for _, test := range []struct {
		name                 string
		stdoutEOF, stderrEOF bool
		stderrEnd            uint64
		wantEnd              bool
	}{
		{name: "neither EOF"},
		{name: "only stdout EOF", stdoutEOF: true},
		{name: "only stderr EOF", stderrEOF: true, stderrEnd: 5},
		{name: "both drained", stdoutEOF: true, stderrEOF: true, stderrEnd: 5, wantEnd: true},
		{name: "stderr bytes still missing", stdoutEOF: true, stderrEOF: true, stderrEnd: 10},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler, runService, runID := deploymentLogFixture(t)
			if err := runService.AppendLog(runID, "dep", 0, "stderr", []byte("ready")); err != nil {
				t.Fatal(err)
			}
			if test.stdoutEOF {
				runService.MarkLogEnd(runID, "dep", 0, "stdout", 0)
			}
			if test.stderrEOF {
				runService.MarkLogEnd(runID, "dep", 0, "stderr", test.stderrEnd)
			}
			output, _, ended := deploymentLogOutput(t, deploymentLogFrames(t, deploymentLogRequest(t, handler, "0")))
			if ended != test.wantEnd || output["stderr"] != "ready" || output["stdout"] != "" {
				t.Fatalf("output=%q ended=%v, want end=%v", output, ended, test.wantEnd)
			}
		})
	}
}

func TestDeploymentLogsRejectInvalidCursors(t *testing.T) {
	handler, _, _ := deploymentLogFixture(t)
	for _, cursor := range []string{"12", "bad", "1:", ":2", "-1:2", "1:2:3", "18446744073709551616:0"} {
		if response := deploymentLogRequest(t, handler, cursor); response.Code != http.StatusBadRequest {
			t.Errorf("cursor %q: HTTP %d, want 400", cursor, response.Code)
		}
	}
}

func readDeploymentLogFrame(t *testing.T, reader *bufio.Reader) deploymentLogFrame {
	t.Helper()
	var frame deploymentLogFrame
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading live SSE: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if frame.data != "" {
				return frame
			}
			continue
		}
		if value, ok := strings.CutPrefix(line, "id: "); ok {
			frame.cursor = value
		}
		if value, ok := strings.CutPrefix(line, "event: "); ok {
			frame.event = value
		}
		if value, ok := strings.CutPrefix(line, "data: "); ok {
			frame.data = value
		}
	}
}

func TestDeploymentLogsDeliverFreshBytesAfterOpenAndReconnectForEveryRank(t *testing.T) {
	for _, rank := range []int32{0, 1} {
		t.Run(fmt.Sprintf("rank%d", rank), func(t *testing.T) {
			handler, runService, runID := deploymentLogFixture(t)
			server := httptest.NewServer(handler)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			appendText := func(stream, data string) {
				t.Helper()
				if err := runService.AppendLog(runID, "dep", rank, stream, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			open := func(cursor string) (*http.Response, *bufio.Reader) {
				t.Helper()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/deployments/dep/logs?rank=%d", server.URL, rank), nil)
				if err != nil {
					t.Fatal(err)
				}
				if cursor != "" {
					request.Header.Set("Last-Event-ID", cursor)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK {
					response.Body.Close()
					t.Fatalf("HTTP %d", response.StatusCode)
				}
				return response, bufio.NewReader(response.Body)
			}
			appendText("stdout", "old out\n")
			appendText("stderr", "old err\n")
			response, reader := open("")
			defer response.Body.Close()
			snapshot := readDeploymentLogFrame(t, reader)
			if snapshot.event != "snapshot" {
				t.Fatalf("initial event = %+v", snapshot)
			}
			// Neither fresh marker exists until the client has received the
			// snapshot on an already-open real HTTP response.
			appendText("stdout", "fresh out\n")
			appendText("stderr", "fresh err\n")
			first := readDeploymentLogFrame(t, reader)
			output, wasSnapshot, ended := deploymentLogOutput(t, []deploymentLogFrame{first})
			if wasSnapshot || ended || output["stdout"] != "fresh out\n" || output["stderr"] != "" {
				t.Fatalf("first live message = %+v", first)
			}
			response.Body.Close() // reconnect between the two stream frames
			response, reader = open(first.cursor)
			defer response.Body.Close()
			second := readDeploymentLogFrame(t, reader)
			resumed, wasSnapshot, ended := deploymentLogOutput(t, []deploymentLogFrame{second})
			if wasSnapshot || ended || resumed["stdout"] != "" || resumed["stderr"] != "fresh err\n" {
				t.Fatalf("resumed message lost or duplicated bytes: %+v", second)
			}
			appendText("stdout", "after reconnect out\n")
			appendText("stderr", "after reconnect err\n")
			for _, stream := range []string{"stdout", "stderr"} {
				size, err := runService.LogSize(runID, "dep", rank, stream)
				if err != nil {
					t.Fatal(err)
				}
				runService.MarkLogEnd(runID, "dep", rank, stream, size)
			}
			var frames []deploymentLogFrame
			for {
				frame := readDeploymentLogFrame(t, reader)
				frames = append(frames, frame)
				if frame.event == "end" {
					break
				}
			}
			final, wasSnapshot, ended := deploymentLogOutput(t, frames)
			if wasSnapshot || !ended || final["stdout"] != "after reconnect out\n" || final["stderr"] != "after reconnect err\n" {
				t.Fatalf("fresh reconnected output = %q, snapshot=%v ended=%v", final, wasSnapshot, ended)
			}
		})
	}
}

func TestDeploymentLogsResetStaleCursorsTogether(t *testing.T) {
	handler, runService, runID := deploymentLogFixture(t)
	for stream, data := range map[string]string{"stdout": "restored out\n", "stderr": "restored err\n"} {
		if err := runService.AppendLog(runID, "dep", 0, stream, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	frames := deploymentLogFrames(t, deploymentLogRequest(t, handler, "99999:99999"))
	output, snapshot, ended := deploymentLogOutput(t, frames)
	if !snapshot || ended || output["stdout"] != "restored out\n" || output["stderr"] != "restored err\n" {
		t.Fatalf("stale cursor recovery = %q snapshot=%v ended=%v", output, snapshot, ended)
	}
	if frames[len(frames)-1].cursor != "13:13" {
		t.Fatalf("stale cursor not replaced: %+v", frames)
	}
}
