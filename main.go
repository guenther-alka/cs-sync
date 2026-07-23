// cs-sync -- realtime bidirectional folder sync for ZFS hosts.
// See csweb-gui/data/menues/03_System/02_Services/25_Realtime_Sync/cs-sync.info
// for the full concept/design doc. This is the v1 implementation.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/guenther-alka/cs-sync/internal/acl"
	"github.com/guenther-alka/cs-sync/internal/apply"
	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/scanner"
	"github.com/guenther-alka/cs-sync/internal/state"
	"github.com/guenther-alka/cs-sync/internal/watch"
	"github.com/guenther-alka/cs-sync/internal/zfscheck"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	switch cmd {
	case "version":
		fmt.Println("cs-sync " + version)
	case "run":
		runCmd(os.Args[2:], true)
	case "scan":
		runCmd(os.Args[2:], false)
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`cs-sync -- realtime bidirectional folder sync (see cs-sync.info)

Usage:
  cs-sync run  --primary <path> --secondary <path> [options]
  cs-sync scan --primary <path> --secondary <path> [options]   (dry-run report)
  cs-sync version

Options:
  --mode bidir|oneway     default bidir (oneway: --primary is the source,
                          --secondary is mirrored/overwritten; for the
                          reverse DR leg, swap which folder you pass as
                          --primary/--secondary)
  --debounce 500ms        event debounce window (section 7)
  --rescan 24h            safety-net full rescan interval (section 7)
  --max-watched-dirs 0    0=unlimited; FreeBSD recommends 50000 (section 7/14)
  --log <file>            default <primary>/.backupdata/cs-sync.log`)
}

func runCmd(args []string, apply_ bool) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	primaryPath := fs.String("primary", "", "primary folder (required)")
	secondaryPath := fs.String("secondary", "", "secondary folder (required)")
	mode := fs.String("mode", "bidir", "bidir | oneway")
	debounce := fs.Duration("debounce", 500*time.Millisecond, "event debounce window")
	rescan := fs.Duration("rescan", 24*time.Hour, "safety-net rescan interval")
	maxWatched := fs.Int("max-watched-dirs", 0, "0=unlimited; FreeBSD suggested 50000")
	logPath := fs.String("log", "", "log file path")
	fs.Parse(args)

	if *primaryPath == "" || *secondaryPath == "" {
		fmt.Fprintln(os.Stderr, "error: --primary and --secondary are required")
		os.Exit(2)
	}
	primaryPath2, _ := filepath.Abs(*primaryPath)
	secondaryPath2, _ := filepath.Abs(*secondaryPath)

	if *logPath == "" {
		if d, err := state.Dir(primaryPath2); err == nil {
			*logPath = filepath.Join(d, "cs-sync.log")
		}
	}
	log, err := logging.New(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log:", err)
		os.Exit(1)
	}
	log.Printf("cs-sync %s starting: primary=%s secondary=%s mode=%s", version, primaryPath2, secondaryPath2, *mode)

	// --- section 2: ZFS preconditions ---
	acltypeP, err := zfscheck.CheckAndPrepare(primaryPath2)
	if err != nil {
		log.Printf("FATAL: primary precondition check failed: %v", err)
		os.Exit(1)
	}
	acltypeS, err := zfscheck.CheckAndPrepare(secondaryPath2)
	if err != nil {
		log.Printf("FATAL: secondary precondition check failed: %v", err)
		os.Exit(1)
	}
	acltype := acltypeP
	if acltypeP != acltypeS {
		log.Printf("WARN: acltype differs (primary=%s secondary=%s) -- using primary's acltype as authoritative", acltypeP, acltypeS)
	}
	log.Printf("acltype=%s (aclinherit=passthrough set on both parent datasets)", acltype)

	// remove leftover crash-safety temp files from a previous run (section 8)
	cleanupTmp(primaryPath2, log)
	cleanupTmp(secondaryPath2, log)

	roots := apply.Roots{Primary: primaryPath2, Secondary: secondaryPath2, AclType: acltype}
	if apply_ {
		// ACL bootstrap (Gea decision 2026.07.23): auto-detect which acl.csv
		// (if any) is authoritative and restore/propagate it once at
		// startup. Priority: primary's own acl.csv, else secondary's
		// (recovery), else none (fresh live scan, folders simply inherit
		// as they're created). See section 10 "ACL bootstrap priority chain".
		roots.PrimaryBootstrapACL = bootstrapACL(primaryPath2, secondaryPath2, acltype, log)
	}

	doPass := func(reason string) {
		st, err := state.Load(primaryPath2)
		if err != nil {
			log.Printf("ERROR loading state: %v", err)
			return
		}
		log.Printf("reconcile pass (%s): scanning...", reason)
		primaryTree, err := scanner.Scan(primaryPath2)
		if err != nil {
			log.Printf("ERROR scanning primary: %v", err)
			return
		}
		secondaryTree, err := scanner.Scan(secondaryPath2)
		if err != nil {
			log.Printf("ERROR scanning secondary: %v", err)
			return
		}
		rootACL := populateACL(primaryTree, primaryPath2, acltype, log)

		if *mode == "oneway" {
			forceOneway(&st.Baseline, primaryTree, secondaryTree)
		}

		res := reconcile.Reconcile(st.Baseline, primaryTree, secondaryTree)
		if len(res.Ops) == 0 {
			log.Printf("reconcile pass (%s): no changes", reason)
			return
		}
		log.Printf("reconcile pass (%s): %d ops, %d conflicts", reason, len(res.Ops), res.Conflicts)

		if apply_ {
			applyOrdered(res.Ops, roots, log)
			newState := &model.State{Baseline: res.NewBaseline, AclType: acltype}
			if err := state.Save(primaryPath2, newState); err != nil {
				log.Printf("ERROR saving state: %v", err)
			}
			if err := state.WriteACLCSV(primaryPath2, res.NewBaseline, acltype, rootACL); err != nil {
				log.Printf("ERROR writing acl.csv: %v", err)
			}
			if err := state.MirrorACLCSV(primaryPath2, secondaryPath2); err != nil {
				log.Printf("ERROR mirroring acl.csv: %v", err)
			}
			// root folder ACL (e.g. tank/data itself) is the inheritance
			// default for new top-level entries -- keep secondary's root
			// ACL in sync with primary's, same as every other folder.
			if rootACL != "" {
				if err := acl.Apply(secondaryPath2, acltype, rootACL); err != nil {
					log.Printf("WARN: could not apply root ACL to secondary: %v", err)
				}
			}
		} else {
			for _, op := range res.Ops {
				fmt.Printf("%-16s dst=%-9s path=%s\n", opName(op.Kind), op.DstSide, op.Path)
			}
		}
	}

	if !apply_ {
		doPass("scan")
		return
	}

	w, err := watch.New([]string{primaryPath2, secondaryPath2}, watch.Options{
		Debounce: *debounce, SafetyNet: *rescan, MaxWatchedDirs: *maxWatched,
	})
	if err != nil {
		log.Printf("FATAL: watcher init failed: %v", err)
		os.Exit(1)
	}
	defer w.Close()

	log.Printf("cs-sync running (mode=%s). Ctrl-C to stop.", *mode)
	for reason := range w.Changed() {
		doPass(reason)
	}
	}
}

// forceOneway implements --mode oneway (section 11: the whole-mirror mode
// used e.g. for the server2 secondary_s3 -> primary_smb DR leg -- there
// the operator simply passes the replicated folder as --primary and the
// empty target as --secondary; "oneway" always means "--primary is the
// source, --secondary is the mirror" (Gea decision 2026.07.23: primary ->
// secondary is the intuitive default direction). Implementation: pin the
// baseline to the CURRENT secondary tree, so the three-way merge only
// ever sees "secondary is unchanged" and propagates every primary
// create/update/delete onto secondary, while any secondary-only changes
// are simply overwritten or removed (full mirror, never fed back).
func forceOneway(baseline *model.Tree, primaryTree, secondaryTree model.Tree) {
	nb := model.Tree{}
	for p, e := range secondaryTree {
		nb[p] = e
	}
	*baseline = nb
}

// bootstrapACL implements the ACL source priority chain (Gea decision
// 2026.07.23, cs-sync.info section 10):
//
//	Fall 1 (initial setup): neither side has acl.csv yet -> nothing to
//	  restore, folders simply get parent-inherited ACL as cs-sync
//	  creates them during the normal reconcile pass that follows.
//	Fall 2 (restart/restore): primary/.backupdata/acl.csv exists and
//	  matches primary's current folder structure -> restore it onto
//	  primary's existing folders, then propagate onto secondary.
//	Fall 3 (recovery): primary has no (matching) acl.csv, but
//	  secondary's does match primary's current folder structure (e.g.
//	  primary was freshly restored/replicated) -> same restore+
//	  propagate, sourced from secondary's copy instead.
//
// "Matches" is all-or-nothing: every path listed in a candidate acl.csv
// must exist as a directory in primary's current tree, or the whole
// candidate is discarded (never partially applied). Any apply failure
// for an individual folder is logged and otherwise ignored -- that
// folder simply keeps whatever default/parent-inherited ACL it already
// has, per Gea's explicit instruction; this bootstrap step must not fail
// the whole startup over one folder's ACL.
//
// Returns the chosen source map (or nil if none matched), which is also
// kept as apply.Roots.PrimaryBootstrapACL so folders created LATER
// during ongoing operation (e.g. more data still arriving via RustFS
// replication) can keep drawing on the same source.
func bootstrapACL(primaryPath2, secondaryPath2, acltype string, log *logging.Logger) map[string]string {
	primaryTree, err := scanner.Scan(primaryPath2)
	if err != nil {
		log.Printf("WARN: ACL bootstrap: could not scan primary: %v", err)
		return nil
	}
	csvPrimary, _ := state.ReadACLCSV(primaryPath2)
	csvSecondary, _ := state.ReadACLCSV(secondaryPath2)
	source, sourceName := chooseACLSource(csvPrimary, csvSecondary, primaryTree)
	log.Printf("ACL bootstrap: source=%s", sourceName)
	if source == nil {
		return nil // Fall 1
	}

	// Fall 2/3: restore onto primary's existing folders first, including
	// the root itself (key "."), the default inheritance source for any
	// new top-level file/folder (e.g. tank/data's own ACL).
	for relpath, text := range source {
		full := primaryPath2
		if relpath != "." {
			if e, ok := primaryTree[relpath]; !ok || e.Type != model.TypeDir {
				continue // defensive; chooseACLSource already validated this
			}
			full = filepath.Join(primaryPath2, filepath.FromSlash(relpath))
		}
		if err := acl.Apply(full, acltype, text); err != nil {
			log.Printf("WARN: ACL bootstrap: could not restore ACL on primary %s: %v (folder keeps default/parent-inherited ACL)", relpath, err)
		}
	}

	// Propagate the now-authoritative primary ACL onto EXISTING secondary
	// folders (new folders are covered by the normal mkdir-time ACL logic),
	// including the secondary root itself.
	if rootText, ok := source["."]; ok {
		if err := acl.Apply(secondaryPath2, acltype, rootText); err != nil {
			log.Printf("WARN: ACL bootstrap: could not push root ACL to secondary: %v (folder keeps default/parent-inherited ACL)", err)
		}
	}
	secondaryTree, err := scanner.Scan(secondaryPath2)
	if err != nil {
		log.Printf("WARN: ACL bootstrap: could not scan secondary: %v", err)
		return source
	}
	for relpath, se := range secondaryTree {
		if se.Type != model.TypeDir {
			continue
		}
		pe, ok := primaryTree[relpath]
		if !ok || pe.Type != model.TypeDir {
			continue // only on secondary -- normal reconcile creates/removes it
		}
		text, err := acl.Read(filepath.Join(primaryPath2, filepath.FromSlash(relpath)), acltype)
		if err != nil {
			log.Printf("WARN: ACL bootstrap: could not read restored primary ACL for %s: %v", relpath, err)
			log.Printf("WARN: ACL bootstrap: could not read restored primary ACL for %s: %v", relpath, err)
			continue
		}
		full := filepath.Join(secondaryPath2, filepath.FromSlash(relpath))
		if err := acl.Apply(full, acltype, text); err != nil {
			log.Printf("WARN: ACL bootstrap: could not push ACL to secondary %s: %v (folder keeps default/parent-inherited ACL)", relpath, err)
		}
	}
	return source
}

// chooseACLSource picks primary's own acl.csv if valid, else secondary's,
// else none. "Valid" = every listed path exists as a directory in
// primaryTree (all-or-nothing, see bootstrapACL doc comment).
func chooseACLSource(csvPrimary, csvSecondary map[string]string, primaryTree model.Tree) (map[string]string, string) {
	if len(csvPrimary) > 0 && aclCSVMatches(csvPrimary, primaryTree) {
		return csvPrimary, "primary acl.csv (Fall 2: restart/restore)"
	}
	if len(csvSecondary) > 0 && aclCSVMatches(csvSecondary, primaryTree) {
		return csvSecondary, "secondary acl.csv (Fall 3: recovery)"
	}
	return nil, "none -- live scan only (Fall 1: initial setup, or no valid acl.csv found)"
}

func aclCSVMatches(csv map[string]string, primaryTree model.Tree) bool {
	for relpath := range csv {
		if relpath == "." {
			continue // root always exists (zfscheck already confirmed primary is a real dir); never a Tree entry
		}
		e, ok := primaryTree[relpath]
		if !ok || e.Type != model.TypeDir {
			return false
		}
	}
	return true
}

// populateACL reads and stores the current folder ACL for every directory
// in tree (section 6 initial scan behaviour), and returns the ACL of root
// itself -- root is never a Tree entry (see WriteACLCSV doc comment), so
// its ACL has to be threaded through separately.
func populateACL(tree model.Tree, root, acltype string, log *logging.Logger) string {
	for p, e := range tree {
		if e.Type != model.TypeDir {
			continue
		}
		text, err := acl.Read(filepath.Join(root, filepath.FromSlash(p)), acltype)
		if err != nil {
			log.Printf("WARN: could not read ACL for %s: %v", p, err)
			continue
		}
		e.ACL = text
		tree[p] = e
	}
	rootACL, err := acl.Read(root, acltype)
	if err != nil {
		log.Printf("WARN: could not read root ACL for %s: %v", root, err)
		return ""
	}
	return rootACL
}

// applyOrdered runs creates/renames/conflict-renames first (in the ascending
// path order reconcile.Reconcile produced, so parents exist before
// children), then deletes/rmdirs in REVERSE order (children removed before
// parents) -- see cs-sync.info section 8.
func applyOrdered(ops []reconcile.Op, roots apply.Roots, log *logging.Logger) {
	var creates, deletes []reconcile.Op
	for _, op := range ops {
		switch op.Kind {
		case reconcile.OpDelete, reconcile.OpRmdir:
			deletes = append(deletes, op)
		default:
			creates = append(creates, op)
		}
	}
	logf := func(format string, args ...any) { log.Printf(format, args...) }
	apply.Apply(creates, roots, logf)
	for i, j := 0, len(deletes)-1; i < j; i, j = i+1, j-1 {
		deletes[i], deletes[j] = deletes[j], deletes[i]
	}
	apply.Apply(deletes, roots, logf)
}

func cleanupTmp(root string, log *logging.Logger) {
	matches, _ := filepath.Glob(filepath.Join(root, "*.cs-sync.tmp.*"))
	for _, m := range matches {
		os.Remove(m)
		log.Printf("removed leftover temp file %s", m)
	}
}

func opName(k reconcile.OpKind) string {
	switch k {
	case reconcile.OpMkdir:
		return "MKDIR"
	case reconcile.OpCopy:
		return "COPY"
	case reconcile.OpDelete:
		return "DELETE"
	case reconcile.OpRmdir:
		return "RMDIR"
	case reconcile.OpRename:
		return "RENAME"
	case reconcile.OpConflictRename:
		return "CONFLICT"
	}
	return "?"
}
