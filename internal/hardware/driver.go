// Package hardware defines the vendor-neutral driver contract. Recipes match
// capability fields (vendor, architecture, memory, count, features), so
// adding AMD/Intel later means adding a driver, not changing recipe or API
// shapes.
package hardware

import (
	"context"
	"fmt"
)

// Requirement is a recipe's hardware ask for one node.
type Requirement struct {
	Vendor         string
	Architectures  []string
	Count          int
	MinMemoryBytes int64
	Features       []string
}

// Accelerator is one device.
type Accelerator struct {
	Index        int      `json:"index"`
	Vendor       string   `json:"vendor"`
	Architecture string   `json:"architecture,omitempty"`
	Name         string   `json:"name"`
	UUID         string   `json:"uuid"`
	MemoryBytes  int64    `json:"memory_bytes"`
	Features     []string `json:"features,omitempty"`
}

// NetworkInterface is one host interface.
type NetworkInterface struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses"`
	MTU       int      `json:"mtu,omitempty"`
	LinkMbps  int      `json:"link_mbps,omitempty"`
}

// RdmaPort is one port of an RDMA device.
type RdmaPort struct {
	Name         string `json:"name"`
	State        string `json:"state"`
	LinkRateGbps int    `json:"link_rate_gbps,omitempty"`
}

// RdmaDevice is one RDMA-capable device.
type RdmaDevice struct {
	Name   string     `json:"name"`
	Vendor string     `json:"vendor,omitempty"`
	Ports  []RdmaPort `json:"ports,omitempty"`
}

// CacheRoot is an existing model/cache root the agent reports.
type CacheRoot struct {
	Path         string   `json:"path"`
	Backend      string   `json:"backend"`
	SizeBytes    int64    `json:"size_bytes,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
}

// Inventory is the full node capability report.
type Inventory struct {
	Hostname     string             `json:"hostname"`
	OS           string             `json:"os"`
	Arch         string             `json:"arch"`
	Docker       DockerInfo         `json:"docker"`
	Accelerators []Accelerator      `json:"accelerators"`
	Interfaces   []NetworkInterface `json:"interfaces"`
	RDMADevices  []RdmaDevice       `json:"rdma_devices"`
	CacheRoots   []CacheRoot        `json:"cache_roots"`
}

// DockerInfo reports the container runtime state.
type DockerInfo struct {
	OK         bool   `json:"ok"`
	Version    string `json:"version,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Telemetry is one sample.
type Telemetry struct {
	CPUUsagePercent  uint32                 `json:"cpu_usage_percent"`
	CPUCores         uint32                 `json:"cpu_cores"`
	Load1x100        uint64                 `json:"load1_x100,omitempty"`
	MemoryUsedBytes  uint64                 `json:"memory_used_bytes"`
	MemoryTotalBytes uint64                 `json:"memory_total_bytes"`
	SwapUsedBytes    uint64                 `json:"swap_used_bytes,omitempty"`
	Accelerators     []AcceleratorTelemetry `json:"accelerators"`
	NetRxBytes       uint64                 `json:"net_rx_bytes,omitempty"`
	NetTxBytes       uint64                 `json:"net_tx_bytes,omitempty"`
}

// AcceleratorTelemetry is one device sample.
type AcceleratorTelemetry struct {
	Index          int    `json:"index"`
	UtilizationPct uint32 `json:"utilization_percent"`
	MemUsedBytes   uint64 `json:"memory_used_bytes"`
	MemTotalBytes  uint64 `json:"memory_total_bytes"`
	TemperatureC   uint32 `json:"temperature_c,omitempty"`
	PowerMW        uint32 `json:"power_mw,omitempty"`
}

// Diagnostic is a hardware validation finding.
type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
}

func warn(code, msg, res string) Diagnostic {
	return Diagnostic{Code: code, Severity: "warning", Message: msg, Resource: res}
}

func fail(code, msg, res string) Diagnostic {
	return Diagnostic{Code: code, Severity: "error", Message: msg, Resource: res}
}

// Driver is the hardware abstraction a node exposes.
type Driver interface {
	// ID returns the driver identifier, e.g. "nvidia".
	ID() string
	// Probe reports static inventory.
	Probe(ctx context.Context) (Inventory, error)
	// Sample reports live telemetry.
	Sample(ctx context.Context) (Telemetry, error)
	// Validate checks a per-node requirement against live state.
	Validate(ctx context.Context, req Requirement) []Diagnostic
}

// Register records drivers available on this node; the agent composes them.
var (
	// ErrNoAccelerator is the stable code for a node without matching GPU.
	ErrNoAccelerator = "node.accelerator-missing"
	// ErrDockerMissing is the stable code for an unusable container runtime.
	ErrDockerMissing = "node.docker-missing"
)

// MatchRequirement checks one node's accelerators against a requirement.
func MatchRequirement(accs []Accelerator, req Requirement) []Diagnostic {
	if req.Count > 0 {
		var matched []Accelerator
		for _, a := range accs {
			if req.Vendor != "" && a.Vendor != req.Vendor {
				continue
			}
			if len(req.Architectures) > 0 && !contains(req.Architectures, a.Architecture) {
				continue
			}
			if req.MinMemoryBytes > 0 && a.MemoryBytes < req.MinMemoryBytes {
				continue
			}
			ok := true
			for _, f := range req.Features {
				if !contains(a.Features, f) {
					ok = false
					break
				}
			}
			if ok {
				matched = append(matched, a)
			}
		}
		if len(matched) < req.Count {
			return []Diagnostic{fail(ErrNoAccelerator, fmt.Sprintf("need %d matching accelerators, have %d", req.Count, len(matched)), "accelerators")}
		}
		return nil
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
