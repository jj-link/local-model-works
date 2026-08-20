package fakeagent

import (
	"context"
	"fmt"

	"github.com/jj-link/local-model-works/internal/hardware"
)

// NVIDIADriver is a stub hardware.Driver emitting deterministic NVIDIA-like
// inventory (GPUs, a non-loopback interface, an optional RDMA device).
type NVIDIADriver struct {
	hostname string
	GPUs     []hardware.Accelerator
	ip       string
	hasRDMA  bool
}

// NewNVIDIADriver builds a driver with gpus NVIDIA-like accelerators (128 GiB
// sm_120 "DGX Spark"-class GPUs), a /24 interface at ip (CIDR form, e.g.
// "10.0.0.11/24") and an mlx5 RoCE device when rdma is true.
func NewNVIDIADriver(hostname string, gpus int, ip string, rdma bool) *NVIDIADriver {
	d := &NVIDIADriver{hostname: hostname, ip: ip, hasRDMA: rdma}
	for i := range gpus {
		d.GPUs = append(d.GPUs, hardware.Accelerator{
			Index:        i,
			Vendor:       "nvidia",
			Architecture: "sm_120",
			Name:         "NVIDIA DGX Spark GPU",
			UUID:         fmt.Sprintf("GPU-%s-%032d", hostname, i),
			MemoryBytes:  128 * 1024 * 1024 * 1024,
			Features:     []string{"p2p", "rdma"},
		})
	}
	return d
}

// ID implements hardware.Driver.
func (d *NVIDIADriver) ID() string { return "nvidia" }

// Probe implements hardware.Driver.
func (d *NVIDIADriver) Probe(ctx context.Context) (hardware.Inventory, error) {
	inv := hardware.Inventory{
		Hostname: d.hostname,
		OS:       "linux (fakeagent)",
		Arch:     "aarch64",
		Docker:   hardware.DockerInfo{OK: true, Version: "27.0.0-fake", APIVersion: "1.47"},
		// "lmw-eth0": a name no real host interface uses, so the stub's
		// entry survives the host-baseline name-collision merge (the agent
		// keeps host-reported entries on duplicate names) and the fixture
		// inventory is deterministic on any test machine.
		Interfaces: []hardware.NetworkInterface{
			{Name: "lo", Addresses: []string{"127.0.0.1/8"}, MTU: 65536},
			{Name: "lmw-eth0", Addresses: []string{d.ip}, MTU: 1500, LinkMbps: 10000},
		},
		CacheRoots: []hardware.CacheRoot{},
	}
	if len(d.GPUs) > 0 {
		inv.Accelerators = append(inv.Accelerators, d.GPUs...)
	}
	if d.hasRDMA {
		inv.RDMADevices = []hardware.RdmaDevice{
			{Name: "mlx5_0", Vendor: "mellanox", Ports: []hardware.RdmaPort{
				{Name: "mlx5_0", State: "active", LinkRateGbps: 200},
			}},
		}
	}
	return inv, nil
}

// Sample implements hardware.Driver with a deterministic, lightweight sample.
func (d *NVIDIADriver) Sample(ctx context.Context) (hardware.Telemetry, error) {
	t := hardware.Telemetry{
		CPUUsagePercent:  7,
		CPUCores:         10,
		MemoryUsedBytes:  4 << 30,
		MemoryTotalBytes: 128 << 30,
	}
	for _, g := range d.GPUs {
		t.Accelerators = append(t.Accelerators, hardware.AcceleratorTelemetry{
			Index:          g.Index,
			UtilizationPct: 3,
			MemUsedBytes:   512 << 20,
			MemTotalBytes:  uint64(g.MemoryBytes),
			TemperatureC:   42,
			PowerMW:        60000,
		})
	}
	return t, nil
}

// Validate implements hardware.Driver via the shared matcher.
func (d *NVIDIADriver) Validate(ctx context.Context, req hardware.Requirement) []hardware.Diagnostic {
	return hardware.MatchRequirement(d.GPUs, req)
}
