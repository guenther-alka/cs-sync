// Status/loop-detection files for the napp-it CS Realtime Sync menu.
// See cs-sync-2.0-design.info section 16+ and the "Sync Service Menu"
// redesign (Gea 2026.07.25).
//
// STATUS FILE: _cfg/sync/<serviceid>.last holds a one-line human-readable
// summary of the last action (">file.txt" sent, "<file.txt" received,
// "started (time)", or "error: ..." for a fatal/refused-to-start
// condition) -- the Perl menu tails this file for the status column
// instead of parsing the full cs-sync.log.
//
// LOOP DETECTION (unidir chains only, v1): a marker file
// <secondary>/.cs-sync-chain/<serviceid>.loop is written ONCE at service
// startup, containing a random per-process-run stamp. This marker is a
// regular file inside the synced tree, so if this secondary is itself
// configured as the PRIMARY of another (or the same) unidir service
// further down a chain (a -> b -> c -> d -> a), it keeps propagating
// forward with every hop. Each service checks, at the start of every
// pass, whether ITS OWN primary now contains a marker with ITS OWN stamp
// -- which can only happen if that exact marker travelled all the way
// around a cycle and landed back at its origin. The marker is written
// into secondary ONLY (never into primary directly), so a healthy
// acyclic setup never produces a false positive: primary is purely a
// check location for this service, never a write target.
//
// Not implemented in v1: propagating the marker over the --remote wire
// leg (would need a dedicated wire message); cross-host chains are not
// yet covered by loop detection. Documented limitation, TODO for v2.1.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/guenther-alka/cs-sync/internal/reconcile"
)

// syncCfgDir returns the fixed napp-it CS config directory for sync
// service status files: /opt/csweb-gui/_cfg/sync on Unix,
// C:\opt\csweb-gui\_cfg\sync on Windows (napp-it CS's uniform /opt
// convention -- Windows maps /opt to C:\opt).
func syncCfgDir() string {
	if runtime.GOOS == "windows" {
		return `C:\opt\csweb-gui\_cfg\sync`
	}
	return "/opt/csweb-gui/_cfg/sync"
}

// writeStatus overwrites _cfg/sync/<serviceID>.last with a single
// timestamped line. No-op if serviceID is empty (status tracking is
// optional -- only napp-it-CS-menu-managed services set --service-id).
func writeStatus(serviceID, line string) {
	if serviceID == "" {
		return
	}
	dir := syncCfgDir()
	if err := os.MkdirAll(dir, 0755); err != nil {
		return // best-effort; a status file failure must never abort a sync
	}
	ts := time.Now().Format("2006-01-02 15:04:05")
	content := fmt.Sprintf("%s  %s\n", ts, line)
	_ = os.WriteFile(filepath.Join(dir, serviceID+".last"), []byte(content), 0644)
}

const loopMarkerDir = ".cs-sync-chain"

// newLoopStamp generates this process run's unique loop-detection stamp.
func newLoopStamp() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// writeLoopMarker drops this service's marker (own stamp) into the
// downstream target -- secondary for a local unidir pair. Never call this
// with primaryPath: the marker must only ever arrive at primary via an
// actual incoming sync, not via our own direct write, or every service
// would trivially "detect" a loop against itself.
func writeLoopMarker(secondaryRoot, serviceID, stamp string) {
	if serviceID == "" || secondaryRoot == "" {
		return
	}
	dir := filepath.Join(secondaryRoot, loopMarkerDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, serviceID+".loop"), []byte(stamp+"\n"), 0644)
}

// checkLoopMarker reports whether THIS service's own marker (matching
// stamp) has appeared in its own primary -- meaning it travelled forward
// through a chain of one or more other unidir services and looped back.
func checkLoopMarker(primaryRoot, serviceID, stamp string) bool {
	if serviceID == "" || primaryRoot == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(primaryRoot, loopMarkerDir, serviceID+".loop"))
	if err != nil {
		return false
	}
	found := string(data)
	return len(found) >= len(stamp) && found[:len(stamp)] == stamp
}

// writeLastFileStatus writes the last file-copy op from a completed pass
// as the service's status line: ">path" for a copy landing on secondary
// (sent from primary), "<path" for a copy landing on primary (received
// from secondary) -- reconcile.Op.DstSide tells us the direction.
// Only OpCopy is reported (mkdir/delete/rmdir/rename are comparatively
// uninteresting for an at-a-glance status).
func writeLastFileStatus(serviceID string, ops []reconcile.Op) {
	var last *reconcile.Op
	for i := range ops {
		if ops[i].Kind == reconcile.OpCopy {
			last = &ops[i]
		}
	}
	if last == nil {
		return
	}
	prefix := ">"
	if last.DstSide == "primary" {
		prefix = "<"
	}
	writeStatus(serviceID, prefix+last.Path)
}
