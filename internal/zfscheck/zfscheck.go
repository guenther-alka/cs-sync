// Package zfscheck implements the ZFS preconditions from cs-sync.info
// section 2: find the parent dataset of a folder, read acltype, and set
// aclinherit=passthrough.
package zfscheck

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
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
		return nil, fmt.Errorf("zfs list failed (is this a ZFS host? is 'zfs' in PATH?): %w", err)
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
// matching prefix of folderPath (i.e. the dataset that actually owns it).
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
		return Dataset{}, fmt.Errorf("no ZFS dataset found for %s -- is it on a ZFS mount?", folderPath)
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

// CheckAndPrepare implements section 2: read acltype (error+exit on "off"),
// and set aclinherit=passthrough. Returns the acltype ("posix" or "nfs4").
func CheckAndPrepare(folderPath string) (string, error) {
	ds, err := ParentDataset(folderPath)
	if err != nil {
		return "", err
	}
	acltype, err := GetProp(ds.Name, "acltype")
	if err != nil {
		return "", err
	}
	switch acltype {
	case "off":
		return "", fmt.Errorf("dataset %s has acltype=off -- cs-sync requires posixacl or nfsv4 (see cs-sync.info section 2)", ds.Name)
	case "posixacl", "posix":
		acltype = "posix"
	case "nfsv4", "nfs4":
		acltype = "nfs4"
	default:
		return "", fmt.Errorf("dataset %s has unexpected acltype=%s", ds.Name, acltype)
	}
	if err := SetProp(ds.Name, "aclinherit", "passthrough"); err != nil {
		return "", err
	}
	return acltype, nil
}
