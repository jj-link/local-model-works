package server

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/jj-link/local-model-works/internal/db"
	"github.com/jj-link/local-model-works/internal/events"
	"github.com/jj-link/local-model-works/internal/nodes"
	"github.com/jj-link/local-model-works/internal/runs"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

func TestLogIngestionRecoversOvertakenGapsWithoutReplayDuplication(t *testing.T) {
	for _, stream := range []string{"stdout", "stderr"} {
		for _, rank := range []int32{0, 1} {
			t.Run(fmt.Sprintf("%s/rank%d", stream, rank), func(t *testing.T) {
				s := &Server{runs: runs.New(nil, nil, nil, t.TempDir())}
				conn := nodes.NewConn("node")
				defer conn.Close()
				pending := make(map[string]logReplay)
				receive := func(offset uint64, data string, final bool, end uint64) {
					t.Helper()
					if err := s.receiveLog(conn, &agentv1.LogChunk{
						RunId: "run", DeploymentId: "dep", Rank: rank, Stream: stream,
						Offset: offset, Data: []byte(data), Final: final, EndOffset: end,
					}, pending); err != nil {
						t.Fatal(err)
					}
				}
				request := func(from uint64) {
					t.Helper()
					select {
					case message := <-conn.SendCh:
						req := message.GetLogRequest()
						if req == nil || req.GetFromOffset() != from || req.GetRunId() != "run" || req.GetDeploymentId() != "dep" || req.GetRank() != rank || req.GetStream() != stream {
							t.Fatalf("replay request = %+v, want cursor %d", message, from)
						}
					default:
						t.Fatalf("no replay request from %d", from)
					}
				}
				receive(0, "abc", false, 0)
				receive(0, "abcdef", false, 0) // overlapping replay
				receive(9, "jkl", false, 0)
				request(6)
				receive(12, "mno", false, 0) // overtakes the first replay
				if len(conn.SendCh) != 0 {
					t.Fatal("duplicate request while the original gap is still pending")
				}
				receive(6, "ghijkl", false, 0)
				request(12) // recover the newer gap even if the source is now quiet
				receive(12, "mno", false, 0)
				receive(9, "jkl", false, 0) // already acknowledged
				receive(0, "", true, 18)    // EOF itself exposes missing final output
				request(15)
				if _, done := s.runs.LogEnded("run", "dep", rank, stream); done {
					t.Fatal("missing bytes were acknowledged as drained")
				}
				receive(15, "pqr", false, 0)
				chunk, next, size, err := s.runs.ReadLog("run", "dep", rank, stream, 0, 0)
				if err != nil || string(chunk) != "abcdefghijklmnopqr" || next != 18 || size != 18 {
					t.Fatalf("consumer bytes = %q cursor=%d size=%d err=%v", chunk, next, size, err)
				}
				if end, ok := s.runs.LogEnded("run", "dep", rank, stream); !ok || end != 18 {
					t.Fatalf("EOF = %d, %v", end, ok)
				}
				if len(pending) != 0 || len(conn.SendCh) != 0 {
					t.Fatal("recovered stream left an outstanding replay")
				}
			})
		}
	}
}

func TestReconnectRequestsQuietStreamsFromStoredCursorsForEveryRank(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	q := db.New(database)
	runService := runs.New(database, q, events.NewEventBus(q), t.TempDir())
	runID, err := runService.Create(ctx, "serving", "serve", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreateRecipe(ctx, db.CreateRecipeParams{Digest: "sha256:logs", Name: "logs", Version: "1", Source: "{}", Manifest: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: "dep", RecipeDigest: "sha256:logs", Parameters: "{}",
		Placement: `{"entries":[{"node_id":"node","rank":0},{"node_id":"other","rank":1},{"node_id":"node","rank":2}]}`,
		RunID:     sql.NullString{String: runID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.CreateDeployment(ctx, db.CreateDeploymentParams{
		ID: "historical", RecipeDigest: "sha256:logs", Parameters: "{}",
		Placement: `{"ranks":{"node":0}}`, RunID: sql.NullString{String: runID, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateDeploymentState(ctx, db.UpdateDeploymentStateParams{ID: "historical", DesiredState: "stopped"}); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateDeploymentObserved(ctx, db.UpdateDeploymentObservedParams{ID: "historical", ObservedState: "stopped", Diagnostics: "[]"}); err != nil {
		t.Fatal(err)
	}
	if err := runService.AppendLog(runID, "dep", 0, "stdout", []byte("stored")); err != nil {
		t.Fatal(err)
	}
	if err := runService.AppendLog(runID, "dep", 2, "stderr", []byte("err")); err != nil {
		t.Fatal(err)
	}
	conn := nodes.NewConn("node")
	defer conn.Close()
	s := &Server{q: q, runs: runService}
	s.replayDeploymentLogs(ctx, conn)
	want := map[string]uint64{"0/stdout": 6, "0/stderr": 0, "2/stdout": 0, "2/stderr": 3}
	for len(conn.SendCh) > 0 {
		req := (<-conn.SendCh).GetLogRequest()
		key := fmt.Sprintf("%d/%s", req.GetRank(), req.GetStream())
		from, ok := want[key]
		if !ok || req.GetRunId() != runID || req.GetDeploymentId() != "dep" || req.GetFromOffset() != from {
			t.Fatalf("unexpected replay request: %+v", req)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("quiet streams were not requested: %v", want)
	}
}

func TestOneShotSourceOffsetsDoNotDiscardControllerPrefixedOutput(t *testing.T) {
	s := &Server{runs: runs.New(nil, nil, nil, t.TempDir())}
	if err := s.runs.AppendLog("build", "", 0, "stdout", []byte("controller preflight\n")); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []*agentv1.LogChunk{
		{RunId: "build", Stream: "stdout", Offset: 0, Data: []byte("agent output\n")},
		{RunId: "build", Stream: "stdout", Final: true, EndOffset: uint64(len("agent output\n"))},
	} {
		if _, err := s.appendLog(chunk); err != nil {
			t.Fatal(err)
		}
	}
	output, next, _, err := s.runs.ReadLog("build", "", 0, "stdout", 0, 0)
	if err != nil || string(output) != "controller preflight\nagent output\n" {
		t.Fatalf("merged one-shot log = %q err=%v", output, err)
	}
	if end, done := s.runs.LogEnded("build", "", 0, "stdout"); !done || end != next {
		t.Fatalf("merged EOF = %d, %v; want %d", end, done, next)
	}
}
