package hardware

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hostSampler computes CPU utilization and network byte rates from counter
// deltas between successive samples rather than since-boot totals, so a busy
// node reports live utilization instead of a lifetime average. It is not
// safe for concurrent use by multiple samplers; the NVML driver owns exactly
// one instance for the process.
type hostSampler struct {
	mu      sync.Mutex
	last    time.Time
	cpuTotal uint64
	cpuIdle  uint64
	aggRx   uint64
	aggTx   uint64
	ifaces  map[string][2]uint64 // interface -> [rx, tx] cumulative
}

func newHostSampler() *hostSampler {
	return &hostSampler{ifaces: map[string][2]uint64{}}
}

// Sample reads the current /proc counters and derives interval values from
// the previous sample. The first sample, counter rollback, and an invalid
// elapsed interval all yield zero rather than a spike.
func (s *hostSampler) Sample(ctx context.Context) Telemetry {
	t := Telemetry{CPUCores: uint32(runtime.NumCPU())}
	t.MemoryUsedBytes, t.MemoryTotalBytes, t.SwapUsedBytes = readMeminfo()
	t.UptimeSeconds = readUptimeSeconds()
	t.Load1x100 = readLoad1x100()

	total, idle := readCPUCumulative()
	net := readNetCounters()
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.apply(&t, now, total, idle, net)
	return t
}

// apply derives interval CPU/network values from deltas against the previous
// sample. The first sample, counter rollback, an invalid elapsed interval, and
// a zero delta all yield zero rather than a spike. Callers hold s.mu.
func (s *hostSampler) apply(t *Telemetry, now time.Time, total, idle uint64, net []netCounter) {
	var aggRx, aggTx uint64
	cur := make(map[string][2]uint64, len(net))
	for _, c := range net {
		cur[c.name] = [2]uint64{c.rx, c.tx}
		aggRx += c.rx
		aggTx += c.tx
	}

	if !s.last.IsZero() {
		dt := now.Sub(s.last).Seconds()
		if dt > 0 {
			// CPU utilization as a fraction of the busy jiffies delta.
			if total > s.cpuTotal && idle >= s.cpuIdle {
				dtTotal := total - s.cpuTotal
				dtIdle := idle - s.cpuIdle
				if dtIdle <= dtTotal {
					t.CPUUsagePercent = uint32((dtTotal - dtIdle) * 100 / dtTotal)
				}
			}
			t.NetRxBytes = aggRx
			t.NetTxBytes = aggTx
			if aggRx >= s.aggRx {
				t.NetRxBytesPerSec = byteRate(aggRx-s.aggRx, dt)
			}
			if aggTx >= s.aggTx {
				t.NetTxBytesPerSec = byteRate(aggTx-s.aggTx, dt)
			}
			for _, c := range net {
				if prev, ok := s.ifaces[c.name]; ok && c.rx >= prev[0] {
					t.NetworkInterfaces = append(t.NetworkInterfaces, NetworkInterfaceTelemetry{
						Name:          c.name,
						RxBytesPerSec: byteRate(c.rx-prev[0], dt),
						TxBytesPerSec: byteRate(c.tx-prev[1], dt),
					})
				}
			}
		}
	}

	s.last = now
	s.cpuTotal, s.cpuIdle = total, idle
	s.aggRx, s.aggTx = aggRx, aggTx
	s.ifaces = cur
}

// byteRate converts a byte delta and a wall-clock delta to whole bytes/sec.
// A zero delta (or degenerate interval) yields zero.
func byteRate(delta uint64, dt float64) uint64 {
	if delta == 0 || dt <= 0 {
		return 0
	}
	return uint64(float64(delta) / dt)
}

// hostMemoryTotal is a stateless memory snapshot used by inventory probes.
func hostMemoryTotal() uint64 {
	_, total, _ := readMeminfo()
	return total
}

// readMeminfo returns used, total, swap bytes from /proc/meminfo.
func readMeminfo() (used, total, swap uint64) {
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		var tm Telemetry
		parseMeminfo(raw, &tm)
		return tm.MemoryUsedBytes, tm.MemoryTotalBytes, tm.SwapUsedBytes
	}
	return 0, 0, 0
}

// readUptimeSeconds returns host uptime from /proc/uptime.
func readUptimeSeconds() uint64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || f < 0 {
		return 0
	}
	return uint64(f)
}

// readLoad1x100 returns the 1-minute load average scaled by 100.
func readLoad1x100() uint64 {
	raw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	parts := strings.Fields(string(raw))
	if len(parts) == 0 {
		return 0
	}
	if f, err := strconv.ParseFloat(parts[0], 64); err == nil {
		return uint64(f * 100)
	}
	return 0
}

func parseMeminfo(raw []byte, t *Telemetry) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := sc.Text()
		var key string
		var val uint64
		if _, err := fmtSscanf(line, &key, &val); err == nil {
			switch key {
			case "MemTotal":
				t.MemoryTotalBytes = val * 1024
			case "MemAvailable":
				t.MemoryUsedBytes = (t.MemoryTotalBytes - val*1024 + 1023) / 1024 * 1024
			case "SwapTotal", "SwapFree":
			}
		}
	}
}

// fmtSscanf parses "Key: value kB" lines without importing fmt's scanner
// complexity; returns the key and numeric value.
func fmtSscanf(line string, key *string, val *uint64) (int, error) {
	i := strings.Index(line, ":")
	if i < 0 {
		return 0, errNonZero
	}
	*key = strings.TrimSpace(line[:i])
	rest := strings.Fields(line[i+1:])
	if len(rest) == 0 {
		return 0, errNonZero
	}
	v, err := strconv.ParseUint(rest[0], 10, 64)
	if err != nil {
		return 0, errNonZero
	}
	*val = v
	return 2, nil
}

var errNonZero = errValue{}

type errValue struct{}

func (errValue) Error() string { return "no value" }

// readCPUCumulative returns total and idle jiffies from /proc/stat.
func readCPUCumulative() (total, idle uint64) {
	if stat, err := os.ReadFile("/proc/stat"); err == nil {
		return cpuCumulative(stat)
	}
	return 0, 0
}

func cpuCumulative(stat []byte) (total, idle uint64) {
	sc := bufio.NewScanner(strings.NewReader(string(stat)))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0
		}
		for i, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		return total, idle
	}
	return 0, 0
}

// netCounter is one interface's cumulative byte counters.
type netCounter struct {
	name  string
	rx, tx uint64
}

// readNetCounters returns cumulative per-interface counters from
// /proc/net/dev, excluding loopback.
func readNetCounters() []netCounter {
	raw, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil
	}
	return parseNetCounters(raw)
}

func parseNetCounters(raw []byte) []netCounter {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	var out []netCounter
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		iface := strings.TrimSpace(line[:idx])
		if iface == "lo" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		var c netCounter
		c.name = iface
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			c.rx = v
		}
		if v, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			c.tx = v
		}
		out = append(out, c)
	}
	return out
}

// netCounters aggregates cumulative rx/tx across interfaces (loopback
// excluded). Retained for callers that only need the sum.
func netCounters(raw []byte) (rx, tx uint64) {
	for _, c := range parseNetCounters(raw) {
		rx += c.rx
		tx += c.tx
	}
	return rx, tx
}

// ProbeNetwork fills inventory interfaces and RDMA devices from sysfs.
func ProbeNetwork(inv *Inventory) error {
	hostname, _ := os.Hostname()
	inv.Hostname = hostname
	inv.OS = "linux"
	inv.Arch = runtime.GOARCH

	entries, err := os.ReadDir("/sys/class/net")
	if err == nil {
		for _, e := range entries {
			base := filepath.Join("/sys/class/net", e.Name())
			ni := NetworkInterface{Name: e.Name()}
			if addrRaw, err := os.ReadFile(filepath.Join(base, "address")); err == nil {
				ni.Addresses = append(ni.Addresses, strings.TrimSpace(string(addrRaw)))
			}
			if mtuRaw, err := os.ReadFile(filepath.Join(base, "mtu")); err == nil {
				ni.MTU, _ = strconv.Atoi(strings.TrimSpace(string(mtuRaw)))
			}
			if speedRaw, err := os.ReadFile(filepath.Join(base, "speed")); err == nil {
				ni.LinkMbps, _ = strconv.Atoi(strings.TrimSpace(string(speedRaw)))
			}
			inv.Interfaces = append(inv.Interfaces, ni)
		}
	}
	if rdmaEntries, err := os.ReadDir("/sys/class/infiniband"); err == nil {
		for _, e := range rdmaEntries {
			dev := RdmaDevice{Name: e.Name()}
			if vendorRaw, err := os.ReadFile(filepath.Join("/sys/class/infiniband", e.Name(), "vendor_part_id")); err == nil {
				dev.Vendor = strings.TrimSpace(string(vendorRaw))
			}
			portDirs, _ := os.ReadDir(filepath.Join("/sys/class/infiniband", e.Name()))
			for _, pd := range portDirs {
				pn := pd.Name()
				if !strings.HasPrefix(pn, "ports/") {
					continue
				}
				pn = strings.TrimPrefix(pn, "ports/")
				port := RdmaPort{Name: pn}
				if stateRaw, err := os.ReadFile(filepath.Join("/sys/class/infiniband", e.Name(), "ports", pn, "state")); err == nil {
					port.State = strings.TrimSpace(string(stateRaw))
				}
				if rateRaw, err := os.ReadFile(filepath.Join("/sys/class/infiniband", e.Name(), "ports", pn, "rate")); err == nil {
					port.LinkRateGbps = parseRdmaRate(string(rateRaw))
				}
				dev.Ports = append(dev.Ports, port)
			}
			inv.RDMADevices = append(inv.RDMADevices, dev)
		}
	}
	return nil
}

// "100 Gb/sec (4X EDR)" style rate lines parse to the leading number.
func parseRdmaRate(s string) int {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0
	}
	return n
}

// Fake is a test double driver with configurable state.
type Fake struct {
	Inventory  Inventory
	Telem      Telemetry
	ValidateFn func(ctx context.Context, req Requirement) []Diagnostic
}

func (f *Fake) ID() string { return "fake" }

func (f *Fake) Probe(ctx context.Context) (Inventory, error)  { return f.Inventory, nil }
func (f *Fake) Sample(ctx context.Context) (Telemetry, error) { return f.Telem, nil }
func (f *Fake) Validate(ctx context.Context, req Requirement) []Diagnostic {
	if f.ValidateFn != nil {
		return f.ValidateFn(ctx, req)
	}
	return MatchRequirement(f.Inventory.Accelerators, req)
}
