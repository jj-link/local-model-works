//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// InspectStorage reports available blocks on the actual destination filesystem.
// A missing child is resolved through its existing parent, never another root.
func InspectStorage(destination string) (*ImageStorageInfo, error) {
	if !filepath.IsAbs(destination) {
		return nil, fmt.Errorf("storage.destination_invalid")
	}
	parent := filepath.Clean(destination)
	for {
		_, err := os.Stat(parent)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) || parent == filepath.Dir(parent) {
			return nil, err
		}
		parent = filepath.Dir(parent)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(parent, &fs); err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err := syscall.Stat(parent, &st); err != nil {
		return nil, err
	}
	return &ImageStorageInfo{Root: destination, Filesystem: fmt.Sprintf("dev:%d", st.Dev), TotalBytes: fs.Blocks * uint64(fs.Bsize), FreeBytes: fs.Bavail * uint64(fs.Bsize)}, nil
}
