package apply

import (
	"fmt"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/rustfs"
)

// remoteObjectPath builds the object path for a given relpath under a
// RustFS-typed Secondary (v3.0: relative to roots.SecondaryRustFSTarget's
// bucket root, resolved by rustfs.CopyTo/CopyFrom/Delete themselves).
func remoteObjectPath(relPath string) string {
	return filepath.ToSlash(relPath)
}

// applyOneRustFS handles every reconcile.Op where Secondary is configured
// as SecondaryKind=="rustfs" (sync-2.1-design.info). Called from applyOne
// in executor.go, which has already confirmed this op touches Secondary.
func applyOneRustFS(op reconcile.Op, roots Roots, log Logf) error {
	switch op.Kind {
	case reconcile.OpCopy:
		if op.DstSide == reconcile.SideSecondary {
			// local (primary) file changed -> push to RustFS.
			// section 4.3: SMB-side change -> S3 PUT via rclone.
			local := filepath.Join(roots.Primary, filepath.FromSlash(op.Path))
			return rustfs.CopyTo(roots.SecondaryRustFSTarget, local, remoteObjectPath(op.Path))
		}
		// op.SrcSide == SideSecondary: RustFS object changed -> pull to
		// local (primary). section 4.3: S3 GET via rclone, written as a
		// normal local file -- this is also exactly what makes ACL
		// inheritance "just work" (section 10): a real open()/write() on
		// a ZFS dataset with aclinherit=passthrough, no special code
		// needed here at all.
		local := filepath.Join(roots.Primary, filepath.FromSlash(op.Path))
		return rustfs.CopyFrom(roots.SecondaryRustFSTarget, remoteObjectPath(op.Path), local)

	case reconcile.OpDelete:
		if op.DstSide == reconcile.SideSecondary {
			// File deleted on primary -> delete the RustFS object.
			return rustfs.Delete(roots.SecondaryRustFSTarget, remoteObjectPath(op.Path))
		}
		// A RustFS object was deleted -> reconcile already targets this
		// as an OpDelete with DstSide=primary in that case, which is a
		// plain local os.Remove -- NOT routed here at all (applyOne only
		// dispatches to this function when DstSide OR SrcSide is
		// Secondary; a delete FROM rustfs has DstSide=Primary,
		// SrcSide="" and is handled by the normal filesystem path in
		// executor.go). This branch is therefore unreachable in
		// practice; kept only so the switch is total and any future
		// caller change surfaces here loudly instead of silently
		// mis-routing.
		return fmt.Errorf("applyOneRustFS: unexpected OpDelete with DstSide=%s (want %s)", op.DstSide, reconcile.SideSecondary)

	case reconcile.OpRename, reconcile.OpConflictRename:
		// RustFS has no real rename (sync-2.1-design.info section 3.2/9,
		// live-verified via inode comparison: every "move" is a fresh
		// create + a separate delete, brand-new shard UUID, not a
		// preserved identity). detectRenames (reconcile/rename.go) can
		// still emit an OpRename targeting a rustfs Secondary when the
		// RENAME HAPPENED ON PRIMARY (real Dev+Ino there) -- RustFS-side
		// entries themselves are never rename SOURCES since package
		// rustfs's Scan leaves Ino/Dev at zero (see scan.go), but they
		// CAN be a rename's destination.
		//
		// The content only exists locally at the NEW path now (the
		// rename already happened on disk before this op runs) -- so
		// this is a copy-to-new-key + delete-old-key pair, matching
		// exactly what a real S3 client would do for a "move" (see
		// section 3.2's own rc-object-move finding: RustFS's own move
		// convenience command does precisely this internally too).
		if op.DstSide != reconcile.SideSecondary {
			return fmt.Errorf("applyOneRustFS: %v has DstSide=%s, want %s", op.Kind, op.DstSide, reconcile.SideSecondary)
		}
		local := filepath.Join(roots.Primary, filepath.FromSlash(op.Path))
		if err := rustfs.CopyTo(roots.SecondaryRustFSTarget, local, remoteObjectPath(op.Path)); err != nil {
			return fmt.Errorf("rename (copy new key %s): %w", op.Path, err)
		}
		if err := rustfs.Delete(roots.SecondaryRustFSTarget, remoteObjectPath(op.OldPath)); err != nil {
			// The new key is already safely in place -- log but don't
			// fail the whole op over a stale old key lingering on the
			// RustFS side (worse to lose the new content than to leave
			// one orphaned old object behind; a safety-net rescan will
			// eventually reconcile this since the old key will simply
			// look like an independent RustFS-side deletion candidate
			// once local no longer has an OldPath entry).
			log("WARN: rename %s -> %s: new key copied ok, but could not delete old key %s: %v", op.OldPath, op.Path, remoteObjectPath(op.OldPath), err)
		}
		return nil

	case reconcile.OpMkdir, reconcile.OpRmdir:
		// v1 simplification, documented not hidden (matches this
		// project's own convention, see e.g. internal/watch's package
		// doc for a precedent): S3 has no real "directory" as a
		// first-class object -- folders are implicit in object key
		// prefixes (a "documents/" folder exists only insofar as some
		// object has a key starting with "documents/"). There is
		// nothing meaningful to create or remove here for an empty
		// folder create/remove on the RustFS side; a genuinely empty
		// folder on the filesystem side simply has no corresponding
		// RustFS object at all, which is the correct end state already.
		// NOT YET HANDLED: OpRmdir for a NON-empty folder being removed
		// on primary should, in principle, delete every RustFS object
		// under that key prefix -- deliberately not implemented in this
		// first pass (would need an rclone purge/delete --recursive call
		// enumerating the prefix); flagged here rather than silently
		// left as a todo elsewhere.
		return nil

	default:
		return fmt.Errorf("applyOneRustFS: unhandled op kind %v", op.Kind)
	}
}
