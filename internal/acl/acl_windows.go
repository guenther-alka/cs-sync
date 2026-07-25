//go:build windows

// Windows ACL sync for cs-sync folder ACL propagation.
//
// Works on ZFS (OpenZFS on Windows, NFS4 ACLs) and NTFS/ReFS alike --
// all three filesystems on Windows use the Windows security model
// (DACL/SACL, stored as SDDL strings). We use PowerShell Get-Acl/Set-Acl
// with the SDDL representation, identical to server.pl's
// _acl_save_folders/_acl_restore_folders.
//
// acltype="nfs4" is returned by zfscheck for ALL Windows filesystems
// (ZFS, NTFS, ReFS) -- they all share the same Windows security model.
package acl

import (
	"fmt"
	"os/exec"
	"strings"
)

func readNFS4ACL(path string) (string, error) {
	out, err := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf("(Get-Acl -LiteralPath '%s').Sddl", escapePSPath(path)),
	).Output()
	if err != nil {
		return "", fmt.Errorf("acl: Get-Acl %s: %w", path, err)
	}
	sddl := strings.TrimSpace(string(out))
	if sddl == "" {
		return "", fmt.Errorf("acl: Get-Acl returned empty SDDL for %s", path)
	}
	return sddl, nil
}

func applyNFS4ACL(path, text string) error {
	if text == "" {
		return nil
	}
	script := fmt.Sprintf(
		"$a = Get-Acl -LiteralPath '%s'; $a.SetSecurityDescriptorSddlForm('%s'); Set-Acl -LiteralPath '%s' -AclObject $a",
		escapePSPath(path), escapePSQuote(text), escapePSPath(path),
	)
	out, err := exec.Command("powershell", "-NoProfile", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("acl: Set-Acl %s: %w -- %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func escapePSPath(p string) string  { return strings.ReplaceAll(p, "'", "''") }
func escapePSQuote(s string) string { return strings.ReplaceAll(s, "'", "''") }
