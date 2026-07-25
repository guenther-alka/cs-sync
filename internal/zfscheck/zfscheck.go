// Package zfscheck implements the ZFS preconditions from cs-sync.info
// section 2: find the parent dataset of a folder, read acltype, and set
// aclinherit=passthrough.
//
// WINDOWS: always returns "nfs4", regardless of filesystem (ZFS, NTFS,
// ReFS). All Windows filesystems use the Windows security model (DACL/
// SDDL); the acl package's Windows implementation uses Get-Acl/Set-Acl
// which works identically on all three. "nfs4" is the type token that
// routes through the Windows ACL path in the acl package.
package zfscheck

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Dataset struct {
	Name       string
	Mountpoint string
}

// listDatasets runs `zfs list -H -o name,mountpoint -t filesystem` once.
func listDatasets() ([]Dataset, error) {
	out, err := exec.Command("zfs", "list", "-H", "-o", "name,mountpoint", "-t", "filesystem").Output()
	if err != nil {
		return nil, fmt.Errorf("zfs list failed: %w", err)
	}
	var ds []Dataset
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 2 {
			continue
		}
		ds = append(ds, Dataset{Name: f[0], Mountpoint: f[1]})
	}
	return ds, nil
}

// ParentDataset finds the ZFS dataset whose mountpoint is the longest
// matching prefix of folderPath. Returns ("", nil) if no dataset matches
// (non-ZFS path -- caller decides whether that's an error).
func ParentDataset(folderPath string) (Dataset, error) {
	abs, err := filepath.Abs(folderPath)
	if err != nil {
		return Dataset{}, err
	}
	ds, err := listDatasets()
	if err != nil {
		return Dataset{}, err
	}
	var best Dataset
	bestLen := -1
	for _, d := range ds {
		if d.Mountpoint == "-" || d.Mountpoint == "none" {
			continue
		}
		mp := filepath.Clean(d.Mountpoint)
		if abs == mp || strings.HasPrefix(abs, mp+string(filepath.Separator)) {
			if len(mp) > bestLen {
				bestLen = len(mp)
				best = d
			}
		}
	}
	if bestLen < 0 {
		return Dataset{}, nil // not on ZFS -- not an error, caller gets ""
	}
	return best, nil
}

// GetProp reads a single ZFS property value.
func GetProp(dataset, prop string) (string, error) {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", prop, dataset).Output()
	if err != nil {
		return "", fmt.Errorf("zfs get %s %s: %w", prop, dataset, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// SetProp sets a single ZFS property value.
func SetProp(dataset, prop, value string) error {
	if err := exec.Command("zfs", "set", prop+"="+value, dataset).Run(); err != nil {
		return fmt.Errorf("zfs set %s=%s %s: %w", prop, value, dataset, err)
	}
	return nil
}

// CheckAndPrepare implements section 2: find the parent ZFS dataset, read
// acltype, set aclinherit=passthrough. Returns:
//   - "posix"  -- Linux ZFS with acltype=posixacl
//   - "nfs4"   -- illumos/Solaris/FreeBSD/OpenZFS-on-Windows (always NFSv4)
//                 AND Windows NTFS/ReFS (same Windows security model/SDDL)
//   - "none"   -- not returned on any platform (reserved)
//
// Platform notes:
//
// illumos/Solaris: no "acltype" property exists -- hardcode nfs4.
//
// Windows (OpenZFS on Windows + NTFS/ReFS): always "nfs4". All Windows
// filesystems share the Windows security model; Get-Acl/Set-Acl works
// identically on ZFS, NTFS, and ReFS.
//
// FreeBSD: acltype=posixacl or nfsv4 on ZFS datasets.
// NOTE: FreeBSD is treated like illumos -- always returns "nfs4".
// zfs get acltype may report "posixacl" on FreeBSD but the actual ACL
// tools are nfs4_getfacl/nfs4_setfacl; the property is a compat label.
func CheckAndPrepare(folderPath string) (string, error) {
	isIllumos := runtime.GOOS == "illumos" || runtime.GOOS == "solaris" || runtime.GOOS == "freebsd"

	ds, err := ParentDataset(folderPath)
	if err != nil {
		// zfs binary missing or not in PATH
		if runtime.GOOS == "windows" {
			// No ZFS, but Windows ACL sync (Get-Acl/Set-Acl) still works.
			return "nfs4", nil
		}
		return "", err
	}

	// No ZFS dataset found for this path.
	if ds.Name == "" {
		if runtime.GOOS == "windows" {
			// NTFS/ReFS: Windows ACL model works on all Windows filesystems.
			return "nfs4", nil
		}
		return "", fmt.Errorf("no ZFS dataset found for %s -- is it on a ZFS mount?", folderPath)
	}

	acltype := "nfs4"
	if !isIllumos {
		raw, err := GetProp(ds.Name, "acltype")
		if err != nil {
			// OpenZFS on Windows may not support "acltype" on older builds.
			if runtime.GOOS == "windows" {
				acltype = "nfs4"
			} else {
				return "", err
			}
		} else {
			switch raw {
			case "off":
				return "", fmt.Errorf("dataset %s has acltype=off -- cs-sync requires posixacl or nfsv4 (see cs-sync.info section 2)", ds.Name)
			case "posixacl", "posix":
				acltype = "posix"
			case "nfsv4", "nfs4":
				acltype = "nfs4"
			default:
				return "", fmt.Errorf("dataset %s has unexpected acltype=%s", ds.Name, raw)
			}
		}
	}

	// aclinherit=passthrough: best-effort, non-fatal on failure.
	// OpenZFS-on-Windows older builds may not support this property.
	_ = SetProp(ds.Name, "aclinherit", "passthrough")
	return acltype, nil
}
