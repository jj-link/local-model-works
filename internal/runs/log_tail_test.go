package runs

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogTailBoundsHistoryAndResumesAtExactCursor(t *testing.T) {
	s := &Service{runRoot: t.TempDir()}
	var lines []string
	for i := range 250 {
		lines = append(lines, fmt.Sprintf("line %03d\n", i))
	}
	history := strings.Join(lines, "")
	if err := s.AppendLog("run", "dep", 0, "stdout", []byte(history)); err != nil {
		t.Fatal(err)
	}
	chunk, offset, err := s.ReadLogTail("run", "dep", 0, "stdout")
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != strings.Join(lines[50:], "") || offset != uint64(len(history)) {
		t.Fatalf("tail/cursor mismatch: lines=%d offset=%d", strings.Count(string(chunk), "\n"), offset)
	}
	if err := s.AppendLog("run", "dep", 0, "stdout", []byte("new live line\n")); err != nil {
		t.Fatal(err)
	}
	live, next, _, err := s.ReadLog("run", "dep", 0, "stdout", offset, 0)
	if err != nil || string(live) != "new live line\n" || next != offset+uint64(len(live)) {
		t.Fatalf("resumed output = %q, next=%d, error=%v", live, next, err)
	}
}

func TestLogTailDropsWindowFragmentAndPreservesUnfinishedLine(t *testing.T) {
	s := &Service{runRoot: t.TempDir()}
	tail := "WARNING: worker recovered\npartial"
	history := strings.Repeat("界", 30000) + "\n" + tail
	if err := s.AppendLog("run", "dep", 1, "stdout", []byte(history)); err != nil {
		t.Fatal(err)
	}
	chunk, offset, err := s.ReadLogTail("run", "dep", 1, "stdout")
	if err != nil || string(chunk) != tail || !utf8.Valid(chunk) || offset != uint64(len(history)) {
		t.Fatalf("tail = %q, offset=%d, error=%v", chunk, offset, err)
	}
	if err := s.AppendLog("run", "dep", 1, "stdout", []byte(" continuation\n")); err != nil {
		t.Fatal(err)
	}
	live, _, _, err := s.ReadLog("run", "dep", 1, "stdout", offset, 0)
	if err != nil || string(chunk)+string(live) != tail+" continuation\n" {
		t.Fatalf("split line lost or duplicated: %q + %q, error=%v", chunk, live, err)
	}
}

func TestLogTailMissingEmptyAndUnsafePaths(t *testing.T) {
	s := &Service{runRoot: t.TempDir()}
	chunk, offset, err := s.ReadLogTail("run", "missing", 0, "stdout")
	if err != nil || len(chunk) != 0 || offset != 0 {
		t.Fatalf("missing log: %q, %d, %v", chunk, offset, err)
	}
	if err := s.AppendLog("run", "empty", 0, "stdout", nil); err != nil {
		t.Fatal(err)
	}
	chunk, offset, err = s.ReadLogTail("run", "empty", 0, "stdout")
	if err != nil || len(chunk) != 0 || offset != 0 {
		t.Fatalf("empty log: %q, %d, %v", chunk, offset, err)
	}
	if _, _, err := s.ReadLogTail("../outside", "dep", 0, "stdout"); err == nil {
		t.Fatal("unsafe log path accepted")
	}
}

func TestLogReadsDoNotAcknowledgeSplitUTF8UntilComplete(t *testing.T) {
	s := New(nil, nil, nil, t.TempDir())
	text := []byte("ready 界\n")
	prefix := len("ready ") + 1
	if err := s.AppendLog("run", "dep", 0, "stderr", text[:prefix]); err != nil {
		t.Fatal(err)
	}
	snapshot, cursor, err := s.ReadLogTail("run", "dep", 0, "stderr")
	if err != nil || string(snapshot) != "ready " || cursor != uint64(len("ready ")) {
		t.Fatalf("snapshot acknowledged an incomplete rune: %q cursor=%d err=%v", snapshot, cursor, err)
	}
	chunk, next, _, err := s.ReadLogText("run", "dep", 0, "stderr", cursor)
	if err != nil || len(chunk) != 0 || next != cursor {
		t.Fatalf("incomplete live suffix = %q cursor=%d err=%v", chunk, next, err)
	}
	if err := s.AppendLog("run", "dep", 0, "stderr", text[prefix:]); err != nil {
		t.Fatal(err)
	}
	chunk, next, _, err = s.ReadLogText("run", "dep", 0, "stderr", cursor)
	if err != nil || string(snapshot)+string(chunk) != string(text) || next != uint64(len(text)) || !utf8.Valid(chunk) {
		t.Fatalf("completed output = %q cursor=%d err=%v", chunk, next, err)
	}
	// Raw readers keep exact byte-range semantics even inside a rune.
	chunk, next, _, err = s.ReadLog("run", "dep", 0, "stderr", cursor, 1)
	if err != nil || len(chunk) != 1 || chunk[0] != text[len("ready ")] || next != cursor+1 {
		t.Fatalf("raw range changed: %q cursor=%d err=%v", chunk, next, err)
	}
}

func TestLogTextReadLimitDoesNotSplitUTF8(t *testing.T) {
	s := New(nil, nil, nil, t.TempDir())
	prefix := strings.Repeat("x", (256<<10)-1)
	text := prefix + "界\n"
	if err := s.AppendLog("run", "dep", 0, "stdout", []byte(text)); err != nil {
		t.Fatal(err)
	}
	first, cursor, _, err := s.ReadLogText("run", "dep", 0, "stdout", 0)
	if err != nil || string(first) != prefix || cursor != uint64(len(prefix)) {
		t.Fatalf("bounded text read split a rune: size=%d cursor=%d err=%v", len(first), cursor, err)
	}
	last, next, _, err := s.ReadLogText("run", "dep", 0, "stdout", cursor)
	if err != nil || string(first)+string(last) != text || next != uint64(len(text)) {
		t.Fatalf("bounded suffix = %q cursor=%d err=%v", last, next, err)
	}
}

func TestLogReadsDrainMalformedFinalUTF8AtConfirmedEOF(t *testing.T) {
	s := New(nil, nil, nil, t.TempDir())
	data := []byte{'x', 0xe7}
	if err := s.AppendLog("run", "dep", 0, "stdout", data); err != nil {
		t.Fatal(err)
	}
	s.MarkLogEnd("run", "dep", 0, "stdout", uint64(len(data)))
	chunk, next, _, err := s.ReadLogText("run", "dep", 0, "stdout", 0)
	if err != nil || string(chunk) != string(data) || next != uint64(len(data)) {
		t.Fatalf("malformed EOF stalled: %q cursor=%d err=%v", chunk, next, err)
	}
}
