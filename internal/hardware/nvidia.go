package hardware

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// Throttle event-reason bits (NVML_CLOCKS_EVENT_REASON_*). go-nvml does not
// export these constants, so the meaningful operator-visible causes are
// declared locally. Idle, low-utilization, submitted/auto-boost, and
// display-clock reasons are intentionally ignored.
const (
	clocksThermalSW = 1 << 9 // APP_CLOCKS_SW_THERMAL_SLOWDOWN
	clocksThermalHW = 1 << 8 // APP_CLOCKS_HW_THERMAL_SLOWDOWN
	clocksPowerCap  = 1 << 4 // APP_CLOCKS_POWER_CAP
	clocksPowerSW   = 1 << 7 // APP_CLOCKS_SW_POWER_CAP
	clocksHardware  = 1 << 6 // APP_CLOCKS_HW_SLOWDOWN
)

// nvidiaDriver implements Driver over NVML. The NVML library is dlopened on
// first Init; a missing library surfaces as an error, not a crash. It owns a
// hostSampler so interval CPU/network values survive between samples.
type nvidiaDriver struct {
	host hostSampler
}

var (
	nvmlOnce sync.Once
	nvmlMu   sync.Mutex
	nvmlLib  nvml.Interface
	nvmlErr  error
)

// NewNvidia returns the NVIDIA driver.
func NewNvidia() Driver { return &nvidiaDriver{host: *newHostSampler()} }

func nvmlInit() (nvml.Interface, error) {
	nvmlOnce.Do(func() {
		lib := nvml.New()
		if rc := lib.Init(); rc != nvml.SUCCESS {
			nvmlErr = fmt.Errorf("nvml init: %v", rc)
			return
		}
		nvmlMu.Lock()
		nvmlLib = lib
		nvmlMu.Unlock()
	})
	nvmlMu.Lock()
	defer nvmlMu.Unlock()
	return nvmlLib, nvmlErr
}

// ShutdownNVML releases the NVML library at agent teardown. It only shuts
// down a library that was actually initialized; calling it before the first
// Probe is a no-op and does not prevent later initialization.
func ShutdownNVML() {
	nvmlMu.Lock()
	lib := nvmlLib
	nvmlMu.Unlock()
	if lib != nil {
		lib.Shutdown()
	}
}

func (d *nvidiaDriver) ID() string { return "nvidia" }

func (d *nvidiaDriver) Probe(ctx context.Context) (Inventory, error) {
	lib, err := nvmlInit()
	if err != nil {
		return Inventory{}, err
	}
	count, rc := lib.DeviceGetCount()
	if rc != nvml.SUCCESS {
		return Inventory{}, fmt.Errorf("nvml device count: %v", rc)
	}
	inv := Inventory{Accelerators: make([]Accelerator, 0, count)}
	// DGX Spark / GB10 exposes unified memory, so NVML's DeviceGetMemoryInfo
	// reports [N/A] (or a zero total); fall back to the host total so
	// unified-memory parts still show a schedulable memory figure.
	fbp := hostMemoryTotal()
	for i := range count {
		dev, rc := lib.DeviceGetHandleByIndex(i)
		if rc != nvml.SUCCESS {
			return Inventory{}, fmt.Errorf("nvml device %d: %v", i, rc)
		}
		a := Accelerator{Index: i, Vendor: "nvidia"}
		if name, rc := lib.DeviceGetName(dev); rc == nvml.SUCCESS {
			a.Name = name
		}
		if uuid, rc := lib.DeviceGetUUID(dev); rc == nvml.SUCCESS {
			a.UUID = uuid
		}
		if mem, rc := lib.DeviceGetMemoryInfo(dev); rc == nvml.SUCCESS && mem.Total > 0 {
			a.MemoryBytes = int64(mem.Total)
		} else if fbp > 0 {
			a.MemoryBytes = int64(fbp)
		}
		if major, minor, rc := lib.DeviceGetCudaComputeCapability(dev); rc == nvml.SUCCESS {
			a.Architecture = fmt.Sprintf("sm_%d%d", major, minor)
		}
		inv.Accelerators = append(inv.Accelerators, a)
	}
	return inv, nil
}

func (d *nvidiaDriver) Sample(ctx context.Context) (Telemetry, error) {
	t := d.host.Sample(ctx)
	lib, err := nvmlInit()
	if err != nil {
		return t, nil // host sample still valid; accelerators unavailable
	}
	count, rc := lib.DeviceGetCount()
	if rc != nvml.SUCCESS {
		return t, nil
	}
	for i := range count {
		dev, rc := lib.DeviceGetHandleByIndex(i)
		if rc != nvml.SUCCESS {
			continue
		}
		at := AcceleratorTelemetry{Index: i}
		if mem, rc := lib.DeviceGetMemoryInfo(dev); rc == nvml.SUCCESS {
			at.MemTotalBytes = mem.Total
			at.MemUsedBytes = mem.Used
		}
		if temp, rc := lib.DeviceGetTemperature(dev, nvml.TEMPERATURE_GPU); rc == nvml.SUCCESS {
			at.TemperatureC = temp
		}
		if util, rc := lib.DeviceGetUtilizationRates(dev); rc == nvml.SUCCESS {
			at.UtilizationPct = util.Gpu
		}
		if power, rc := lib.DeviceGetPowerUsage(dev); rc == nvml.SUCCESS {
			at.PowerMW = power
		}
		if limit, rc := lib.DeviceGetEnforcedPowerLimit(dev); rc == nvml.SUCCESS {
			at.PowerLimitMW = limit
		}
		if reasons, rc := lib.DeviceGetCurrentClocksEventReasons(dev); rc == nvml.SUCCESS {
			at.ThrottleReasons = throttleReasons(reasons)
		}
		at.Processes = computeProcesses(dev)
		t.Accelerators = append(t.Accelerators, at)
	}
	return t, nil
}

func (d *nvidiaDriver) Validate(ctx context.Context, req Requirement) []Diagnostic {
	inv, err := d.Probe(ctx)
	if err != nil {
		return []Diagnostic{fail("node.accelerator-unavailable", err.Error(), "accelerators")}
	}
	return MatchRequirement(inv.Accelerators, req)
}

// throttleReasons maps a clocks-event-reason bitmask to stable operator-facing
// causes. Only the meaningful slowdown causes are surfaced.
func throttleReasons(reasons uint64) []string {
	var out []string
	if reasons&clocksThermalSW != 0 || reasons&clocksThermalHW != 0 {
		out = append(out, "thermal")
	}
	if reasons&clocksPowerSW != 0 || reasons&clocksPowerCap != 0 {
		out = append(out, "power")
	}
	if reasons&clocksHardware != 0 {
		out = append(out, "hardware")
	}
	return out
}

// computeProcesses reports up to the five largest GPU processes by memory
// (descending, then PID). Rows whose memory is NVML's VALUE_NOT_AVAILABLE
// sentinel are dropped; names resolve from /proc/<pid>/comm, empty when
// unreadable.
func computeProcesses(dev nvml.Device) []AcceleratorProcess {
	procs, rc := dev.GetComputeRunningProcesses()
	if rc != nvml.SUCCESS {
		return nil
	}
	const valueNotAvailable = ^uint64(0)
	type candidate struct {
		pid  uint32
		mem  uint64
		name string
	}
	var cands []candidate
	for _, p := range procs {
		if p.UsedGpuMemory == valueNotAvailable {
			continue
		}
		cands = append(cands, candidate{
			pid:  p.Pid,
			mem:  p.UsedGpuMemory,
			name: procName(p.Pid),
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].mem != cands[j].mem {
			return cands[i].mem > cands[j].mem
		}
		return cands[i].pid < cands[j].pid
	})
	if len(cands) > 5 {
		cands = cands[:5]
	}
	out := make([]AcceleratorProcess, 0, len(cands))
	for _, c := range cands {
		out = append(out, AcceleratorProcess{
			PID:        int32(c.pid),
			Name:       c.name,
			UsedGpuMem: c.mem,
		})
	}
	return out
}

// procName reads a process name from /proc/<pid>/comm.
func procName(pid uint32) string {
	raw, err := os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/comm")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}
