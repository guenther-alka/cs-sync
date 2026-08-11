//go:build linux || windows || freebsd || darwin

package watch

import (
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

type fsWatcher struct {
	w         *fsnotify.Watcher
	out       chan string
	closeOnce sync.Once
	closed    chan struct{}

	mu           sync.Mutex
	watchedDirs  int
	maxDirs      int
	pollFallback bool
}

// New creates a real fsnotify-based watcher and recursively registers
// watches under each root. On FreeBSD (kqueue: one fd per watched path,
// expensive at scale) the watcher automatically switches to a periodic
// poll-only mode once MaxWatchedDirs is exceeded -- see section 7/14.
func New(roots []string, opt Options) (Watcher, error) {
	opt = opt.withDefaults()
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	fw := &fsWatcher{
		w:       w,
		out:     make(chan string, 1),
		closed:  make(chan struct{}),
		maxDirs: opt.MaxWatchedDirs,
	}

	for _, root := range roots {
		_ = fw.addRecursive(root)
	}

	go fw.debounceLoop(opt.Debounce)
	go fw.safetyNetLoop(opt.SafetyNet)
	go func() { fw.out <- "start" }() // sync once at startup, section 6

	return fw, nil
}

func (fw *fsWatcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		// v3.0 fix (live-verified cs_26.08.11, sync/push/pull live tests
		// against real infra): NEVER watch .backupdata (state.Dir's own
		// directory) -- it holds cs-sync.log, cs-sync.state.json, and
		// acl.csv, all of which this process itself rewrites on every
		// pass. Without this exclusion, those self-writes generate their
		// own fsnotify events, which re-trigger a debounced pass, which
		// rewrites the log again, ... a permanent, if low-cost (no real
		// ops -- scanner.Scan already excludes .backupdata from the
		// compared tree, see scanner.ExcludedTopLevel), self-sustaining
		// busy-loop that never settles. skipping the whole subtree (not
		// just the directory itself) is correct: nothing under
		// .backupdata is ever meant to be watched.
		if d.Name() == ".backupdata" {
			return filepath.SkipDir
		}
		fw.mu.Lock()
		if fw.pollFallback || (fw.maxDirs > 0 && fw.watchedDirs >= fw.maxDirs) {
			fw.pollFallback = true
			fw.mu.Unlock()
			return nil // over threshold: stop adding real watches (section 7/14)
		}
		fw.watchedDirs++
		fw.mu.Unlock()
		_ = fw.w.Add(p) // best-effort; a failed watch on one subdir doesn't abort the walk
		return nil
	})
}

func (fw *fsWatcher) debounceLoop(debounce time.Duration) {
	var timer *time.Timer
	reset := func() {
		if timer == nil {
			timer = time.NewTimer(debounce)
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(debounce)
	}
	var timerC <-chan time.Time

	for {
		select {
		case ev, ok := <-fw.w.Events:
			if !ok {
				return
			}
			// v3.0 fix, take 2 (live-verified cs_26.08.11): the
			// addRecursive SkipDir above is necessary but NOT
			// sufficient on Windows -- fsnotify's Windows backend
			// (ReadDirectoryChangesW) watches with bWatchSubtree=TRUE,
			// meaning a single Add() on primaryPath2 already recursively
			// observes everything below it, INCLUDING .backupdata, even
			// though this process never explicitly registered a watch
			// there. Confirmed by re-running the exact same idle sync-
			// mode test after the SkipDir fix: events kept firing every
			// ~500ms indefinitely, unchanged. The only backend-portable
			// fix is filtering by EVENT PATH, not by which directories
			// were explicitly registered -- do that here, before the
			// event is allowed to reset the debounce timer at all.
			if isBackupdataPath(ev.Name) {
				continue
			}
			if ev.Op&fsnotify.Create == fsnotify.Create {
				// best-effort: newly created dir needs its own watch too
				fw.addRecursive(ev.Name)
			}
			reset()
			timerC = timer.C
		case <-timerC:
			fw.emit("event")
			timerC = nil
		case <-fw.w.Errors:
			// swallow -- errors are logged by the caller via Changed()'s
			// next reconcile pass finding nothing to do; not fatal
		case <-fw.closed:
			return
		}
	}
}

func (fw *fsWatcher) safetyNetLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			fw.emit("safety-net")
		case <-fw.closed:
			return
		}
	}
}

// emit sends reason on out, coalescing if a signal is already pending.
//
// BUG FIX 2026.07.28 (audit finding): a plain "send or drop" coalesce
// (the previous implementation) can silently drop a "safety-net"/"sighup"
// reason in favor of an already-queued "event" -- main.go's doPass()
// gates the periodic existing-folder ACL re-sync specifically on
// reason=="safety-net"||"sighup" (deliberately NOT run on every fast
// event pass, see that function's doc comment), so losing the distinct
// reason string could skip that re-sync for up to a full --rescan
// interval (default 24h) if an "event" happened to be sitting unconsumed
// in the channel at the exact moment the safety-net ticker fired. Fix:
// when the channel is full, drain-and-compare instead of dropping --
// keep whichever reason is more significant ("event" never overwrites a
// pending safety-net/sighup/start; anything else may upgrade a pending
// "event"). Identical logic in watch_eventport_illumossolaris.go.
func (fw *fsWatcher) emit(reason string) {
	select {
	case fw.out <- reason:
		return
	default:
	}
	select {
	case old := <-fw.out:
		keep := reason
		if old != "event" && reason == "event" {
			keep = old // don't downgrade a more significant pending reason
		}
		select {
		case fw.out <- keep:
		default:
			select {
			case fw.out <- keep:
			default:
			}
		}
	default:
		select {
		case fw.out <- reason:
		default:
		}
	}
}

func (fw *fsWatcher) Changed() <-chan string { return fw.out }

func (fw *fsWatcher) Close() error {
	fw.closeOnce.Do(func() { close(fw.closed) })
	return fw.w.Close()
}
