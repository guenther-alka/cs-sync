// cs-sync -- realtime bidirectional/unidirectional folder sync for ZFS
// hosts, with a unified local/cs-sync/RustFS endpoint model (v3.0,
// sync-3.0-design.info). BREAKING relative to v2.x: no flag-compatible
// migration path exists -- v2.x-configured services must be recreated,
// not upgraded in place (Gea 2026.08.11).
//
// See csweb-gui/data/howto.ai/sync-3.0-design.info for the full
// concept/design doc.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/acl"
	"github.com/guenther-alka/cs-sync/internal/apply"
	"github.com/guenther-alka/cs-sync/internal/endpoint"
	"github.com/guenther-alka/cs-sync/internal/guard"
	"github.com/guenther-alka/cs-sync/internal/lock"
	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/remote"
	"github.com/guenther-alka/cs-sync/internal/rustfs"
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
	case "webhook-test":
		webhookTestCmd(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`cs-sync -- unified local/cs-sync/RustFS folder sync (v3.0, BREAKING vs 2.x)

Usage:
  cs-sync run --mode sync|push|pull --primary <endpoint> [options]
  cs-sync scan --mode sync --primary <endpoint> --secondary <endpoint>  (dry-run report)
  cs-sync serve --dest <path> --listen 127.0.0.1:9010 --key <k> --allow_ip <ip>
  cs-sync webhook-test --listen <addr> [--token <t>] [--allow_ip <ip>]
  cs-sync version

ENDPOINT syntax (--primary / --secondary / --backup alike):
  local path     D:\data, /tank/data, ./data, ../data
  cs-sync host   host[:port]           (default port 9010)
  RustFS bucket  host[:port]/bucket    (default port 9000)

MODES (always left-to-right: Primary -> Secondary -> Backup):
  sync   Primary(local) <==> Secondary(local, bidir) [==> Backup(any)]
  push   Primary(local) ==> Backup(any)                 (no Secondary)
  pull   Primary(any, source) ==> Backup(local)          (no Secondary)
         NOTE: pulling from a live cs-sync host is not yet implemented
         (Primary must be a local path or a RustFS endpoint for pull).

KEY / CREDENTIALS:
  --key <k>             default for every leg that needs one
  --primary_key/--secondary_key/--backup_key <k>   per-leg override
  Interpreted per that leg's endpoint kind: a cs-sync endpoint takes a
  pre-shared transfer/encryption key; a RustFS endpoint takes
  "access:secret" (used to auto-create/update the underlying rclone
  remote non-interactively -- no manual "rclone config" step needed).

  --allow_ip <ip>        restricts EVERY incoming listener this process
                          opens (serve's receiver, run's --webhook-listen)
                          to this single source IP.

Options:
  --secondary-storage-path <path>  sync mode only, OPTIONAL: when
                          --secondary is a local RustFS bucket, the
                          underlying ZFS dataset mountpoint backing it,
                          for real-time (not just --rescan-interval)
                          reaction to bucket-side changes. Omit it and
                          the bucket side is still fully correct, just
                          only re-scanned on --rescan / whenever
                          primary's own fsnotify triggers a pass anyway
                          -- fine for the common case where the real-
                          time direction that matters is primary's own
                          side (e.g. SMB writes propagating out to S3).
  --debounce 500ms        event debounce window
  --rescan 24h            safety-net full rescan interval
  --max-watched-dirs 0    0=unlimited; FreeBSD recommends 50000
  --log <file>            default <local anchor>/.backupdata/cs-sync.log
  --bwlimit <bytes/s>     rate limit backup transfers, 0=unlimited
  --max-delete-count 1000 delete-budget guard (backup leg only), 0=off
  --max-delete-percent 20 delete-budget guard (backup leg only), 0=off
  --service-id <id>       status (_cfg/sync/<id>.last) + loop detection
  --webhook-listen <addr> RustFS bucket-notification webhook receiver
                          (pull mode, remote RustFS source only)
  --webhook-token <t>     optional bearer token required on webhook POSTs

serve options:
  --dest <path>           destination folder on ZFS (required)
  --listen <addr>         default 127.0.0.1:9010 (loopback; put a
                          cs-stream tunnel-listen in front for encryption)
  --key <k>               REQUIRED, must match the sender's key
  --allow_ip <ip>         REQUIRED, single source IP allowed to connect
  --log <file>
  --service-id <id>

webhook-test options (diagnostic -- verifies a RustFS bucket's webhook
notification config actually reaches this host, before wiring
--webhook-listen into a real pull relationship):
  --listen <addr>         required, e.g. 0.0.0.0:9020
  --token <t>             optional bearer token required on POSTs
  --allow_ip <ip>         optional source-IP restriction`)
}

// resolvePSK returns the effective pre-shared key for a cs-sync-kind leg:
// per-leg override if set, else the shared --key default.
func resolvePSK(defaultKey, override string) string {
	if override != "" {
		return override
	}
	return defaultKey
}

// resolveRustFSTarget builds a rustfs.Target from a parsed RustFS-kind
// endpoint plus the effective key for that leg ("access:secret" form,
// per-leg override if set else the shared --key default). Does NOT call
// EnsureRemote -- callers do that once, right before first use.
func resolveRustFSTarget(ep endpoint.Endpoint, defaultKey, override string) (rustfs.Target, error) {
	key := override
	if key == "" {
		key = defaultKey
	}
	i := strings.Index(key, ":")
	if key == "" || i <= 0 || i == len(key)-1 {
		return rustfs.Target{}, fmt.Errorf("RustFS endpoint %q requires a key in \"access:secret\" form (via --key or the per-leg _key override)", ep.Raw)
	}
	return rustfs.Target{Host: ep.Host, Port: ep.Port, Bucket: ep.Bucket, Access: key[:i], Secret: key[i+1:]}, nil
}

func runCmd(args []string, apply_ bool) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	modeFlag := fs.String("mode", "", "sync | push | pull (required)")
	primarySpec := fs.String("primary", "", "required -- see endpoint syntax in usage")
	secondarySpec := fs.String("secondary", "", "sync mode only")
	backupSpec := fs.String("backup", "", "optional in sync; required in push/pull")
	keyFlag := fs.String("key", "", "default key/credentials for every leg that needs one")
	primaryKey := fs.String("primary_key", "", "override --key for the primary leg")
	secondaryKey := fs.String("secondary_key", "", "override --key for the secondary leg")
	backupKey := fs.String("backup_key", "", "override --key for the backup leg")
	allowIP := fs.String("allow_ip", "", "restrict incoming listeners (--webhook-listen) to this source IP")
	secondaryStoragePath := fs.String("secondary-storage-path", "", "sync mode only, OPTIONAL: when --secondary is a local RustFS bucket, the underlying ZFS dataset mountpoint backing it, for real-time (not just --rescan-interval) reaction to bucket-side changes")
	debounce := fs.Duration("debounce", 500*time.Millisecond, "event debounce window")
	rescan := fs.Duration("rescan", 24*time.Hour, "safety-net rescan interval")
	maxWatched := fs.Int("max-watched-dirs", 0, "0=unlimited; FreeBSD suggested 50000")
	logPath := fs.String("log", "", "log file path")
	bwlimit := fs.Int64("bwlimit", 0, "backup leg rate limit bytes/s, 0=unlimited")
	maxDelCount := fs.Int("max-delete-count", 1000, "delete-budget guard (backup leg), 0=off")
	maxDelPercent := fs.Int("max-delete-percent", 20, "delete-budget guard (backup leg), 0=off")
	serviceID := fs.String("service-id", "", "optional: service id for status/loop-detection files")
	webhookListen := fs.String("webhook-listen", "", "optional: RustFS bucket webhook receiver address, e.g. 0.0.0.0:9020")
	webhookToken := fs.String("webhook-token", "", "optional bearer token required on webhook POSTs")
	fs.Parse(args)
	_ = secondaryKey // reserved: sync mode's --secondary is always local (bidir-local-only rule), so no per-leg key is currently consumed; kept as a defined flag for CLI-surface consistency with --primary_key/--backup_key and for a future RustFS-typed local Secondary.

	if *modeFlag != "sync" && *modeFlag != "push" && *modeFlag != "pull" {
		fmt.Fprintln(os.Stderr, "error: --mode must be sync, push, or pull")
		os.Exit(2)
	}
	if *primarySpec == "" {
		fmt.Fprintln(os.Stderr, "error: --primary is required")
		os.Exit(2)
	}
	primaryEP, err := endpoint.Parse(*primarySpec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: --primary: %v\n", err)
		os.Exit(2)
	}
	var secondaryEP, backupEP endpoint.Endpoint
	haveSecondary, haveBackup := false, false
	if *secondarySpec != "" {
		secondaryEP, err = endpoint.Parse(*secondarySpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --secondary: %v\n", err)
			os.Exit(2)
		}
		haveSecondary = true
	}
	if *backupSpec != "" {
		backupEP, err = endpoint.Parse(*backupSpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: --backup: %v\n", err)
			os.Exit(2)
		}
		haveBackup = true
	}

	// --- v3.0 TRANSLATION LAYER ---------------------------------------
	// Map {mode, primary, secondary, backup} onto the EXISTING internal
	// model this file's core (below) already implements and has live-
	// tested: a local primaryPath2/secondaryPath2 pair (bidir or oneway,
	// optionally oneway-inverted via "pull"), plus an OPTIONAL
	// remote.Sender for a one-way backup leg (wire protocol or RustFS).
	// This keeps the reconcile/apply/state/watch core, and every ACL/
	// loop-detection/guard mechanism built on it, completely unchanged --
	// only endpoint parsing and role assignment are new.
	var (
		internalMode          = "bidir" // "bidir" | "oneway"
		pull                  = false
		primaryPath2          string // always a real local path -- also the state/lock/log anchor
		secondaryPath2        string // "" if no local secondary/mirror side
		secondaryKind         string // "" | "rustfs"
		secondaryRustFSTarget rustfs.Target
		senderRustFSTarget    rustfs.Target // backup leg, if backup is RustFS-kind
		senderWireAddr        string        // backup leg, if backup is cs-sync-kind
		senderWireKey         string
		haveSender            bool
	)

	switch *modeFlag {
	case "sync":
		if primaryEP.Kind != endpoint.KindLocal {
			fmt.Fprintln(os.Stderr, "error: sync mode requires --primary to be a local path")
			os.Exit(2)
		}
		if !haveSecondary {
			fmt.Fprintln(os.Stderr, "error: sync mode requires --secondary")
			os.Exit(2)
		}
		primaryPath2, _ = filepath.Abs(primaryEP.Path)
		switch secondaryEP.Kind {
		case endpoint.KindLocal:
			secondaryPath2, _ = filepath.Abs(secondaryEP.Path)
		case endpoint.KindRustFS:
			if !secondaryEP.IsLocal() {
				fmt.Fprintln(os.Stderr, "error: sync mode's --secondary must be local (cs-sync's bidir sync is local-only); a remote RustFS source belongs in pull mode instead")
				os.Exit(2)
			}
			secondaryKind = "rustfs"
			secondaryRustFSTarget, err = resolveRustFSTarget(secondaryEP, *keyFlag, *secondaryKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(2)
			}
			if *secondaryStoragePath != "" {
				// optional: real-time filesystem watch of the ZFS
				// dataset backing the bucket, for near-instant reaction
				// to a change made on the bucket side (S3 PUT/DELETE by
				// something other than this cs-sync process). Without
				// it, the bucket side is still fully correct -- just
				// only re-scanned on the existing --rescan safety-net
				// interval (or whenever primary's own fsnotify triggers
				// a pass anyway) rather than instantly. Gea 2026.08.11:
				// deliberately optional, no separate GUI field required
				// -- --secondary alone (a plain path, or "host/bucket" /
				// "host:port/bucket") is enough for the essential SMB<->
				// S3 case, where the important real-time direction is
				// SMB-side (primary) writes propagating OUT to S3, which
				// is already instant via primary's own fsnotify watch
				// regardless of whether this flag is set.
				secondaryPath2, _ = filepath.Abs(*secondaryStoragePath)
			}
		case endpoint.KindCSSync:
			fmt.Fprintln(os.Stderr, "error: sync mode's --secondary cannot be a cs-sync host (bidirectional sync over the wire protocol is not implemented -- a receiver only ever accepts pushed data)")
			os.Exit(2)
		}
		if haveBackup {
			if backupEP.Kind == endpoint.KindLocal {
				fmt.Fprintln(os.Stderr, "error: sync mode's --backup cannot be a local path (a third local mirror is not yet supported here -- use a second cs-sync relationship for that)")
				os.Exit(2)
			}
			haveSender = true
		}
	case "push":
		if primaryEP.Kind != endpoint.KindLocal {
			fmt.Fprintln(os.Stderr, "error: push mode requires --primary to be a local path (the source)")
			os.Exit(2)
		}
		if haveSecondary {
			fmt.Fprintln(os.Stderr, "error: push mode does not use --secondary")
			os.Exit(2)
		}
		if !haveBackup {
			fmt.Fprintln(os.Stderr, "error: push mode requires --backup (the target)")
			os.Exit(2)
		}
		primaryPath2, _ = filepath.Abs(primaryEP.Path)
		if backupEP.Kind == endpoint.KindLocal {
			// Local push target: reuse the plain local bidir engine in
			// one-way mode -- backup IS the internal "secondary" here,
			// with no sender leg at all.
			secondaryPath2, _ = filepath.Abs(backupEP.Path)
			internalMode = "oneway"
		} else {
			haveSender = true
		}
	case "pull":
		if haveSecondary {
			fmt.Fprintln(os.Stderr, "error: pull mode does not use --secondary")
			os.Exit(2)
		}
		if !haveBackup {
			fmt.Fprintln(os.Stderr, "error: pull mode requires --backup (the local destination)")
			os.Exit(2)
		}
		if backupEP.Kind != endpoint.KindLocal {
			fmt.Fprintln(os.Stderr, "error: pull mode's --backup must be a local path (the destination)")
			os.Exit(2)
		}
		if primaryEP.Kind == endpoint.KindCSSync {
			fmt.Fprintln(os.Stderr, "error: pulling from a live cs-sync host is not yet implemented (the wire protocol only supports pushing to a receiver, not pulling from one) -- --primary must be a local path or a RustFS endpoint for pull mode")
			os.Exit(2)
		}
		// Internal model: backup (local) becomes the state/lock/log
		// anchor AND the mirror target ("internal primary"); the real
		// source (v3.0 --primary) becomes "internal secondary", with
		// pull=true telling forceOneway that SECONDARY is the source of
		// truth instead of primary -- this is exactly the existing
		// --secondaryfs=rustfs+--pull machinery, unchanged.
		primaryPath2, _ = filepath.Abs(backupEP.Path)
		internalMode = "oneway"
		pull = true
		if primaryEP.Kind == endpoint.KindLocal {
			secondaryPath2, _ = filepath.Abs(primaryEP.Path)
		} else { // KindRustFS
			secondaryKind = "rustfs"
			secondaryRustFSTarget, err = resolveRustFSTarget(primaryEP, *keyFlag, *primaryKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(2)
			}
		}
	}

	if haveSender {
		switch backupEP.Kind {
		case endpoint.KindCSSync:
			senderWireAddr = backupEP.HostPort()
			senderWireKey = resolvePSK(*keyFlag, *backupKey)
			if apply_ && senderWireKey == "" {
				fmt.Fprintln(os.Stderr, "error: --backup is a cs-sync host: --key (or --backup_key) is required")
				os.Exit(2)
			}
		case endpoint.KindRustFS:
			senderRustFSTarget, err = resolveRustFSTarget(backupEP, *keyFlag, *backupKey)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
				os.Exit(2)
			}
		}
	}
	// --- end translation layer -----------------------------------------

	haveSecondaryLeg := secondaryPath2 != "" || secondaryKind == "rustfs"

	if !apply_ && !haveSecondaryLeg {
		fmt.Fprintln(os.Stderr, "error: scan needs a local secondary (sync mode with a --secondary)")
		os.Exit(2)
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
	log.Printf("cs-sync %s starting: mode=%s primary(anchor)=%s secondary=%s pull=%v", version, *modeFlag, primaryPath2, secondaryPath2, pull)
	loopStamp := newLoopStamp()

	// --- section 2: ZFS preconditions ---
	acltypeP, err := zfscheck.CheckAndPrepare(primaryPath2)
	if err != nil {
		log.Printf("FATAL: primary precondition check failed: %v", err)
		writeStatus(*serviceID, fmt.Sprintf("error: primary precondition check failed: %v", err))
		os.Exit(1)
	}

	acltype := acltypeP
	if secondaryPath2 != "" && secondaryKind != "rustfs" {
		acltypeS, err := zfscheck.CheckAndPrepare(secondaryPath2)
		if err != nil {
			log.Printf("FATAL: secondary precondition check failed: %v", err)
			writeStatus(*serviceID, fmt.Sprintf("error: secondary precondition check failed: %v", err))
			os.Exit(1)
		}
		if acltypeP != acltypeS {
			log.Printf("WARN: acltype differs (primary=%s secondary=%s) -- using primary's acltype as authoritative", acltypeP, acltypeS)
		}
	}
	if acltype == "none" {
		log.Printf("acltype=none (non-ZFS filesystem, file sync only -- no ACL propagation)")
	} else {
		log.Printf("acltype=%s (aclinherit=passthrough set on both parent datasets)", acltype)
	}

	writeStatus(*serviceID, fmt.Sprintf("started (%s)", time.Now().Format("2006-01-02 15:04:05")))
	if apply_ && *serviceID != "" && internalMode == "oneway" && secondaryPath2 != "" {
		writeLoopMarker(secondaryPath2, *serviceID, loopStamp)
	}
	cleanupTmp(primaryPath2, log)
	if secondaryPath2 != "" && secondaryKind != "rustfs" {
		cleanupTmp(secondaryPath2, log)
	}

	roots := apply.Roots{Primary: primaryPath2, Secondary: secondaryPath2, AclType: acltype}
	if secondaryKind == "rustfs" {
		if err := secondaryRustFSTarget.EnsureRemote(); err != nil {
			log.Printf("FATAL: could not configure rclone remote for %s: %v", secondaryRustFSTarget.HostPort(), err)
			writeStatus(*serviceID, fmt.Sprintf("error: rclone remote setup failed: %v", err))
			os.Exit(1)
		}
		roots.SecondaryKind = "rustfs"
		roots.SecondaryRustFSTarget = secondaryRustFSTarget
		log.Printf("secondary is RustFS: %s/%s", secondaryRustFSTarget.HostPort(), secondaryRustFSTarget.Bucket)

		if apply_ {
			if pull {
				log.Printf("running initial rclone one-way pull sync: %s/%s -> %s", secondaryRustFSTarget.HostPort(), secondaryRustFSTarget.Bucket, primaryPath2)
				if err := rustfs.PullSync(secondaryRustFSTarget, primaryPath2); err != nil {
					log.Printf("FATAL: initial rclone pull sync failed: %v", err)
					writeStatus(*serviceID, fmt.Sprintf("error: initial rclone pull sync failed: %v", err))
					os.Exit(1)
				}
			} else {
				log.Printf("running initial rclone bisync --resync: %s <-> %s/%s", primaryPath2, secondaryRustFSTarget.HostPort(), secondaryRustFSTarget.Bucket)
				if err := rustfs.Resync(primaryPath2, secondaryRustFSTarget); err != nil {
					log.Printf("FATAL: initial rclone bisync --resync failed: %v", err)
					writeStatus(*serviceID, fmt.Sprintf("error: initial rclone resync failed: %v", err))
					os.Exit(1)
				}
			}
		}
	}

	// backup/sender leg (always one-way, primary -> target)
	var sender *remote.Sender
	if apply_ && haveSender {
		var name string
		if senderWireAddr != "" {
			name = sanitizeName(senderWireAddr)
		} else {
			name = sanitizeName(senderRustFSTarget.HostPort() + "_" + senderRustFSTarget.Bucket)
		}
		sd, derr := state.Dir(primaryPath2)
		if derr != nil {
			log.Printf("FATAL: %v", derr)
			os.Exit(1)
		}
		if senderRustFSTarget.Bucket != "" {
			if err := senderRustFSTarget.EnsureRemote(); err != nil {
				log.Printf("FATAL: could not configure rclone remote for %s: %v", senderRustFSTarget.HostPort(), err)
				writeStatus(*serviceID, fmt.Sprintf("error: rclone remote setup failed: %v", err))
				os.Exit(1)
			}
		}
		sender = &remote.Sender{
			Primary:      primaryPath2,
			Addr:         senderWireAddr,
			StateDir:     filepath.Join(sd, "remote_"+name),
			AclType:      acltype,
			TransferKey:  senderWireKey,
			Budget:       guard.Budget{MaxCount: *maxDelCount, MaxPercent: *maxDelPercent},
			Limiter:      remote.NewLimiter(*bwlimit),
			Version:      version,
			Log:          log,
			ServiceID:    *serviceID,
			LoopStamp:    loopStamp,
			RustFSTarget: senderRustFSTarget,
		}
		if err := sender.Init(); err != nil {
			log.Printf("FATAL: remote state init: %v", err)
			os.Exit(1)
		}
		if senderRustFSTarget.Bucket != "" {
			log.Printf("backup leg enabled: RustFS %s/%s (state=remote_%s, delete budget count=%d percent=%d)",
				senderRustFSTarget.HostPort(), senderRustFSTarget.Bucket, name, *maxDelCount, *maxDelPercent)
		} else {
			log.Printf("backup leg enabled: cs-sync %s (state=remote_%s, bwlimit=%d, delete budget count=%d percent=%d)",
				senderWireAddr, name, *bwlimit, *maxDelCount, *maxDelPercent)
		}
	}

	if apply_ && secondaryPath2 != "" {
		if roots.SecondaryKind != "rustfs" {
			roots.PrimaryBootstrapACL = bootstrapACL(primaryPath2, secondaryPath2, acltype, log)
		}
	}

	doPass := func(reason string) {
		if *serviceID != "" && (internalMode == "oneway" || haveSender) && checkLoopMarker(primaryPath2, *serviceID, loopStamp) {
			log.Printf("FATAL: loop detected -- this service's own marker returned to its primary via a sync chain (a -> b -> ... -> a); stopping")
			writeStatus(*serviceID, "error: loop detected")
			os.Exit(1)
		}

		primaryDir, err := state.Dir(primaryPath2)
		if err != nil {
			log.Printf("ERROR: %v", err)
			return
		}
		passLock, err := lock.Acquire(primaryDir)
		if err != nil {
			log.Printf("ERROR acquiring pass lock: %v", err)
			return
		}
		defer passLock.Unlock()

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

		if sender != nil {
			var csvBlob []byte
			if d, derr := state.Dir(primaryPath2); derr == nil {
				csvBlob, _ = os.ReadFile(filepath.Join(d, state.AclCsvName))
			}
			sender.Pass(primaryTree, reason, rootACL, csvBlob)
		}
		if !haveSecondaryLeg {
			if apply_ {
				if err := state.WriteACLCSV(primaryPath2, primaryTree, acltype, rootACL); err != nil {
					log.Printf("ERROR writing acl.csv: %v", err)
				}
			}
			return
		}

		var secondaryTree model.Tree
		if roots.SecondaryKind == "rustfs" {
			secondaryTree, err = rustfs.Scan(roots.SecondaryRustFSTarget)
		} else {
			secondaryTree, err = scanner.Scan(secondaryPath2)
		}
		if err != nil {
			log.Printf("ERROR scanning secondary: %v", err)
			return
		}

		if apply_ && (reason == "safety-net" || reason == "sighup") && roots.SecondaryKind != "rustfs" {
			syncExistingFolderACLs(primaryTree, secondaryPath2, acltype, log)
		}

		if internalMode == "oneway" {
			forceOneway(&st.Baseline, primaryTree, secondaryTree, pull)
		}

		res := reconcile.Reconcile(st.Baseline, primaryTree, secondaryTree)
		if len(res.Ops) == 0 {
			log.Printf("reconcile pass (%s): no changes", reason)
			return
		}
		log.Printf("reconcile pass (%s): %d ops, %d conflicts", reason, len(res.Ops), res.Conflicts)

		if apply_ {
			applyOrdered(res.Ops, roots, log)
			if *serviceID != "" {
				writeLastFileStatus(*serviceID, res.Ops)
			}
			newState := &model.State{Baseline: res.NewBaseline, AclType: acltype}
			if err := state.Save(primaryPath2, newState); err != nil {
				log.Printf("ERROR saving state: %v", err)
			}
			if err := state.WriteACLCSV(primaryPath2, res.NewBaseline, acltype, rootACL); err != nil {
				log.Printf("ERROR writing acl.csv: %v", err)
			}
			if roots.SecondaryKind != "rustfs" {
				if err := state.MirrorACLCSV(primaryPath2, secondaryPath2); err != nil {
					log.Printf("ERROR mirroring acl.csv: %v", err)
				}
				if rootACL != "" {
					if err := acl.Apply(secondaryPath2, acltype, rootACL); err != nil {
						log.Printf("WARN: could not apply root ACL to secondary: %v", err)
					}
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

	if *webhookListen != "" {
		webhookCtx, webhookCancel := context.WithCancel(context.Background())
		defer webhookCancel()
		go func() {
			err := rustfs.ServeWebhook(webhookCtx, *webhookListen, *webhookToken, *allowIP, func(e rustfs.WebhookEvent) {
				log.Printf("webhook event: %s bucket=%s key=%s -- triggering reconcile pass", e.EventName, e.Bucket, e.Key)
				doPass("webhook")
			})
			if err != nil {
				log.Printf("ERROR: webhook listener stopped: %v", err)
			}
		}()
		log.Printf("webhook listener enabled on %s (allow_ip=%q)", *webhookListen, *allowIP)
	}

	log.Printf("cs-sync running (mode=%s). Ctrl-C to stop.", *modeFlag)
	for reason := range w.Changed() {
		doPass(reason)
	}
}

// forceOneway implements internal oneway mode: pin the baseline to the
// CURRENT tree of whichever side is NOT the source of truth, so the
// three-way merge only ever sees "that side is unchanged" and propagates
// every create/update/delete from the source side onto it.
//
// pull=false (push mode, or sync mode's non-existent oneway variant --
// kept general): primary is the source, secondary is the mirror.
// pull=true (pull mode): secondary (the real --primary source in the
// v3.0 CLI model, see runCmd's translation layer) is the source of
// truth, primary (the real --backup destination) is the mirror.
func forceOneway(baseline *model.Tree, primaryTree, secondaryTree model.Tree, pull bool) {
	src := secondaryTree
	if pull {
		src = primaryTree
	}
	nb := model.Tree{}
	for p, e := range src {
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
			continue
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
			continue
		}
		e, ok := primaryTree[relpath]
		if !ok || e.Type != model.TypeDir {
			return false
		}
	}
	return true
}

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

func syncExistingFolderACLs(primaryTree model.Tree, secondaryPath2, acltype string, log *logging.Logger) {
	n := 0
	for relpath, pe := range primaryTree {
		if pe.Type != model.TypeDir || pe.ACL == "" {
			continue
		}
		dst := filepath.Join(secondaryPath2, filepath.FromSlash(relpath))
		if fi, err := os.Stat(dst); err != nil || !fi.IsDir() {
			continue
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

// sanitizeName turns a remote address (or host+bucket) into a filesystem-
// safe state dir name.
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

// serveCmd is the wire-protocol receiver ("cs-sync serve"): applies
// incoming operations under --dest with atomic temp+hash-verify+rename
// writes, natively encrypted (ChaCha20-Poly1305, keyed from --key).
// --key and --allow_ip are both mandatory: an empty key or a missing
// source-IP allowlist would let an arbitrary host on the LAN attempt a
// sync.
func serveCmd(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dest := fs.String("dest", "", "destination folder on ZFS (required)")
	listen := fs.String("listen", "127.0.0.1:9010", "listen address")
	logPath := fs.String("log", "", "log file path")
	transferKey := fs.String("key", "", "REQUIRED: pre-shared transfer key -- also the encryption key, must match the sender's --key/--backup_key exactly")
	allowIP := fs.String("allow_ip", "", "REQUIRED: only this single source IP may connect (prevents an arbitrary LAN host from syncing)")
	serviceID := fs.String("service-id", "", "optional: service id for status/loop-detection files under _cfg/sync/<id>.last")
	fs.Parse(args)

	if *dest == "" {
		fmt.Fprintln(os.Stderr, "error: --dest is required")
		os.Exit(2)
	}
	if *transferKey == "" {
		fmt.Fprintln(os.Stderr, "error: --key is required (cs-sync encrypts natively; empty key is refused)")
		os.Exit(2)
	}
	if *allowIP == "" {
		fmt.Fprintln(os.Stderr, "error: --allow_ip is required (must name the single sender IP allowed to connect)")
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
		writeStatus(*serviceID, fmt.Sprintf("error: dest precondition check failed: %v", err))
		os.Exit(1)
	}
	cleanupTmp(dest2, log)
	writeStatus(*serviceID, fmt.Sprintf("started (%s)", time.Now().Format("2006-01-02 15:04:05")))
	rv := &remote.Receiver{Dest: dest2, AclType: acltype, TransferKey: *transferKey, AllowIP: *allowIP, Version: version, Log: log}
	if err := rv.Serve(*listen); err != nil {
		log.Printf("FATAL: %v", err)
		writeStatus(*serviceID, fmt.Sprintf("error: %v", err))
		os.Exit(1)
	}
}

// webhookTestCmd is a diagnostic tool: starts a webhook receiver and logs
// every event it gets, so an operator can verify a RustFS bucket's
// notification config actually reaches this host -- e.g. firewall/NAT/
// reachability from a remote member -- before wiring a real pull-based
// relationship on top of it. Ctrl-C to stop; this does not itself
// trigger any sync.
func webhookTestCmd(args []string) {
	fs := flag.NewFlagSet("webhook-test", flag.ExitOnError)
	listen := fs.String("listen", "", "required: address to listen on, e.g. 0.0.0.0:9020")
	token := fs.String("token", "", "optional: require this bearer token on POSTs")
	allowIP := fs.String("allow_ip", "", "optional: restrict POSTs to this single source IP")
	fs.Parse(args)

	if *listen == "" {
		fmt.Fprintln(os.Stderr, "error: --listen is required")
		os.Exit(2)
	}

	fmt.Printf("cs-sync webhook-test: listening on %s (Ctrl-C to stop)\n", *listen)
	if *token != "" {
		fmt.Println("  bearer token required on POSTs")
	}
	if *allowIP != "" {
		fmt.Printf("  restricted to source IP %s\n", *allowIP)
	}
	fmt.Println("  point a RustFS bucket's webhook notification config at this address,")
	fmt.Println("  then upload/delete an object in that bucket to see an event below.")

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		<-sig
		fmt.Println("\nstopping...")
		cancel()
	}()

	n := 0
	err := rustfs.ServeWebhook(ctx, *listen, *token, *allowIP, func(e rustfs.WebhookEvent) {
		n++
		fmt.Printf("[%s] #%d event=%s bucket=%s key=%s\n",
			time.Now().Format("15:04:05"), n, e.EventName, e.Bucket, e.Key)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("stopped, %d event(s) received total\n", n)
}
