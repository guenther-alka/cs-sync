// Package zfscheck implements the ZFS preconditions from cs-sync.info
// section 2: find the parent dataset of a folder, read acltype, and set
// aclinherit=passthrough.
//
// WINDOWS: always returns "nfs4", regardless of filesystem (ZFS, NTFS,
// ReFS). All Windows filesystems use the Windows security model (DACL/
// SDDL); the acl package's Windows implementation uses Get-Acl/Set-Acl
// which works identically on all three. "nfs4" is the type token that
// routes through the Windows ACL path in the acl package.
//
// MACOS: returns "none" if ZFS is not installed (APFS/HFS+): file sync
// only, no ACL propagation. If OpenZFS for Mac is installed and the path
// is on a ZFS dataset, normal nfs4 ACL sync applies.
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
// matching prefix of folderPath. Returns ("", nil) if no dataset matches.
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
		return Dataset{}, nil
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

// CheckAndPrepare implements section 2. Returns:
//   - "posix"  -- Linux ZFS with acltype=posixacl
//   - "nfs4"   -- illumos/Solaris/FreeBSD/OpenZFS-on-Windows (always NFSv4)
//                 AND Windows NTFS/ReFS (same Windows security model/SDDL)
//   - "none"   -- macOS APFS/HFS+ without ZFS: file sync only, no ACL sync
//
// Platform notes:
//
// illumos/Solaris/FreeBSD: no "acltype" read -- hardcode nfs4.
// FreeBSD: zfs get acltype may return "posixacl" but the actual ACL
// tools are nfs4_getfacl/nfs4_setfacl (or getfacl/setfacl for nfsv4 datasets);
// "posixacl" is a compat label. Treating as nfs4 is correct.
//
// Windows (OpenZFS on Windows + NTFS/ReFS): always "nfs4". All Windows
// filesystems share the Windows security model; Get-Acl/Set-Acl works
// identically on ZFS, NTFS, and ReFS.
//
// macOS: if ZFS not installed, returns "none" (APFS/HFS+ file sync only).
// If OpenZFS for Mac is installed and path is on ZFS, normal nfs4 applies.
func CheckAndPrepare(folderPath string) (string, error) {
	// BUG FIX 2026.07.28 (audit finding): Windows was only forced to
	// "nfs4" in the "no dataset found" and "GetProp errored" branches --
	// if `zfs get acltype` on a real OpenZFS-on-Windows dataset SUCCEEDED
	// and returned "posixacl" (a plausible compat label: this exact
	// pattern is already documented and confirmed live for FreeBSD a few
	// lines below, where OpenZFS reports "posixacl" for what is actually
	// NFSv4), CheckAndPrepare would set acltype="posix" for a Windows
	// host. But acl_posix_other.go (built for any non-linux GOOS,
	// including windows) has NO real implementation -- it unconditionally
	// returns "posix acltype not supported on this OS". Every single ACL
	// read/apply on that host would then hard-fail, silently breaking
	// ACL sync (folders just keep default/parent-inherited permissions,
	// logged as repeated WARN lines, never fixed) despite the package's
	// own doc comment promising "Windows: always nfs4, regardless of
	// filesystem". Same risk exists for acltype=off (previously a hard
	// FATAL precondition failure on Windows) or any other unexpected
	// string a given OpenZFS-on-Windows build might report. Fix: treat
	// windows exactly like illumos/solaris/freebsd -- skip the live
	// GetProp("acltype") call entirely and hardcode "nfs4" per the
	// function's own documented, unconditional design intent.
	skipLiveAcltypeCheck := runtime.GOOS == "illumos" || runtime.GOOS == "solaris" || runtime.GOOS == "freebsd" || runtime.GOOS == "windows"

	ds, err := ParentDataset(folderPath)
	if err != nil {
		switch runtime.GOOS {
		case "windows":
			return "nfs4", nil
		case "darwin":
			return "none", nil
		}
		return "", err
	}

	if ds.Name == "" {
		switch runtime.GOOS {
		case "windows":
			return "nfs4", nil
		case "darwin":
			return "none", nil
		default:
			return "", fmt.Errorf("no ZFS dataset found for %s -- is it on a ZFS mount?", folderPath)
		}
	}

	acltype := "nfs4"
	if !skipLiveAcltypeCheck {
		raw, err := GetProp(ds.Name, "acltype")
		if err != nil {
			return "", err
		}
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

	_ = SetProp(ds.Name, "aclinherit", "passthrough")
	return acltype, nil
}
