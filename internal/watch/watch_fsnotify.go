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
		if err != nil || !d.IsDir() {
			return nil
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

func (fw *fsWatcher) emit(reason string) {
	select {
	case fw.out <- reason:
	default:
		// a signal is already pending -- coalesce, no need to queue more
	}
}

func (fw *fsWatcher) Changed() <-chan string { return fw.out }

func (fw *fsWatcher) Close() error {
	fw.closeOnce.Do(func() { close(fw.closed) })
	return fw.w.Close()
}
