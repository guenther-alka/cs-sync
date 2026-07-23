//go:build illumos || solaris

package watch

import "time"

// pollWatcher is used on illumos/Solaris, where the fsnotify library has
// no FEN (File Event Notification) backend. v1 LIMITATION (documented,
// not silently degraded): there is no true event-driven push here -- the
// "event debounce window" collapses to a fixed poll interval instead, and
// every tick triggers a full reconcile pass. This is functionally the
// same three-way reconcile as the fsnotify platforms, just triggered on a
// timer instead of a kernel event. See cs-sync.info section 7 (marked as
// a documented v1 gap, not present in the original section 7 text --
// added here because native FEN integration needs cgo or manual syscall
// wrapping that was out of scope for this implementation pass).
type pollWatcher struct {
	out    chan string
	closed chan struct{}
}

func New(roots []string, opt Options) (Watcher, error) {
	opt = opt.withDefaults()
	pw := &pollWatcher{
		out:    make(chan string, 1),
		closed: make(chan struct{}),
	}
	interval := opt.Debounce
	if interval < 2*time.Second {
		interval = 2 * time.Second // polling every 500ms would be wasteful; floor at 2s
	}
	go pw.loop(interval, opt.SafetyNet)
	go func() { pw.out <- "start" }()
	return pw, nil
}

func (pw *pollWatcher) loop(interval, safetyNet time.Duration) {
	pollTicker := time.NewTicker(interval)
	safetyTicker := time.NewTicker(safetyNet)
	defer pollTicker.Stop()
	defer safetyTicker.Stop()
	for {
		select {
		case <-pollTicker.C:
			pw.emit("poll")
		case <-safetyTicker.C:
			pw.emit("safety-net")
		case <-pw.closed:
			return
		}
	}
}

func (pw *pollWatcher) emit(reason string) {
	select {
	case pw.out <- reason:
	default:
	}
}

func (pw *pollWatcher) Changed() <-chan string { return pw.out }

func (pw *pollWatcher) Close() error {
	close(pw.closed)
	return nil
}
