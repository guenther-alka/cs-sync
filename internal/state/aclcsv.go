package state

import (
	"encoding/csv"
	"os"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/model"
)

const AclCsvName = "acl.csv"

// WriteACLCSV writes .backupdata/acl.csv: one line per directory,
// "relpath";"acltype";"acl text" (cs-sync.info section 10). rootACL is
// the ACL of the sync root itself (e.g. the ZFS dataset mountpoint, like
// tank/data) -- stored under the special relpath "." since it is never a
// Tree entry (the root is never created/deleted by cs-sync, only its
// children are). The root ACL is the default inheritance source for any
// new top-level file/folder and must be captured/restored just like any
// other folder's ACL (Gea, 2026.07.23).
func WriteACLCSV(root string, tree model.Tree, acltype string, rootACL string) error {
	dir, err := Dir(root)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, AclCsvName)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Comma = ';'
	if rootACL != "" {
		if err := w.Write([]string{".", acltype, rootACL}); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	for _, e := range tree {
		if e.Type != model.TypeDir {
			continue
		}
		if err := w.Write([]string{e.Path, acltype, e.ACL}); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}

// ReadACLCSV reads .backupdata/acl.csv into relpath -> acl text.
// Used when secondary was filled by RustFS replication and primary is
// empty (cs-sync.info section 6, first-sync case b).
func ReadACLCSV(root string) (map[string]string, error) {
	dir, err := Dir(root)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, AclCsvName)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.Comma = ';'
	r.FieldsPerRecord = 3
	out := map[string]string{}
	for {
		rec, err := r.Read()
		if err != nil {
			break // EOF or malformed -- stop, return what we have
		}
		out[rec[0]] = rec[2]
	}
	return out, nil
}

// MirrorACLCSV copies primary's acl.csv into secondary/.backupdata/acl.csv
// so it travels along with secondary's data (e.g. via RustFS bucket
// replication to a remote site, cs-sync.info section 11).
func MirrorACLCSV(primaryRoot, secondaryRoot string) error {
	srcDir, err := Dir(primaryRoot)
	if err != nil {
		return err
	}
	dstDir, err := Dir(secondaryRoot)
	if err != nil {
		return err
	}
	src := filepath.Join(srcDir, AclCsvName)
	data, err := os.ReadFile(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	dst := filepath.Join(dstDir, AclCsvName)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
