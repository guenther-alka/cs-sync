//go:build windows

package scanner

import "io/fs"

// devIno: Windows does not expose dev+ino cheaply via os.FileInfo without
// an extra per-file OpenFile+GetFileInformationByHandle syscall. cs-sync
// runs primarily against Linux/illumos/FreeBSD ZFS hosts (see cs-sync.info
// section 2); on Windows, rename detection (section 8) is simply disabled
// (falls back to delete+copy) and this always returns 0,0.
func devIno(fi fs.FileInfo) (dev uint64, ino uint64) {
	return 0, 0
}
