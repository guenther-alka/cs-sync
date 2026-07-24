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
	"github.com/guenther-alka/cs-sync/internal/guard"
	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/remote"
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
	case "serve":
		serveCmd(os.Args[2:])
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
  cs-sync run  --primary <path> [--secondary <path>] [--remote host:port] [options]
  cs-sync scan --primary <path> --secondary <path> [options]   (dry-run report)
  cs-sync serve --dest <path> --listen 127.0.0.1:9010          (2.0 receiver)
  cs-sync version

Options:
  --mode bidir|oneway     default bidir (oneway: --primary is the source,
                          --secondary is mirrored/overwritten; for the
                          reverse DR leg, swap which folder you pass as
                          --primary/--secondary)
  --debounce 500ms        event debounce window (section 7)
  --rescan 24h            safety-net full rescan interval (section 7)
  --max-watched-dirs 0    0=unlimited; FreeBSD recommends 50000 (section 7/14)
  --log <file>            default <primary>/.backupdata/cs-sync.log

2.0 remote options (cs-sync-2.0-design.info; --remote is ALWAYS one-way
primary -> remote and can be combined with a local --secondary pair):
  --remote host:port      receiver address (typically a local cs-stream
                          tunnel-send endpoint -- cs-sync does not encrypt)
  --remote-name <id>      state dir name (default: address, sanitized)
  --bwlimit <bytes/s>     rate limit remote transfers, 0=unlimited
  --max-delete-count 1000 delete-budget guard (section 5b), 0=off
  --max-delete-percent 20 delete-budget guard, 0=off

serve options:
  --dest <path>           destination folder on ZFS (required)
  --listen <addr>         default 127.0.0.1:9010 (loopback; put a
                          cs-stream tunnel-listen in front for encryption)
  --log <file>            default <dest>/.backupdata/cs-sync.log`)
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
	remoteAddr := fs.String("remote", "", "2.0: receiver host:port (one-way primary -> remote)")
	remoteName := fs.String("remote-name", "", "2.0: per-pair state dir name")
	bwlimit := fs.Int64("bwlimit", 0, "2.0: remote rate limit bytes/s, 0=unlimited")
	maxDelCount := fs.Int("max-delete-count", 1000, "2.0: delete-budget guard, 0=off")
	maxDelPercent := fs.Int("max-delete-percent", 20, "2.0: delete-budget guard, 0=off")
	fs.Parse(args)

	if *primaryPath == "" {
		fmt.Fprintln(os.Stderr, "error: --primary is required")
		os.Exit(2)
	}
	if *secondaryPath == "" && *remoteAddr == "" {
		fmt.Fprintln(os.Stderr, "error: at least one of --secondary or --remote is required")
		os.Exit(2)
	}
	if !apply_ && *secondaryPath == "" {
		fmt.Fprintln(os.Stderr, "error: scan needs --secondary")
		os.Exit(2)
	}
	primaryPath2, _ := filepath.Abs(*primaryPath)
	secondaryPath2 := ""
	if *secondaryPath != "" {
		secondaryPath2, _ = filepath.Abs(*secondaryPath)
	}

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
	acltype := acltypeP
	if secondaryPath2 != "" {
		acltypeS, err := zfscheck.CheckAndPrepare(secondaryPath2)
		if err != nil {
			log.Printf("FATAL: secondary precondition check failed: %v", err)
			os.Exit(1)
		}
		if acltypeP != acltypeS {
			log.Printf("WARN: acltype differs (primary=%s secondary=%s) -- using primary's acltype as authoritative", acltypeP, acltypeS)
		}
	}
	log.Printf("acltype=%s (aclinherit=passthrough set on both parent datasets)", acltype)

	// remove leftover crash-safety temp files from a previous run (section 8)
	cleanupTmp(primaryPath2, log)
	if secondaryPath2 != "" {
		cleanupTmp(secondaryPath2, log)
	}

	roots := apply.Roots{Primary: primaryPath2, Secondary: secondaryPath2, AclType: acltype}

	// 2.0 remote sender (section 12: remote is ALWAYS one-way, can be
	// combined with a local secondary pair -- topology section 11).
	var sender *remote.Sender
	if apply_ && *remoteAddr != "" {
		name := *remoteName
		if name == "" {
			name = sanitizeName(*remoteAddr)
		}
		sd, derr := state.Dir(primaryPath2)
		if derr != nil {
			log.Printf("FATAL: %v", derr)
			os.Exit(1)
		}
		sender = &remote.Sender{
			Primary:  primaryPath2,
			Addr:     *remoteAddr,
			StateDir: filepath.Join(sd, "remote_"+name),
			AclType:  acltype,
			Budget:   guard.Budget{MaxCount: *maxDelCount, MaxPercent: *maxDelPercent},
			Limiter:  remote.NewLimiter(*bwlimit),
			Version:  version,
			Log:      log,
		}
		if err := sender.Init(); err != nil {
			log.Printf("FATAL: remote state init: %v", err)
			os.Exit(1)
		}
		log.Printf("remote leg enabled: %s (state=remote_%s, bwlimit=%d, delete budget count=%d percent=%d)",
			*remoteAddr, name, *bwlimit, *maxDelCount, *maxDelPercent)
	}

	if apply_ && secondaryPath2 != "" {
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
		rootACL := populateACL(primaryTree, primaryPath2, acltype, log)

		// ---- 2.0 remote leg (independent of the local pair; its own
		// baseline/queue/retry state, section 15) ----
		if sender != nil {
			var csvBlob []byte
			if d, derr := state.Dir(primaryPath2); derr == nil {
				csvBlob, _ = os.ReadFile(filepath.Join(d, state.AclCsvName))
			}
			sender.Pass(primaryTree, reason, rootACL, csvBlob)
		}
		if secondaryPath2 == "" {
			// remote-only relationship: local pair machinery not in play,
			// but acl.csv must still be produced for the remote push above
			// (next pass) -- write it from the primary tree directly.
			if apply_ {
				if err := state.WriteACLCSV(primaryPath2, primaryTree, acltype, rootACL); err != nil {
					log.Printf("ERROR writing acl.csv: %v", err)
				}
			}
			return
		}

		secondaryTree, err := scanner.Scan(secondaryPath2)
		if err != nil {
			log.Printf("ERROR scanning secondary: %v", err)
			return
		}

		// EXISTING-FOLDER ACL RE-SYNC (Gea decision 2026.07.24, "option B"
		// of the mkdirWithACL fix follow-up discussion): reconcile's
		// identity check is size+mtime+type only (model.SameIdentity,
		// "dir identity is its existence + ACL, checked separately" -- this
		// IS that separate check). A later ACL-only edit on an existing
		// primary folder (e.g. adding an ACE with setfacl/nfs4_setfacl/
		// chmod A+ after the folder was already created and synced) has NO
		// effect on size/mtime/type, so it produces zero reconcile.Op's and
		// would otherwise NEVER propagate to secondary during normal live
		// operation -- only brand-new folders (mkdirWithACL) and the
		// startup bootstrapACL restore flow ever touched folder ACLs
		// before this fix. MUST run before the "no changes" early return
		// below, since the whole point is to catch ACL-only edits that
		// produce no ops. Deliberately gated to safety-net/sighup passes
		// only (not every debounced event pass, typically every few
		// hundred ms-seconds): primary's folder ACL is already read every
		// pass via populateACL above (existing cost), but pushing it onto
		// secondary needs one exec call (setfacl/nfs4_setfacl/chmod) per
		// existing folder on the secondary side too -- cheap once per
		// --rescan interval (default 24h) or on-demand via SIGHUP, not
		// worth paying on every fast event pass. See cs-sync.info section
		// 4/10/14.
		if apply_ && (reason == "safety-net" || reason == "sighup") {
			syncExistingFolderACLs(primaryTree, secondaryPath2, acltype, log)
		}

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

	watchRoots := []string{primaryPath2}
	if secondaryPath2 != "" {
		watchRoots = append(watchRoots, secondaryPath2)
	}
	w, err := watch.New(watchRoots, watch.Options{
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

// syncExistingFolderACLs re-applies primary's current folder ACL onto the
// corresponding EXISTING secondary folder, for every directory present on
// BOTH sides. This is the "checked separately" half of dir identity
// (model.SameIdentity's doc comment) -- newly created folders already get
// primary's ACL via mkdirWithACL (internal/apply/executor.go), and a
// fresh/recovered start gets it via bootstrapACL, but an ACL-only edit on
// an already-synced, pre-existing folder had no other path to secondary
// before this (see the call site's comment in runCmd/doPass for why this
// only runs on safety-net/sighup passes, not every event pass).
//
// KISS: unconditionally re-applies rather than reading secondary's ACL
// first to compare -- this only runs once per --rescan interval (default
// 24h) or on-demand via SIGHUP, so the extra idempotent applies are cheap;
// avoiding a second per-folder read on secondary keeps this simple and
// keeps the exec-call count to one per folder (matching mkdirWithACL's
// existing cost model) instead of two.
func syncExistingFolderACLs(primaryTree model.Tree, secondaryPath2, acltype string, log *logging.Logger) {
	n := 0
	for relpath, pe := range primaryTree {
		if pe.Type != model.TypeDir || pe.ACL == "" {
			continue
		}
		dst := filepath.Join(secondaryPath2, filepath.FromSlash(relpath))
		if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
			continue // not on secondary yet -- normal reconcile creates it (with ACL) separately
		}
		if err := acl.Apply(dst, acltype, pe.ACL); err != nil {
			log.Printf("WARN: could not re-sync ACL for %s: %v", relpath, err)
			continue
		}
		n++
	}
	if n > 0 {
		log.Printf("existing-folder ACL re-sync: %d folder(s) refreshed on secondary", n)
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

// sanitizeName turns a remote address into a filesystem-safe state dir name.
func sanitizeName(addr string) string {
	out := make([]rune, 0, len(addr))
	for _, r := range addr {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// serveCmd is the 2.0 receiver ("cs-sync serve", cs-sync-2.0-design.info
// sections 4+6): applies incoming operations under --dest with atomic
// temp+hash-verify+rename writes. Encryption is cs-stream's job -- put a
// tunnel-listen in front for anything beyond loopback.
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dest := fs.String("dest", "", "destination folder on ZFS (required)")
	listen := fs.String("listen", "127.0.0.1:9010", "listen address")
	logPath := fs.String("log", "", "log file path")
	fs.Parse(args)

	if *dest == "" {
		fmt.Fprintln(os.Stderr, "error: --dest is required")
		os.Exit(2)
	}
	dest2, _ := filepath.Abs(*dest)
	if *logPath == "" {
		if d, err := state.Dir(dest2); err == nil {
			*logPath = filepath.Join(d, "cs-sync.log")
		}
	}
	log, err := logging.New(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log:", err)
		os.Exit(1)
	}
	acltype, err := zfscheck.CheckAndPrepare(dest2)
	if err != nil {
		log.Printf("FATAL: dest precondition check failed: %v", err)
		os.Exit(1)
	}
	cleanupTmp(dest2, log)
	rv := &remote.Receiver{Dest: dest2, AclType: acltype, Version: version, Log: log}
	if err := rv.Serve(*listen); err != nil {
		log.Printf("FATAL: %v", err)
		os.Exit(1)
	}
}
