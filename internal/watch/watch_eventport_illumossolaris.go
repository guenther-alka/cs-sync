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
	}
	for _, root := range roots {
		pw.addRecursive(root)
	}

	go pw.eventLoop()
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

func (pw *portWatcher) eventLoop() {
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

		pw.emit("event")
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

func (pw *portWatcher) emit(reason string) {
	select {
	case pw.out <- reason:
	default:
		// a signal is already pending -- coalesce, matches the fsnotify watcher
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
