//go:build !linux

package runtime

import "fmt"

func InspectStorage(destination string) (*ImageStorageInfo, error) {
	return nil, fmt.Errorf("storage.observation_unsupported")
}
