//go:build !windows

package remote

import (
	"io/fs"
	"syscall"
)

func devOfPlatform(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev)
	}
	return 0
}
