//go:build !linux && !freebsd && !illumos && !solaris

package acl

import "fmt"

func readNFS4ACL(path string) (string, error) {
	return "", fmt.Errorf("acl: nfs4 acltype not supported on this OS")
}

func applyNFS4ACL(path, text string) error {
	return fmt.Errorf("acl: nfs4 acltype not supported on this OS")
}
