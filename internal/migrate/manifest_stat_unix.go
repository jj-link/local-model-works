//go:build !windows

package migrate

import (
	"io/fs"
	"syscall"
)

func mtimeNS(info fs.FileInfo) int64 {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return stat.Mtim.Nano()
	}
	return info.ModTime().UnixNano()
}
