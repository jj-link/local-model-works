//go:build !linux

package runtime

import (
	"fmt"
	"os"
)

func InspectStorage(destination string) (*ImageStorageInfo, error) {
	return nil, fmt.Errorf("storage.observation_unsupported")
}

func logFileIdentity(info os.FileInfo) string {
	// Upstream host execution is Linux-only; the content anchor still protects
	// truncation if a retained file is inspected on another platform.
	return ""
}
