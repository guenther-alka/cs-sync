//go:build illumos || solaris

package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// portWatcher is the illumos/Solaris implementation using native Event
// Ports (port_create/port_associate/port_get), via golang.org/x/sys/unix's
// EventPort wrapper. No cgo required: illumos/Solaris route these calls
// through libc via Go's dynamic-symbol-import mechanism, which is plain
// Go as far as the toolchain and cross-compilation are concerned (the
// EventPort type lives in a file suffixed "_solaris", but Go's build
// constraint system treats GOOS=illumos as implying "solaris" for build
// tag purposes, so the same code serves both).
//
// FEN (File Events Notification, PORT_SOURCE_FILE) semantics: an
// association fires AT MOST ONCE -- after port_get returns an event for
// a path, that path is no longer watched until re-associated. This
// watcher re-associates the fired directory (and re-walks it for any
// new subdirectories) immediately after each event, so watches are
// continuously renewed.
type portWatcher struct {
	ep     *unix.EventPort
	out    chan string
	closed chan struct{}

	// raw is fed by pollLoop on every fired event; debounceLoop coalesces
	// bursts of raw signals into a single "event" emission after the
	// configured debounce window has passed with no further signals --
	// see BUG FIX 2026.07.28 comment on debounceLoop below.
	raw chan struct{}

	mu      sync.Mutex
	watched map[string]bool
}

const fenEvents = unix.FILE_MODIFIED | unix.FILE_ATTRIB

// New creates an Event-Ports-based watcher and recursively associates
// every directory under each root. See cs-sync.info section 7 (the
// illumos/Solaris path was originally documented as poll-only in v1;
// this native implementation replaces that, added the same day after
// establishing x/sys/unix.EventPort needs no cgo).
func New(roots []string, opt Options) (Watcher, error) {
	opt = opt.withDefaults()
	ep, err := unix.NewEventPort()
	if err != nil {
		return nil, err
	}
	pw := &portWatcher{
		ep:      ep,
		out:     make(chan string, 1),
		closed:  make(chan struct{}),
		watched: map[string]bool{},
		raw:     make(chan struct{}, 1),
	}
	for _, root := range roots {
		pw.addRecursive(root)
	}

	go pw.pollLoop()
	go pw.debounceLoop(opt.Debounce)
	go pw.safetyNetLoop(opt.SafetyNet)
	go func() { pw.out <- "start" }() // sync once at startup, section 6

	return pw, nil
}

// addRecursive walks dir and associates every directory found that isn't
// already watched. Called at startup for each root, and again (on the
// directory that just fired) after every event, to pick up newly created
// subdirectories -- mirroring fsnotify's "watch new dirs on Create".
func (pw *portWatcher) addRecursive(dir string) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		pw.associate(p)
		return nil
	})
}

func (pw *portWatcher) associate(path string) {
	pw.mu.Lock()
	already := pw.watched[path]
	pw.mu.Unlock()
	if already {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return // gone already -- nothing to watch
	}
	if err := pw.ep.AssociatePath(path, fi, fenEvents, nil); err != nil {
		return // best-effort, same as fsnotify.Add failures elsewhere
	}
	pw.mu.Lock()
	pw.watched[path] = true
	pw.mu.Unlock()
}

// pollLoop blocks on port_get (via ep.GetOne) and re-associates each fired
// directory, feeding a raw signal to debounceLoop for every event. The
// actual filesystem re-walk (addRecursive) happens here, off the
// debounce-timer-management loop, so a burst of many events doesn't
// delay the timer bookkeeping.
func (pw *portWatcher) pollLoop() {
	for {
		pe, err := pw.ep.GetOne(nil) // blocks until an event or Close()
		if err != nil {
			select {
			case <-pw.closed:
				return
			default:
				time.Sleep(100 * time.Millisecond) // avoid a hot loop on transient errors
				continue
			}
		}
		path := pe.Path
		pw.mu.Lock()
		delete(pw.watched, path) // FEN semantics: firing dissociates
		pw.mu.Unlock()

		// re-associate the fired directory and pick up any new subdirs
		pw.addRecursive(path)

		select {
		case pw.raw <- struct{}{}:
		default:
			// a raw signal is already pending -- debounceLoop hasn't
			// consumed it yet, no need to queue more (it just resets a
			// timer regardless of how many events arrived).
		}
	}
}

// debounceLoop coalesces bursts of raw fire signals into one "event"
// emission per debounce window, mirroring watch_fsnotify.go's
// debounceLoop exactly.
//
// BUG FIX 2026.07.28 (audit finding): this loop did not exist before --
// the illumos/Solaris watcher called pw.emit("event") directly from
// eventLoop on EVERY single fired FEN association, with no debounce
// timer at all, unlike every other platform (fsnotify-based watchers all
// share watch_fsnotify.go's debounceLoop). In practice this meant a bulk
// operation generating many filesystem events in quick succession (large
// copy, archive extraction, etc.) triggered a SEPARATE full three-way
// reconcile pass (a full recursive scan of both primary and secondary
// trees) for close to EVERY individual event, instead of settling into a
// small number of passes after the burst quiets down -- exactly the
// "excessive rescan cost" the debounce window exists to avoid per
// cs-sync.info section 7, and a real regression specifically on illumos/
// Solaris, this project's flagship reference platform.
func (pw *portWatcher) debounceLoop(debounce time.Duration) {
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
		case <-pw.raw:
			reset()
			timerC = timer.C
		case <-timerC:
			pw.emit("event")
			timerC = nil
		case <-pw.closed:
			return
		}
	}
}

func (pw *portWatcher) safetyNetLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			pw.emit("safety-net")
		case <-pw.closed:
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
// "event").
func (pw *portWatcher) emit(reason string) {
	select {
	case pw.out <- reason:
		return
	default:
	}
	select {
	case old := <-pw.out:
		keep := reason
		if old != "event" && reason == "event" {
			keep = old // don't downgrade a more significant pending reason
		}
		select {
		case pw.out <- keep:
		default:
			// consumer drained it between our two selects -- try once more,
			// non-blocking; if that also fails, drop rather than risk
			// blocking emit() forever (matches the original best-effort
			// coalescing contract).
			select {
			case pw.out <- keep:
			default:
			}
		}
	default:
		// consumer drained the pending value between our two selects --
		// the channel is empty now, so just send directly.
		select {
		case pw.out <- reason:
		default:
		}
	}
}

func (pw *portWatcher) Changed() <-chan string { return pw.out }

func (pw *portWatcher) Close() error {
	select {
	case <-pw.closed:
		return nil
	default:
		close(pw.closed)
	}
	return pw.ep.Close()
}
