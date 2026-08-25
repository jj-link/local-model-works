//go:build linux

package hardware

import (
	"os"
	"sort"
	"syscall"
)

// SampleFilesystems returns usage for the given paths, deduplicating paths
// that resolve to the same device while preferring "/" (then the shorter
// path). Missing or unreadable paths are omitted. Used bytes count allocated
// blocks (Blocks - Bfree), matching df's used column; reserved root blocks
// are included in used.
func SampleFilesystems(paths []string) []FilesystemTelemetry {
	preferred := map[uint64]string{}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		stt, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		dev := uint64(stt.Dev)
		if prev, ok := preferred[dev]; ok {
			if pick := preferPath(prev, p); pick != prev {
				preferred[dev] = pick
			}
		} else {
			preferred[dev] = p
		}
	}

	out := make([]FilesystemTelemetry, 0, len(preferred))
	for _, p := range preferred {
		var fs syscall.Statfs_t
		if err := syscall.Statfs(p, &fs); err != nil {
			continue
		}
		blockSize := uint64(fs.Bsize)
		out = append(out, FilesystemTelemetry{
			MountPath:  p,
			TotalBytes: uint64(fs.Blocks) * blockSize,
			UsedBytes:  (uint64(fs.Blocks) - uint64(fs.Bfree)) * blockSize,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MountPath < out[j].MountPath })
	return out
}

// preferPath picks a representative path for a device across duplicate
// inputs: "/" always wins, otherwise the shorter path.
func preferPath(a, b string) string {
	if a == "/" {
		return a
	}
	if b == "/" {
		return b
	}
	if len(a) <= len(b) {
		return a
	}
	return b
}
