package hardware

import (
	"testing"
	"time"
)

func TestSamplerCPUAndNetworkDeltas(t *testing.T) {
	s := newHostSampler()
	t0 := time.Unix(1_800_000_000, 0)

	// First sample: counters seed, nothing to diff → zero values.
	var first Telemetry
	s.apply(&first, t0, 2000, 1000, []netCounter{{name: "eth0", rx: 1000, tx: 500}})
	if first.CPUUsagePercent != 0 || first.NetRxBytesPerSec != 0 || first.NetTxBytesPerSec != 0 {
		t.Fatalf("first sample not zero: %+v", first)
	}

	// Second sample 5s later: +100 total/+40 idle → busy 60 over 100 = 60%.
	// Network: +1000 rx / +500 tx over 5s → 200 B/s / 100 B/s.
	var second Telemetry
	s.apply(&second, t0.Add(5*time.Second), 2100, 1040, []netCounter{{name: "eth0", rx: 2000, tx: 1000}})
	if second.CPUUsagePercent != 60 {
		t.Fatalf("cpu=%d want 60", second.CPUUsagePercent)
	}
	if second.NetRxBytesPerSec != 200 || second.NetTxBytesPerSec != 100 {
		t.Fatalf("net=%d/%d want 200/100", second.NetRxBytesPerSec, second.NetTxBytesPerSec)
	}
	if len(second.NetworkInterfaces) != 1 || second.NetworkInterfaces[0].RxBytesPerSec != 200 {
		t.Fatalf("iface=%+v", second.NetworkInterfaces)
	}
}

func TestSamplerCounterRollbackYieldsZero(t *testing.T) {
	s := newHostSampler()
	t0 := time.Unix(1_800_000_000, 0)
	s.apply(&Telemetry{}, t0, 2100, 1040, []netCounter{{name: "eth0", rx: 2000, tx: 1000}})

	// Counter rollback (netdev reset) must not produce a negative or spike.
	var reset Telemetry
	s.apply(&reset, t0.Add(5*time.Second), 2100, 1040, []netCounter{{name: "eth0", rx: 100, tx: 50}})
	if reset.NetRxBytesPerSec != 0 || reset.NetTxBytesPerSec != 0 {
		t.Fatalf("rollback produced rates: %+v", reset)
	}
	if reset.CPUUsagePercent != 0 {
		t.Fatalf("zero cpu delta should be 0: %d", reset.CPUUsagePercent)
	}
}

func TestFilesystemSamplingDedupPrefersRoot(t *testing.T) {
	// preferPath selects "/" over anything else, and the shorter path otherwise.
	if preferPath("/", "/home") != "/" {
		t.Fatal("root must win")
	}
	if preferPath("/home", "/") != "/" {
		t.Fatal("root must win over longer")
	}
	if preferPath("/a/b", "/a") != "/a" {
		t.Fatal("shorter path must win")
	}
}

func TestThrottleReasonsMapping(t *testing.T) {
	cases := []struct {
		mask uint64
		want []string
	}{
		{clocksThermalSW | clocksThermalHW, []string{"thermal"}},
		{clocksPowerSW | clocksPowerCap, []string{"power"}},
		{clocksHardware, []string{"hardware"}},
		{clocksThermalSW | clocksPowerSW | clocksHardware, []string{"thermal", "power", "hardware"}},
		{1 << 0 /* GPU_IDLE */, nil},
		{1 << 11 /* DISPLAY_CLOCK */, nil},
		{0, nil},
	}
	for _, c := range cases {
		got := throttleReasons(c.mask)
		if len(got) != len(c.want) {
			t.Fatalf("mask %x: got %v want %v", c.mask, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("mask %x: got %v want %v", c.mask, got, c.want)
			}
		}
	}
}

func TestParseNetCountersExcludesLoopback(t *testing.T) {
	raw := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets
  eth0:  1000       0    0    0    0     0          0         0    2000       0
    lo:  99999      0    0    0    0     0          0         0  999999       0
`
	counters := parseNetCounters([]byte(raw))
	if len(counters) != 1 || counters[0].name != "eth0" || counters[0].rx != 1000 || counters[0].tx != 2000 {
		t.Fatalf("counters=%+v", counters)
	}
}

func TestCpuCumulativeParsing(t *testing.T) {
	raw := []byte("cpu  2000 10 20 1000 30 10 0 0 0 0\n")
	total, idle := cpuCumulative(raw)
	if idle != 1000+30 {
		t.Fatalf("idle=%d want 1030", idle)
	}
	// 2000+10+20+1000+30+10 = 3070
	if total != 3070 {
		t.Fatalf("total=%d want 3070", total)
	}
}
