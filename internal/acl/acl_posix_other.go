//go:build !linux

package acl

import "fmt"

// posixacl (ZFS) is a Linux-only compatibility layer; FreeBSD/illumos/
// Solaris ZFS only support acltype=off|nfsv4 (see cs-sync.info section 2).
func readPosixACL(path string) (string, error) {
	return "", fmt.Errorf("acl: posix acltype not supported on this OS")
}

func applyPosixACL(path, text string) error {
	return fmt.Errorf("acl: posix acltype not supported on this OS")
}
