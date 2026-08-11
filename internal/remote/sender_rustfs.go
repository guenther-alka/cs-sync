package remote

import (
	"fmt"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/rustfs"
)

// sendOpRustFS is sendOp's counterpart when s.RustFSTarget is set --
// backup target is a RustFS bucket (sync-2.1-design.info section 5/11,
// v3.0 flag: --backup as a RustFS-kind endpoint). Called from sendOp,
// which has already dispatched here based on RustFSTarget being set;
// every caller in Pass (guard checks, delete budget, retry/quarantine,
// baseline tracking, create-before-delete ordering) is completely
// unchanged and unaware the transport differs -- this function is the
// ONLY thing that's different between a wire-protocol Backup and an
// rclone/RustFS Backup.
//
// Unlike executor_rustfs.go's applyOneRustFS (used for the LOCAL bidir
// Secondary relationship, which has no retry queue of its own -- Apply
// just logs and moves on), THIS function returns errors for genuine
// failures rather than swallowing them: Sender's retry/quarantine
// machinery (guard.RetryState, see Pass) already expects exactly that
// contract from sendOp, and reusing it here (rather than inventing a
// second, different error-handling philosophy) is both less code and
// more consistent with how every other kind of Backup failure already
// behaves (temporary network blip, receiver down, disk full, ...).
func (s *Sender) sendOpRustFS(op reconcile.Op, primaryTree model.Tree) error {
	objPath := func(relPath string) string { return filepath.ToSlash(relPath) }

	switch op.Kind {
	case reconcile.OpMkdir, reconcile.OpRmdir:
		// v1 simplification, same as executor_rustfs.go: S3 has no real
		// "directory" as a first-class object, folders are implicit in
		// object key prefixes. Nothing to do for either op.
		return nil

	case reconcile.OpDelete:
		return rustfs.Delete(s.RustFSTarget, objPath(op.Path))

	case reconcile.OpCopy:
		e := primaryTree[op.Path]
		if e.Type == model.TypeSymlink {
			return fmt.Errorf("cannot back up symlink %s to RustFS: S3 has no symlink concept", op.Path)
		}
		local := filepath.Join(s.Primary, filepath.FromSlash(op.Path))
		return rustfs.CopyTo(s.RustFSTarget, local, objPath(op.Path))

	case reconcile.OpRename:
		// RustFS has no real rename (sync-2.1-design.info section 3.2/9,
		// live-verified via inode comparison) -- copy new key + delete
		// old key, same reasoning as executor_rustfs.go. Unlike that
		// function (which logs-and-continues on the delete half so a
		// stale local bidir op doesn't fail the whole reconcile pass),
		// here BOTH halves report their real error: if the delete fails,
		// returning it lets this op re-enter the normal retry queue,
		// where a retry harmlessly re-copies (rclone overwrite, cheap)
		// and re-attempts the delete -- correct, idempotent retry
		// semantics already built into Pass, no special-casing needed.
		e := primaryTree[op.Path]
		if e.Type == model.TypeDir {
			return nil // dirs are implicit on the RustFS side, nothing to move
		}
		local := filepath.Join(s.Primary, filepath.FromSlash(op.Path))
		if err := rustfs.CopyTo(s.RustFSTarget, local, objPath(op.Path)); err != nil {
			return fmt.Errorf("rename (copy new key %s): %w", op.Path, err)
		}
		return rustfs.Delete(s.RustFSTarget, objPath(op.OldPath))

	case reconcile.OpConflictRename:
		// cannot happen in one-way mode (baseline == secondary tree, see
		// Pass's own comment on this exact case for the wire-protocol
		// path) -- defensive no-op, matching sendOp's existing behavior.
		return nil
	}
	return fmt.Errorf("sendOpRustFS: unknown op kind %d", op.Kind)
}
