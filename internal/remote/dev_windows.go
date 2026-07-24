//go:build windows

package remote

import "io/fs"

// Windows has no cheap dev id; the section 5a liveness check is disabled
// there (no ZFS acltype on Windows anyway, cs-sync.info section 2).
func devOfPlatform(fi fs.FileInfo) uint64 { return 0 }
