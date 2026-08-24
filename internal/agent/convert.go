package agent

import (
	"github.com/jj-link/local-model-works/internal/hardware"
	agentv1 "github.com/jj-link/local-model-works/proto/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// toProtoInventory renders the hardware inventory for the wire.
func toProtoInventory(inv hardware.Inventory) *agentv1.Inventory {
	out := &agentv1.Inventory{
		Hostname:   inv.Hostname,
		Os:         inv.OS,
		Arch:       inv.Arch,
		PeerListen: inv.PeerListen,
		Docker: &agentv1.DockerInfo{
			Ok:         inv.Docker.OK,
			Version:    inv.Docker.Version,
			ApiVersion: inv.Docker.APIVersion,
			Error:      inv.Docker.Error,
		},
	}
	for _, a := range inv.Accelerators {
		out.Accelerators = append(out.Accelerators, &agentv1.Accelerator{
			Index:        int32(a.Index),
			Vendor:       a.Vendor,
			Architecture: a.Architecture,
			Name:         a.Name,
			Uuid:         a.UUID,
			MemoryBytes:  uint64(a.MemoryBytes),
			Features:     a.Features,
		})
	}
	for _, ni := range inv.Interfaces {
		out.Interfaces = append(out.Interfaces, &agentv1.NetworkInterface{
			Name:      ni.Name,
			Addresses: ni.Addresses,
			Mtu:       uint32(ni.MTU),
			LinkMbps:  uint32(ni.LinkMbps),
		})
	}
	for _, d := range inv.RDMADevices {
		p := &agentv1.RdmaDevice{Name: d.Name, Vendor: d.Vendor}
		for _, port := range d.Ports {
			p.Ports = append(p.Ports, &agentv1.RdmaPort{
				Name:         port.Name,
				State:        port.State,
				LinkRateGbps: uint32(port.LinkRateGbps),
			})
		}
		out.RdmaDevices = append(out.RdmaDevices, p)
	}
	for _, c := range inv.CacheRoots {
		out.CacheRoots = append(out.CacheRoots, &agentv1.CacheRoot{
			Path:         c.Path,
			Backend:      c.Backend,
			SizeBytes:    uint64(c.SizeBytes),
			Repositories: c.Repositories,
		})
	}
	return out
}

// toProtoTelemetry renders one telemetry sample for the wire.
func toProtoTelemetry(t hardware.Telemetry) *agentv1.Telemetry {
	out := &agentv1.Telemetry{
		At:            timestamppb.Now(),
		UptimeSeconds: t.UptimeSeconds,
		Cpu: &agentv1.CpuTelemetry{
			UsagePercent: t.CPUUsagePercent,
			Cores:        t.CPUCores,
			Load1:        t.Load1x100,
		},
		Memory: &agentv1.MemoryTelemetry{
			UsedBytes:     t.MemoryUsedBytes,
			TotalBytes:    t.MemoryTotalBytes,
			SwapUsedBytes: t.SwapUsedBytes,
		},
		Network: &agentv1.NetworkTelemetry{
			RxBytes:           t.NetRxBytes,
			TxBytes:           t.NetTxBytes,
			RxBytesPerSecond:  t.NetRxBytesPerSec,
			TxBytesPerSecond:  t.NetTxBytesPerSec,
		},
	}
	for _, fs := range t.Filesystems {
		out.Filesystems = append(out.Filesystems, &agentv1.FilesystemTelemetry{
			MountPath:  fs.MountPath,
			UsedBytes:  fs.UsedBytes,
			TotalBytes: fs.TotalBytes,
		})
	}
	for _, ni := range t.NetworkInterfaces {
		out.Network.Interfaces = append(out.Network.Interfaces, &agentv1.NetworkInterfaceTelemetry{
			Name:          ni.Name,
			RxBytesPerSecond: ni.RxBytesPerSec,
			TxBytesPerSecond: ni.TxBytesPerSec,
		})
	}
	for _, a := range t.Accelerators {
		at := &agentv1.AcceleratorTelemetry{
			Index:              int32(a.Index),
			UtilizationPercent: a.UtilizationPct,
			MemoryUsedBytes:    a.MemUsedBytes,
			MemoryTotalBytes:   a.MemTotalBytes,
			TemperatureC:       a.TemperatureC,
			PowerMw:            a.PowerMW,
			PowerLimitMw:       a.PowerLimitMW,
			ThrottleReasons:    a.ThrottleReasons,
		}
		for _, p := range a.Processes {
			at.Processes = append(at.Processes, &agentv1.AcceleratorProcess{
				Pid:               p.PID,
				Name:              p.Name,
				UsedGpuMemoryBytes: p.UsedGpuMem,
			})
		}
		out.Accelerators = append(out.Accelerators, at)
	}
	return out
}
