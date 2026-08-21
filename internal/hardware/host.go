package hardware

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// hostSample reads CPU, memory, and network counters from /proc.
func hostSample(ctx context.Context) Telemetry {
	var t Telemetry
	t.CPUCores = uint32(runtime.NumCPU())
	if stat, err := os.ReadFile("/proc/stat"); err == nil {
		t.CPUUsagePercent, t.Load1x100 = cpuUsage(stat)
	}
	if meminfo, err := os.ReadFile("/proc/meminfo"); err == nil {
		parseMeminfo(meminfo, &t)
	}
	if netdev, err := os.ReadFile("/proc/net/dev"); err == nil {
		t.NetRxBytes, t.NetTxBytes = netCounters(netdev)
	}
	return t
}

func cpuUsage(stat []byte) (usagePct uint32, load1x100 uint64) {
	sc := bufio.NewScanner(strings.NewReader(string(stat)))
	var idle, total uint64
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		for i, f := range fields[1:] {
			v, _ := strconv.ParseUint(f, 10, 64)
			total += v
			if i == 3 || i == 4 { // idle + iowait
				idle += v
			}
		}
		break
	}
	if total == 0 {
		return 0, 0
	}
	busy := total - idle
	usagePct = uint32(busy * 100 / total)
	if loadavg, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(loadavg))
		if len(parts) >= 1 {
			if f, err := strconv.ParseFloat(parts[0], 64); err == nil {
				load1x100 = uint64(f * 100)
			}
		}
	}
	return usagePct, load1x100
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

func netCounters(raw []byte) (rx, tx uint64) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	for sc.Scan() {
		line := sc.Text()
		idx := strings.Index(line, ":")
		if idx < 0 {
			continue
		}
		ifage := strings.TrimSpace(line[:idx])
		if ifage == "lo" {
			continue
		}
		fields := strings.Fields(line[idx+1:])
		if len(fields) < 9 {
			continue
		}
		if v, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
			rx += v
		}
		if v, err := strconv.ParseUint(fields[8], 10, 64); err == nil {
			tx += v
		}
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

func (f *Fake) ID() string {
	if f.Inventory.Arch == "" {
		return "fake"
	}
	return "fake"
}

func (f *Fake) Probe(ctx context.Context) (Inventory, error)  { return f.Inventory, nil }
func (f *Fake) Sample(ctx context.Context) (Telemetry, error) { return f.Telem, nil }
func (f *Fake) Validate(ctx context.Context, req Requirement) []Diagnostic {
	if f.ValidateFn != nil {
		return f.ValidateFn(ctx, req)
	}
	return MatchRequirement(f.Inventory.Accelerators, req)
}
