package diag

import "testing"

func TestCompactAndUpsertKeepLatestPerCodeAndResource(t *testing.T) {
	existing := []Diagnostic{
		Error("workload.exited", "old").Res("rank:0"),
		Error("agent.offline", "offline").Res("node-a"),
		Error("workload.exited", "newer").Res("rank:0"),
	}
	compacted := Compact(existing)
	if len(compacted) != 2 {
		t.Fatalf("compacted diagnostics = %+v", compacted)
	}
	if compacted[0].Message != "newer" {
		t.Fatalf("latest duplicate message = %q, want newer", compacted[0].Message)
	}

	updated := Upsert(compacted, Error("workload.exited", "latest").Res("rank:0"))
	if len(updated) != 2 || updated[0].Message != "latest" {
		t.Fatalf("upserted diagnostics = %+v", updated)
	}
}
