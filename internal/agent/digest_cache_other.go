//go:build !linux

package agent

import "os"

func fileChangeTime(os.FileInfo) ([2]int64, bool) {
	return [2]int64{}, false
}
