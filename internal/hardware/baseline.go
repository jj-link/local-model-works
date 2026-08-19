package hardware

import (
	"context"
	"os"
	"runtime"
)

// HostBaseline returns an inventory with hostname, OS, architecture, host
// network interfaces, and RDMA devices populated. Accelerators, Docker
// state, and cache roots are filled in by the caller (the node agent).
// It never fails: a missing /proc or sysfs degrades the affected fields.
func HostBaseline(ctx context.Context) Inventory {
	var inv Inventory
	if h, err := os.Hostname(); err == nil {
		inv.Hostname = h
	}
	inv.OS = runtime.GOOS
	inv.Arch = runtime.GOARCH
	_ = ProbeNetwork(&inv)
	return inv
}
