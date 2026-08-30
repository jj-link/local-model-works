package agent

import (
	"testing"

	"github.com/jj-link/local-model-works/internal/hardware"
)

func TestToProtoTelemetryCarriesExtendedFields(t *testing.T) {
	tm := hardware.Telemetry{
		CPUUsagePercent:  12,
		CPUCores:         16,
		MemoryUsedBytes:  1 << 30,
		MemoryTotalBytes: 2 << 30,
		SwapUsedBytes:    3,
		SwapTotalBytes:   16,
		Swappiness:       60,
		UptimeSeconds:    99,
		NetRxBytes:       5,
		NetTxBytes:       6,
		NetRxBytesPerSec: 7,
		NetTxBytesPerSec: 8,
		Filesystems: []hardware.FilesystemTelemetry{
			{MountPath: "/", UsedBytes: 1, TotalBytes: 2},
		},
		NetworkInterfaces: []hardware.NetworkInterfaceTelemetry{
			{Name: "eth0", RxBytesPerSec: 10, TxBytesPerSec: 20},
		},
		Accelerators: []hardware.AcceleratorTelemetry{{
			Index:           0,
			UtilizationPct:  0,
			MemUsedBytes:    1,
			MemTotalBytes:   2,
			TemperatureC:    3,
			PowerMW:         4,
			PowerLimitMW:    5,
			ThrottleReasons: []string{"thermal"},
			Processes: []hardware.AcceleratorProcess{
				{PID: 42, Name: "python", UsedGpuMem: 9000},
			},
		}},
	}

	p := toProtoTelemetry(tm)
	if p.GetUptimeSeconds() != 99 {
		t.Fatalf("uptime=%d want 99", p.GetUptimeSeconds())
	}
	if p.GetMemory().GetSwapTotalBytes() != 16 || p.GetMemory().GetSwappiness() != 60 {
		t.Fatalf("memory: %+v", p.GetMemory())
	}
	if p.GetNetwork().GetRxBytesPerSecond() != 7 || p.GetNetwork().GetTxBytesPerSecond() != 8 {
		t.Fatalf("net rates: %+v", p.GetNetwork())
	}
	if n := p.GetNetwork().GetInterfaces(); len(n) != 1 || n[0].GetName() != "eth0" || n[0].GetRxBytesPerSecond() != 10 {
		t.Fatalf("ifaces: %+v", n)
	}
	if fs := p.GetFilesystems(); len(fs) != 1 || fs[0].GetMountPath() != "/" {
		t.Fatalf("filesystems: %+v", fs)
	}
	acc := p.GetAccelerators()[0]
	if acc.GetPowerLimitMw() != 5 || len(acc.GetThrottleReasons()) != 1 || acc.GetThrottleReasons()[0] != "thermal" {
		t.Fatalf("acc: %+v", acc)
	}
	proc := acc.GetProcesses()[0]
	if proc.GetPid() != 42 || proc.GetName() != "python" || proc.GetUsedGpuMemoryBytes() != 9000 {
		t.Fatalf("proc: %+v", proc)
	}
}

func TestToProtoTelemetryZeroValuesAndAbsentFields(t *testing.T) {
	// A minimal older-agent sample has no extended fields; conversion must not
	// invent them, and valid zero utilization/rates must remain present.
	tm := hardware.Telemetry{
		CPUUsagePercent:  0,
		CPUCores:         8,
		MemoryUsedBytes:  0,
		MemoryTotalBytes: 0,
		Accelerators: []hardware.AcceleratorTelemetry{{
			Index: 0, UtilizationPct: 0, MemUsedBytes: 0, MemTotalBytes: 0,
		}},
	}
	p := toProtoTelemetry(tm)
	if p.GetUptimeSeconds() != 0 || len(p.GetFilesystems()) != 0 {
		t.Fatalf("extended fields should be absent: %+v", p)
	}
	if p.GetAccelerators()[0].GetUtilizationPercent() != 0 || len(p.GetAccelerators()[0].GetProcesses()) != 0 {
		t.Fatalf("acc zero/absent wrong: %+v", p.GetAccelerators()[0])
	}
}
