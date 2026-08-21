package hardware

import "testing"

// TestParseMeminfo guards the colon bug: fmtSscanf returns the key without
// the trailing colon ("MemTotal"), so the switch cases must match the
// unstripped name or memory is never populated. Host total is also the
// GB10 unified-memory fallback for the accelerator report.
func TestParseMeminfo(t *testing.T) {
	raw := "MemTotal:       127535204 kB\nMemAvailable:    1000 kB\nSwapTotal:       15 kB\nSwapFree:        14 kB\n"
	var tm Telemetry
	parseMeminfo([]byte(raw), &tm)
	if tm.MemoryTotalBytes != 127535204*1024 {
		t.Fatalf("MemoryTotalBytes = %d, want %d", tm.MemoryTotalBytes, 127535204*1024)
	}
	if tm.MemoryUsedBytes != 127535204*1024-1000*1024 {
		t.Fatalf("MemoryUsedBytes = %d, want %d", tm.MemoryUsedBytes, 127535204*1024-1000*1024)
	}
}
