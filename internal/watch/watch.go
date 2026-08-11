// Package watch triggers reconcile passes on filesystem events, per
// cs-sync.info section 7 (EVENT-DRIVEN SERVICE MODE).
//
// v1 simplification (documented, not hidden): the watcher does NOT track
// which paths changed -- it only signals "something changed" after a
// debounce window. The reconciler (internal/reconcile) always does a full
// three-way diff of both trees, which is correct regardless of how many
// paths changed; this trades some rescan cost for a much simpler and more
// robust event layer. See cs-sync.info section 7/14 if this needs
// revisiting for very large trees.
package watch

import (
	"path/filepath"
	"strings"
	"time"
)

// Watcher signals Changed() whenever a debounced batch of filesystem
// events has settled, and periodically via the safety-net rescan timer
// (section 7).
type Watcher interface {
	// Changed fires (debounced) after real filesystem events, AND on the
	// periodic safety-net rescan interval, AND once immediately at start.
	Changed() <-chan string // reason string for logging ("event" | "safety-net" | "sighup")
	Close() error
}

// Options configures debounce and the safety-net rescan interval.
type Options struct {
	Debounce       time.Duration // default 500ms, section 7
	SafetyNet      time.Duration // default 24h, section 7
	MaxWatchedDirs int           // 0 = unlimited; FreeBSD default 50000, section 7/14
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		o.Debounce = 500 * time.Millisecond
	}
	if o.SafetyNet <= 0 {
		o.SafetyNet = 24 * time.Hour
	}
	return o
}

// isBackupdataPath reports whether p (an absolute or relative path, as
// delivered by either backend's raw event) has ".backupdata" as one of
// its path components. Used by both watch_fsnotify.go (Windows's
// ReadDirectoryChangesW-based backend watches subtrees recursively
// regardless of which individual directories were explicitly
// registered, so addRecursive's SkipDir alone does not stop events for
// paths under .backupdata from arriving -- see that file's debounceLoop
// for the live-verified finding) and watch_eventport_illumossolaris.go
// (defense in depth; FEN associations are not known to auto-recurse the
// same way, but filtering by path is cheap and removes any doubt).
func isBackupdataPath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if part == ".backupdata" {
			return true
		}
	}
	return false
}
