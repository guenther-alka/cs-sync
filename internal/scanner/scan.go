// Package scanner walks a sync root and builds an in-RAM model.Tree.
// See cs-sync.info section 6 (INITIAL SCAN) and section 4 (METADATA MODEL).
package scanner

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/guenther-alka/cs-sync/internal/model"
)

// ExcludedTopLevel is skipped at the sync root (state store, never synced).
const ExcludedTopLevel = ".backupdata"

// Scan walks root recursively and returns a Tree of relative paths.
// Folder ACLs are NOT read here (too expensive to call getfacl/nfs4_getfacl
// for every directory on every scan) -- callers that need current folder
// ACLs call acl.Read explicitly for directories that are new or changed.
func Scan(root string) (model.Tree, error) {
	tree := model.Tree{}
	root = filepath.Clean(root)

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// Do not abort the whole scan on one unreadable entry (permission
			// error on a single file/dir) -- log via caller, skip subtree.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil // root itself is not an entry
		}
		if rel == ExcludedTopLevel || strings.HasPrefix(rel, ExcludedTopLevel+"/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		fi, statErr := d.Info()
		if statErr != nil {
			return nil // skip unreadable entry, do not fail whole scan
		}

		e := model.Entry{Path: rel}

		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			e.Type = model.TypeSymlink
			target, lerr := os.Readlink(p)
			if lerr == nil {
				e.LinkTarget = target
			}
		case d.IsDir():
			e.Type = model.TypeDir
		default:
			e.Type = model.TypeFile
			e.Size = fi.Size()
		}

		e.MtimeNS = fi.ModTime().UnixNano()
		e.Mode = uint32(fi.Mode().Perm())
		e.Dev, e.Ino = devIno(fi)

		tree[rel] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tree, nil
}
