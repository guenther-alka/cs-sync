// Package acl reads and applies FOLDER ACLs, per cs-sync.info section 10.
// File ACLs are intentionally never handled here -- the concept relies on
// ACL inheritance (nfs4) or posix default ACLs, see section 10 decisions log.
//
// Windows: acltype is always "nfs4" for ALL Windows filesystems (ZFS, NTFS,
// ReFS) -- they all share the Windows security model (DACL/SDDL). The
// acl_windows.go build-tagged file provides Get-Acl/Set-Acl via PowerShell,
// identical to server.pl's _acl_save_folders/_acl_restore_folders.
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
	case "none":
		return "", nil // no-op: defensive, should not reach here
	default:
		return "", fmt.Errorf("acl: unknown acltype %q", acltype)
	}
}

// Apply sets the folder ACL of path from the canonical text form.
func Apply(path, acltype, text string) error {
	if text == "" {
		return nil
	}
	switch acltype {
	case TypePosix:
		return applyPosixACL(path, text)
	case TypeNFS4:
		return applyNFS4ACL(path, text)
	case "none":
		return nil
	default:
		return fmt.Errorf("acl: unknown acltype %q", acltype)
	}
}
