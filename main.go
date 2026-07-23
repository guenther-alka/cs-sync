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
  --mode bidir|oneway     default bidir (oneway: secondary -> primary only)
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
	// bootstrap ACL source for the rare "secondary filled by replication,
	// primary empty" case (section 6, first-sync case b)
	roots.PrimaryBootstrapACL, _ = state.ReadACLCSV(secondaryPath2)

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
		populateACL(primaryTree, primaryPath2, acltype, log)

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
			if err := state.WriteACLCSV(primaryPath2, res.NewBaseline, acltype); err != nil {
				log.Printf("ERROR writing acl.csv: %v", err)
			}
			if err := state.MirrorACLCSV(primaryPath2, secondaryPath2); err != nil {
				log.Printf("ERROR mirroring acl.csv: %v", err)
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

// forceOneway drops any baseline/state that would let the reconciler
// propagate primary -> secondary, implementing --mode oneway (section 11:
// server2 secondary_s3 -> primary_smb DR leg). Simplest correct approach:
// treat primary as if it always equals the baseline, so the three-way
// merge never proposes primary -> secondary ops, only secondary -> primary.
func forceOneway(baseline *model.Tree, primaryTree, secondaryTree model.Tree) {
	nb := model.Tree{}
	for p, e := range primaryTree {
		nb[p] = e
	}
	*baseline = nb
}

func populateACL(tree model.Tree, root, acltype string, log *logging.Logger) {
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
	// root folder itself
	if text, err := acl.Read(root, acltype); err == nil {
		e := tree["."]
		e.ACL = text
	}
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
