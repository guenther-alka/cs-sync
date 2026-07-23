package reconcile

import "github.com/guenther-alka/cs-sync/internal/model"

type devIno struct {
	dev uint64
	ino uint64
}

// detectRenames implements cs-sync.info section 8 (rename detection, a v1
// feature, not v2): if a file disappears from a side and a NEW path with
// the same dev+inode (and matching size, as an inode-reuse guard) appears
// on that same side, it is a local rename -- propagate a cheap OpRename to
// the other side instead of OpDelete+OpCopy. Falls back silently (returns
// the path unconsumed, handled by the generic three-way diff) whenever the
// other side is not in a clean state to receive a plain rename (it already
// changed that path independently, or Dev/Ino is unavailable e.g. Windows).
func detectRenames(baseline, primary, secondary model.Tree, res *Result) map[string]bool {
	consumed := map[string]bool{}

	applySideRename := func(newTree model.Tree, dstSide string, otherTree model.Tree) {
		deletedByKey := map[devIno]string{}
		for p, e := range baseline {
			if e.Type != model.TypeFile {
				continue
			}
			if _, stillThere := newTree[p]; stillThere {
				continue
			}
			if e.Dev == 0 && e.Ino == 0 {
				continue
			}
			deletedByKey[devIno{e.Dev, e.Ino}] = p
		}
		if len(deletedByKey) == 0 {
			return
		}
		for np, ne := range newTree {
			if ne.Type != model.TypeFile {
				continue
			}
			if _, inBaseline := baseline[np]; inBaseline {
				continue
			}
			if ne.Dev == 0 && ne.Ino == 0 {
				continue
			}
			oldPath, found := deletedByKey[devIno{ne.Dev, ne.Ino}]
			if !found {
				continue
			}
			oldEntry, ok := baseline[oldPath]
			if !ok || oldEntry.Size != ne.Size {
				continue // inode-reuse guard failed
			}
			// oldPath -> np happened on newTree's side. The other side must
			// still be in the baseline state at oldPath, and must not
			// already have np, for a clean rename to apply.
			otherOld, otherHasOld := otherTree[oldPath]
			_, otherHasNew := otherTree[np]
			if !otherHasOld || otherHasNew || !model.SameIdentity(oldEntry, otherOld) {
				continue // ambiguous -- let the generic diff handle it (safe fallback)
			}

			res.Ops = append(res.Ops, Op{Kind: OpRename, DstSide: dstSide, OldPath: oldPath, Path: np, Type: model.TypeFile})
			res.NewBaseline[np] = ne
			consumed[oldPath] = true
			consumed[np] = true
		}
	}

	// rename happened on primary -> replicate onto secondary
	applySideRename(primary, SideSecondary, secondary)
	// rename happened on secondary -> replicate onto primary
	applySideRename(secondary, SidePrimary, primary)

	return consumed
}
