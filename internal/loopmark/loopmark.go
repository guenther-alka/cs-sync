// Package loopmark implements cross-service loop detection for cs-sync
// (Gea 2026.07.25 "Sync Service Menu" redesign). A service configured as
// unidir (local secondary or a --remote leg) drops a small marker into
// its downstream target's .backupdata/loop/ directory; every pass it
// checks whether ITS OWN marker has reappeared in ITS OWN primary's
// .backupdata/loop/ -- which can only happen if the marker travelled all
// the way around a configured chain of services (a -> b -> c -> d -> a)
// and landed back at its origin.
//
// The marker lives in .backupdata (already excluded from the regular
// file scan/reconcile), not in the visible synced tree -- for local
// chains this is a direct filesystem write (secondary and the next hop's
// primary are literally the same directory on disk); for --remote it
// arrives via a dedicated wire.TLoopMarker frame sent once per connect.
package loopmark

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const subdir = "loop"

// NewStamp generates a fresh per-process-run loop-detection stamp.
func NewStamp() string {
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

// Write drops serviceID's marker (stamp) into <targetRoot>/.backupdata/loop/.
// Never call with a PRIMARY path -- the marker must only ever arrive there
// via an actual incoming write from elsewhere (local copy or the wire
// frame), never via this service's own direct write, or every service
// would trivially "detect" a loop against itself.
func Write(targetRoot, serviceID, stamp string) {
	if serviceID == "" || targetRoot == "" {
		return
	}
	dir := filepath.Join(targetRoot, ".backupdata", subdir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, serviceID+".loop"), []byte(stamp+"\n"), 0644)
}

// Check reports whether serviceID's own marker (matching stamp) has
// appeared in primaryRoot's .backupdata/loop/ -- meaning it travelled
// through one or more other services (local hops and/or a --remote leg)
// and looped all the way back to its origin.
func Check(primaryRoot, serviceID, stamp string) bool {
	if serviceID == "" || primaryRoot == "" {
		return false
	}
	data, err := os.ReadFile(filepath.Join(primaryRoot, ".backupdata", subdir, serviceID+".loop"))
	if err != nil {
		return false
	}
	found := string(data)
	return len(found) >= len(stamp) && found[:len(stamp)] == stamp
}
