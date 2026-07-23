// Package acl reads and applies FOLDER ACLs, per cs-sync.info section 10.
// File ACLs are intentionally never handled here -- the concept relies on
// ACL inheritance (nfs4) or posix default ACLs, see section 10 decisions log.
package acl

import "fmt"

const (
	TypePosix = "posix"
	TypeNFS4  = "nfs4"
)

// Read returns the folder ACL of path in the canonical text form cs-sync
// stores in acl.csv and in the in-RAM baseline (section 4/5/10).
func Read(path, acltype string) (string, error) {
	switch acltype {
	case TypePosix:
		return readPosixACL(path)
	case TypeNFS4:
		return readNFS4ACL(path)
	default:
		return "", fmt.Errorf("acl: unknown acltype %q", acltype)
	}
}

// Apply sets the folder ACL of path from the canonical text form.
func Apply(path, acltype, text string) error {
	if text == "" {
		return nil // nothing captured (e.g. first run before any read) -- skip
	}
	switch acltype {
	case TypePosix:
		return applyPosixACL(path, text)
	case TypeNFS4:
		return applyNFS4ACL(path, text)
	default:
		return fmt.Errorf("acl: unknown acltype %q", acltype)
	}
}
