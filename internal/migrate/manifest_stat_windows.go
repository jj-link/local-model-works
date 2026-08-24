//go:build windows

package migrate

import "io/fs"

func mtimeNS(info fs.FileInfo) int64 { return info.ModTime().UnixNano() }
