package hardware

import (
	"context"
	"fmt"
	"sync"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// nvidiaDriver implements Driver over NVML. The NVML library is dlopened on
// first Init; a missing library surfaces as an error, not a crash.
type nvidiaDriver struct{}

var (
	nvmlOnce sync.Once
	nvmlMu   sync.Mutex
	nvmlLib  nvml.Interface
	nvmlErr  error
)

// NewNvidia returns the NVIDIA driver.
func NewNvidia() Driver { return &nvidiaDriver{} }

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
	for i := 0; i < count; i++ {
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
		if mem, rc := lib.DeviceGetMemoryInfo(dev); rc == nvml.SUCCESS {
			a.MemoryBytes = int64(mem.Total)
		}
		if major, minor, rc := lib.DeviceGetCudaComputeCapability(dev); rc == nvml.SUCCESS {
			a.Architecture = fmt.Sprintf("sm_%d%d", major, minor)
		}
		inv.Accelerators = append(inv.Accelerators, a)
	}
	return inv, nil
}

func (d *nvidiaDriver) Sample(ctx context.Context) (Telemetry, error) {
	t := hostSample(ctx)
	lib, err := nvmlInit()
	if err != nil {
		return t, nil // host sample still valid; accelerators unavailable
	}
	count, rc := lib.DeviceGetCount()
	if rc != nvml.SUCCESS {
		return t, nil
	}
	for i := 0; i < count; i++ {
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
