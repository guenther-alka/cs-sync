//go:build !windows

package scanner

import (
	"io/fs"
	"syscall"
)

// devIno extracts device+inode from a FileInfo on unix-like systems
// (linux, freebsd, illumos, solaris, darwin). Used for rename detection
// (cs-sync.info section 8).
func devIno(fi fs.FileInfo) (dev uint64, ino uint64) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), uint64(st.Ino)
	}
	return 0, 0
}
