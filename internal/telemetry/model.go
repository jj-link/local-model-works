package telemetry

import (
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
)

// NodePayload is the typed full-raw node telemetry document stored at both the
// five-second and one-minute resolutions. Scalar gauges that are meaningful at
// zero (CPU utilization, memory, network rates, accelerator utilization) are
// emitted even when zero; capacity/power/temperature values that are zero are
// interpreted by the UI as unavailable. Filesystems, GPU processes, and
// throttle reasons are current-only and are stripped from minute rows and
// history responses.
type NodePayload struct {
	CPU           *CPUPayload          `json:"cpu,omitempty"`
	Memory        *MemoryPayload       `json:"memory,omitempty"`
	UptimeSeconds uint64               `json:"uptime_seconds"`
	Network       *NetworkPayload      `json:"network,omitempty"`
	Filesystems   []FilesystemPayload  `json:"filesystems,omitempty"`
	Accelerators  []AcceleratorPayload `json:"accelerators,omitempty"`
}

// CPUPayload is one CPU gauge sample.
type CPUPayload struct {
	UsagePercent uint32 `json:"usage_percent"`
	Cores        uint32 `json:"cores"`
	Load1        uint64 `json:"load1,omitempty"`
}

// MemoryPayload is one host memory gauge sample.
type MemoryPayload struct {
	UsedBytes      uint64 `json:"used_bytes"`
	TotalBytes     uint64 `json:"total_bytes"`
	SwapUsedBytes  uint64 `json:"swap_used_bytes,omitempty"`
	SwapTotalBytes uint64 `json:"swap_total_bytes,omitempty"`
	Swappiness     uint32 `json:"swappiness"`
}

// NetworkPayload is one aggregate network sample plus per-interface rates.
type NetworkPayload struct {
	RxBytes          uint64                    `json:"rx_bytes,omitempty"`
	TxBytes          uint64                    `json:"tx_bytes,omitempty"`
	RxBytesPerSecond uint64                    `json:"rx_bytes_per_second"`
	TxBytesPerSecond uint64                    `json:"tx_bytes_per_second"`
	Interfaces       []NetworkInterfacePayload `json:"interfaces,omitempty"`
}

// NetworkInterfacePayload is one interface's byte rates.
type NetworkInterfacePayload struct {
	Name             string `json:"name"`
	RxBytesPerSecond uint64 `json:"rx_bytes_per_second"`
	TxBytesPerSecond uint64 `json:"tx_bytes_per_second"`
}

// FilesystemPayload is one mounted filesystem's usage.
type FilesystemPayload struct {
	MountPath  string `json:"mount_path"`
	UsedBytes  uint64 `json:"used_bytes"`
	TotalBytes uint64 `json:"total_bytes"`
}

// AcceleratorPayload is one accelerator's gauge sample.
type AcceleratorPayload struct {
	Index              int                         `json:"index"`
	UtilizationPercent uint32                      `json:"utilization_percent"`
	MemoryUsedBytes    uint64                      `json:"memory_used_bytes"`
	MemoryTotalBytes   uint64                      `json:"memory_total_bytes"`
	TemperatureC       uint32                      `json:"temperature_c,omitempty"`
	PowerMW            uint32                      `json:"power_mw,omitempty"`
	PowerLimitMW       uint32                      `json:"power_limit_mw,omitempty"`
	ThrottleReasons    []string                    `json:"throttle_reasons,omitempty"`
	Processes          []AcceleratorProcessPayload `json:"processes,omitempty"`
}

// AcceleratorProcessPayload is one GPU process.
type AcceleratorProcessPayload struct {
	PID                int32  `json:"pid"`
	Name               string `json:"name,omitempty"`
	UsedGpuMemoryBytes uint64 `json:"used_gpu_memory_bytes,omitempty"`
}

// NodeSample is one persisted node sample.
type NodeSample struct {
	NodeID  string      `json:"node_id"`
	TS      int64       `json:"ts"`
	Payload NodePayload `json:"payload"`
}

// NodePayloadFromProto converts the agent wire telemetry into the typed
// store document. It excludes the redundant protobuf timestamp (the server
// buckets by received time) and preserves scalar zero values.
func NodePayloadFromProto(t *agentv1.Telemetry) NodePayload {
	p := NodePayload{UptimeSeconds: t.GetUptimeSeconds()}
	if c := t.GetCpu(); c != nil {
		p.CPU = &CPUPayload{
			UsagePercent: c.GetUsagePercent(),
			Cores:        c.GetCores(),
			Load1:        c.GetLoad1(),
		}
	}
	if m := t.GetMemory(); m != nil {
		p.Memory = &MemoryPayload{
			UsedBytes:      m.GetUsedBytes(),
			TotalBytes:     m.GetTotalBytes(),
			SwapUsedBytes:  m.GetSwapUsedBytes(),
			SwapTotalBytes: m.GetSwapTotalBytes(),
			Swappiness:     m.GetSwappiness(),
		}
	}
	if n := t.GetNetwork(); n != nil {
		np := &NetworkPayload{
			RxBytes:          n.GetRxBytes(),
			TxBytes:          n.GetTxBytes(),
			RxBytesPerSecond: n.GetRxBytesPerSecond(),
			TxBytesPerSecond: n.GetTxBytesPerSecond(),
		}
		for _, ni := range n.GetInterfaces() {
			np.Interfaces = append(np.Interfaces, NetworkInterfacePayload{
				Name:             ni.GetName(),
				RxBytesPerSecond: ni.GetRxBytesPerSecond(),
				TxBytesPerSecond: ni.GetTxBytesPerSecond(),
			})
		}
		p.Network = np
	}
	for _, fs := range t.GetFilesystems() {
		p.Filesystems = append(p.Filesystems, FilesystemPayload{
			MountPath:  fs.GetMountPath(),
			UsedBytes:  fs.GetUsedBytes(),
			TotalBytes: fs.GetTotalBytes(),
		})
	}
	for _, a := range t.GetAccelerators() {
		ap := AcceleratorPayload{
			Index:              int(a.GetIndex()),
			UtilizationPercent: a.GetUtilizationPercent(),
			MemoryUsedBytes:    a.GetMemoryUsedBytes(),
			MemoryTotalBytes:   a.GetMemoryTotalBytes(),
			TemperatureC:       a.GetTemperatureC(),
			PowerMW:            a.GetPowerMw(),
			PowerLimitMW:       a.GetPowerLimitMw(),
			ThrottleReasons:    a.GetThrottleReasons(),
		}
		for _, pr := range a.GetProcesses() {
			ap.Processes = append(ap.Processes, AcceleratorProcessPayload{
				PID:                pr.GetPid(),
				Name:               pr.GetName(),
				UsedGpuMemoryBytes: pr.GetUsedGpuMemoryBytes(),
			})
		}
		p.Accelerators = append(p.Accelerators, ap)
	}
	return p
}

// stripCurrentOnly drops current-state arrays that history never consumes.
func (p *NodePayload) stripCurrentOnly() {
	p.Filesystems = nil
	if p.Network != nil {
		p.Network.Interfaces = nil
	}
	for i := range p.Accelerators {
		p.Accelerators[i].ThrottleReasons = nil
		p.Accelerators[i].Processes = nil
	}
}

// ServingPayload is the typed serving-telemetry document for one deployment.
// Rate/ratio/latency/load values are nullable so the minute average remains
// representable when a metric is unsupported (null) rather than a valid zero.
// Cumulative counters and context length stay integers.
type ServingPayload struct {
	Available           bool     `json:"available"`
	Backend             string   `json:"backend"`
	ModelID             string   `json:"model_id,omitempty"`
	GenerationTPS       *float64 `json:"generation_tps"`
	PrefillTPS          *float64 `json:"prefill_tps"`
	RequestsRunning     int32    `json:"requests_running,omitempty"`
	RequestsWaiting     int32    `json:"requests_waiting,omitempty"`
	SlotsActive         int32    `json:"slots_active,omitempty"`
	SlotsTotal          int32    `json:"slots_total,omitempty"`
	KVCacheUsageRatio   *float64 `json:"kv_cache_usage_ratio"`
	PrefixCacheHitRatio *float64 `json:"prefix_cache_hit_ratio"`
	TTFTP95Seconds      *float64 `json:"ttft_p95_seconds"`
	E2EP95Seconds       *float64 `json:"e2e_p95_seconds"`
	ITLP95Seconds       *float64 `json:"itl_p95_seconds"`
	PreemptionsTotal    int64    `json:"preemptions_total,omitempty"`
	SpecAcceptanceRatio *float64 `json:"spec_acceptance_ratio"`
	ContextLength       int32    `json:"context_length,omitempty"`
	ErrorCode           *string  `json:"error_code"`
	Error               *string  `json:"error"`
}

// ServingSample is one persisted serving sample.
type ServingSample struct {
	DeploymentID string         `json:"deployment_id"`
	TS           int64          `json:"ts"`
	Payload      ServingPayload `json:"payload"`
}
