// Package reconcile implements the three-way merge described in
// cs-sync.info section 3 (STRATEGY) and section 8 (rename detection).
package reconcile

import (
	"fmt"
	"sort"

	"github.com/guenther-alka/cs-sync/internal/model"
)

const (
	SidePrimary   = "primary"
	SideSecondary = "secondary"
)

type OpKind int

const (
	OpMkdir OpKind = iota
	OpCopy         // copy file/symlink content, src side -> dst side
	OpDelete
	OpRmdir
	OpRename // rename OldPath -> Path on DstSide (no content transfer)
	OpConflictRename
)

// Op is one action for the executor to apply (cs-sync.info section 8).
type Op struct {
	Kind    OpKind
	DstSide string // primary | secondary -- where the op is applied
	SrcSide string // for OpCopy: where the content comes from
	Path    string // relpath (destination name after the op)
	OldPath string // for OpRename/OpConflictRename: relpath before the op
	Type    string // model.TypeFile/Dir/Symlink, for the executor's benefit
}

// Result is the outcome of one reconcile pass.
type Result struct {
	Ops         []Op
	NewBaseline model.Tree // baseline AFTER ops are applied (caller applies ops then saves this)
	Conflicts   int
}

// Reconcile computes the three-way diff between baseline, primary and
// secondary and returns the operations needed to converge both sides,
// plus the resulting baseline. See cs-sync.info section 3.
func Reconcile(baseline, primary, secondary model.Tree) Result {
	res := Result{NewBaseline: model.Tree{}}

	renamed := detectRenames(baseline, primary, secondary, &res)

	all := map[string]bool{}
	for p := range baseline {
		all[p] = true
	}
	for p := range primary {
		all[p] = true
	}
	for p := range secondary {
		all[p] = true
	}

	paths := make([]string, 0, len(all))
	for p := range all {
		if renamed[p] {
			continue // already resolved by rename detection
		}
		paths = append(paths, p)
	}
	sort.Strings(paths) // parents sort before children (see reconcile note)

	for _, p := range paths {
		b, inB := baseline[p]
		pr, inP := primary[p]
		se, inS := secondary[p]

		switch {
		case !inB && inP && !inS:
			res.addCreate(SideSecondary, SidePrimary, p, pr)
		case !inB && !inP && inS:
			res.addCreate(SidePrimary, SideSecondary, p, se)
		case !inB && inP && inS:
			if model.SameIdentity(pr, se) {
				res.NewBaseline[p] = pr
			} else {
				res.conflict(p, pr, se)
			}
		case inB && !inP && !inS:
			// gone on both sides, nothing to do, drop from baseline
		case inB && !inP && inS:
			if model.SameIdentity(b, se) {
				res.Ops = append(res.Ops, Op{Kind: delKind(se.Type), DstSide: SideSecondary, Path: p, Type: se.Type})
			} else {
				// primary deleted it, secondary modified it: keep secondary's
				// edit as a conflict copy, then apply primary's delete (see
				// section 9 -- primary wins, nothing lost)
				res.conflictDelete(SideSecondary, p, se)
			}
		case inB && inP && !inS:
			if model.SameIdentity(b, pr) {
				res.Ops = append(res.Ops, Op{Kind: delKind(pr.Type), DstSide: SidePrimary, Path: p, Type: pr.Type})
			} else {
				res.conflictDelete(SidePrimary, p, pr)
			}
		case inB && inP && inS:
			bp := model.SameIdentity(b, pr)
			bs := model.SameIdentity(b, se)
			switch {
			case bp && bs:
				res.NewBaseline[p] = b
			case bp && !bs:
				res.addCreate(SidePrimary, SideSecondary, p, se)
			case !bp && bs:
				res.addCreate(SideSecondary, SidePrimary, p, pr)
			default: // both changed
				if model.SameIdentity(pr, se) {
					res.NewBaseline[p] = pr
				} else {
					res.conflict(p, pr, se)
				}
			}
		}
	}

	return res
}

func delKind(t string) OpKind {
	if t == model.TypeDir {
		return OpRmdir
	}
	return OpDelete
}

// addCreate queues a create (mkdir or copy) of srcEntry from srcSide onto
// dstSide at path p, and records the resulting baseline entry.
func (r *Result) addCreate(dstSide, srcSide, p string, srcEntry model.Entry) {
	if srcEntry.Type == model.TypeDir {
		r.Ops = append(r.Ops, Op{Kind: OpMkdir, DstSide: dstSide, SrcSide: srcSide, Path: p, Type: model.TypeDir})
	} else {
		r.Ops = append(r.Ops, Op{Kind: OpCopy, DstSide: dstSide, SrcSide: srcSide, Path: p, Type: srcEntry.Type})
	}
	r.NewBaseline[p] = srcEntry
}

// conflict resolves a same-path double-change per section 9: primary wins
// in place, secondary's version is preserved on both sides with a
// _conflict_<timestamp> suffix.
func (r *Result) conflict(p string, primaryEntry, secondaryEntry model.Entry) {
	r.Conflicts++
	conflictName := conflictPath(p)
	// 1. rename secondary's losing version aside, in place on secondary
	r.Ops = append(r.Ops, Op{Kind: OpConflictRename, DstSide: SideSecondary, OldPath: p, Path: conflictName, Type: secondaryEntry.Type})
	// 2. copy that conflict version onto primary too (nothing lost)
	r.Ops = append(r.Ops, Op{Kind: OpCopy, DstSide: SidePrimary, SrcSide: SideSecondary, Path: conflictName, Type: secondaryEntry.Type})
	// 3. primary's winning version is copied onto secondary at the original path
	r.Ops = append(r.Ops, Op{Kind: OpCopy, DstSide: SideSecondary, SrcSide: SidePrimary, Path: p, Type: primaryEntry.Type})
	r.NewBaseline[p] = primaryEntry
	r.NewBaseline[conflictName] = secondaryEntry
}

// conflictDelete resolves a delete-vs-modify conflict: the side that still
// has content (loserSide) keeps it under a conflict name; primary's delete
// (or, if the modify happened on primary, primary's content) wins at the
// original path.
func (r *Result) conflictDelete(loserSide, p string, loserEntry model.Entry) {
	r.Conflicts++
	conflictName := conflictPath(p)
	r.Ops = append(r.Ops, Op{Kind: OpConflictRename, DstSide: loserSide, OldPath: p, Path: conflictName, Type: loserEntry.Type})
	other := SidePrimary
	if loserSide == SidePrimary {
		other = SideSecondary
	}
	r.Ops = append(r.Ops, Op{Kind: OpCopy, DstSide: other, SrcSide: loserSide, Path: conflictName, Type: loserEntry.Type})
	// original path stays deleted on loserSide (already renamed away) and is
	// absent on the winning side too -- nothing to (re)create.
	r.NewBaseline[conflictName] = loserEntry
}

func conflictPath(p string) string {
	return fmt.Sprintf("%s_conflict_%s", p, nowStamp())
}
