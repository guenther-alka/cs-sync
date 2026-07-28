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
// LOOP DETECTION (local unidir AND --remote): a marker file
// <secondary-or-remote-dest>/.backupdata/loop/<serviceid>.loop is written
// ONCE at service startup, containing a random per-process-run stamp.
// The marker is NOT part of the synced user-data tree -- it lives in
// .backupdata (the existing state directory, already excluded from the
// regular file scan/reconcile) and is written directly:
//   - local unidir: written straight to <secondary>/.backupdata/loop/
//     (no sync-engine round trip needed -- secondary and the next hop's
//     primary are literally the same directory on disk).
//   - --remote: pushed once per connect via a dedicated TLoopMarker wire
//     frame (internal/wire) right after the handshake, so it arrives
//     immediately rather than waiting for a full reconcile pass; the
//     receiver writes it to <dest>/.backupdata/loop/ the same way.
//
// Each service only ever checks its OWN primary for its OWN
// serviceID+stamp, so a marker only ever matches if it travelled all the
// way around a genuine cycle (a -> b -> c -> d -> a) and landed back at
// its origin. Cross-host chains are covered via wire.TLoopMarker.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/guenther-alka/cs-sync/internal/loopmark"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
)

// Loop-detection is implemented in internal/loopmark (shared by main and
// internal/remote, since the receiver also needs to write markers on
// TLoopMarker receipt). newLoopStamp/writeLoopMarker/checkLoopMarker below
// are thin wrappers kept for call-site readability in main.go.

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

func newLoopStamp() string { return loopmark.NewStamp() }
func writeLoopMarker(targetRoot, serviceID, stamp string) {
	loopmark.Write(targetRoot, serviceID, stamp)
}
func checkLoopMarker(primaryRoot, serviceID, stamp string) bool {
	return loopmark.Check(primaryRoot, serviceID, stamp)
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
