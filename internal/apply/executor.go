// Package apply executes reconcile.Op operations against the filesystem,
// per cs-sync.info section 8 (COPY EXECUTION -- CRASH SAFE).
package apply

import (
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/acl"
	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
)

// Roots maps side name -> filesystem root path.
type Roots struct {
	Primary   string
	Secondary string
	AclType   string // "posix" | "nfs4", from zfscheck (section 2)

	// PrimaryBootstrapACL is used ONLY when creating a directory ON
	// PRIMARY (section 6, first-sync case b: secondary was filled by
	// RustFS replication, primary starts empty). Normally primary is the
	// ACL source of truth and nothing creates folders on primary FROM
	// secondary; this bootstrap map (relpath -> acl text, loaded from
	// secondary/.backupdata/acl.csv at startup) is the one exception.
	PrimaryBootstrapACL map[string]string
}

func (r Roots) root(side string) string {
	if side == reconcile.SidePrimary {
		return r.Primary
	}
	return r.Secondary
}

// Logf is a minimal logging hook, injected by the caller (see internal/logging).
type Logf func(format string, args ...any)

// Apply executes ops in order. Mkdir/Copy/Rename ops must come before
// Delete/Rmdir of unrelated paths is not required (order doesn't matter
// across different paths), but WITHIN a single reconcile pass the caller
// (cmd) must ensure creates run before deletes are not entangled -- see
// reconcile.Reconcile's path sort (ascending) already puts parent-before-
// child for creates; Apply here processes ops in the slice order handed
// to it, and the caller is responsible for passing creates before deletes
// when they interact (mkdir/copy pass, then delete/rmdir pass, reversed).
func Apply(ops []reconcile.Op, roots Roots, log Logf) []error {
	var errs []error
	for _, op := range ops {
		if err := applyOne(op, roots, log); err != nil {
			errs = append(errs, fmt.Errorf("%v: %w", op, err))
			log("ERROR op=%v path=%s: %v", op.Kind, op.Path, err)
		}
	}
	return errs
}

func applyOne(op reconcile.Op, roots Roots, log Logf) error {
	dstRoot := roots.root(op.DstSide)

	switch op.Kind {
	case reconcile.OpMkdir:
		return mkdirWithACL(dstRoot, op.Path, roots, log)

	case reconcile.OpRmdir:
		return os.RemoveAll(filepath.Join(dstRoot, filepath.FromSlash(op.Path)))

	case reconcile.OpDelete:
		err := os.Remove(filepath.Join(dstRoot, filepath.FromSlash(op.Path)))
		if os.IsNotExist(err) {
			return nil
		}
		return err

	case reconcile.OpRename:
		oldp := filepath.Join(dstRoot, filepath.FromSlash(op.OldPath))
		newp := filepath.Join(dstRoot, filepath.FromSlash(op.Path))
		if err := os.MkdirAll(filepath.Dir(newp), 0755); err != nil {
			return err
		}
		return os.Rename(oldp, newp)

	case reconcile.OpConflictRename:
		oldp := filepath.Join(dstRoot, filepath.FromSlash(op.OldPath))
		newp := filepath.Join(dstRoot, filepath.FromSlash(op.Path))
		log("CONFLICT: preserving %s as %s (side=%s)", op.OldPath, op.Path, op.DstSide)
		return os.Rename(oldp, newp)

	case reconcile.OpCopy:
		srcRoot := roots.root(op.SrcSide)
		return copyEntry(srcRoot, dstRoot, op.Path, op.Type)
	}
	return fmt.Errorf("unknown op kind %v", op.Kind)
}

// mkdirWithACL creates a directory and applies the FOLDER ACL rule from
// section 10: the new folder gets the ACL of its parent folder as
// currently recorded on PRIMARY (primary is always the ACL source of
// truth, independent of which side the create is destined for -- Gea
// decision 2026.07.23, section 10).
func mkdirWithACL(dstRoot, relPath string, roots Roots, log Logf) error {
	dst := filepath.Join(dstRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	if roots.AclType == "" {
		return nil // ACL type unknown (e.g. dry-run/scan mode) -- skip
	}

	if dstRoot == roots.Primary {
		// bootstrap case (section 6b): source is acl.csv, not a live
		// primary-side parent read (primary may still be empty).
		if text, ok := roots.PrimaryBootstrapACL[relPath]; ok && text != "" {
			if err := acl.Apply(dst, roots.AclType, text); err != nil {
				log("WARN: could not apply bootstrap ACL to %s: %v", relPath, err)
			}
		}
		return nil
	}

	parentRel := filepath.ToSlash(filepath.Dir(relPath))
	parentOnPrimary := filepath.Join(roots.Primary, filepath.FromSlash(parentRel))
	if parentRel == "." {
		parentOnPrimary = roots.Primary
	}
	text, err := acl.Read(parentOnPrimary, roots.AclType)
	if err != nil {
		log("WARN: could not read parent ACL for %s: %v", relPath, err)
		return nil // do not fail the whole sync over an ACL read issue
	}
	if err := acl.Apply(dst, roots.AclType, text); err != nil {
		log("WARN: could not apply ACL to %s: %v", relPath, err)
	}
	return nil
}

// copyEntry copies a file or recreates a symlink, crash-safe (temp file +
// atomic rename), per section 8.
func copyEntry(srcRoot, dstRoot, relPath, entryType string) error {
	src := filepath.Join(srcRoot, filepath.FromSlash(relPath))
	dst := filepath.Join(dstRoot, filepath.FromSlash(relPath))

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	if entryType == model.TypeSymlink {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst) // symlink target replace: remove-then-create (no atomic symlink replace on all OSes)
		return os.Symlink(target, dst)
	}

	tmp := fmt.Sprintf("%s.cs-sync.tmp.%d", dst, rand.Int63())
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	fi, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chtimes(tmp, fi.ModTime(), fi.ModTime()); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
