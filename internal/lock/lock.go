// Package lock provides a simple cross-platform advisory file lock used to
// serialize state.Save/state.Load/WriteACLCSV/MirrorACLCSV access to a
// shared primaryRoot/.backupdata directory.
//
// WHY THIS EXISTS (found live, OmniOS .189, 2026.07.27/28 test round --
// see cs-sync-2.0-design.info section 17 "CRITICAL FINDING: illumos race
// condition"): two cs-sync processes sharing the same --primary path
// (a bidir process + a local-path "Backup" oneway process, exactly what
// action.pl's _cssync_svc_start spawns for that config) both read/write
// <primary>/.backupdata/cs-sync.state and cs-sync-acl.csv with NO
// coordination. Both writers used the SAME fixed ".tmp" filename, so a
// concurrent Save() from one process could be interleaved with or
// overwritten by the other's Save(), corrupting the persisted baseline
// with no I/O error at all -- the corruption is a silent LOGICAL one
// (wrong baseline content), not a crash, so nothing in the existing
// error handling ever caught it. Reproduced as complete silent data loss
// (test file vanished from primary+secondary+backup, zero WARN/ERROR).
//
// FIX: a simple O_CREATE|O_EXCL lockfile per primaryRoot, held for the
// duration of the load-reconcile-save critical section in main.go's
// doPass(). No cgo, no per-OS syscall (flock/LockFileEx) needed -- plain
// O_EXCL semantics are atomic on every filesystem cs-sync targets
// (ZFS/illumos, ZFS/FreeBSD, ext4/Linux, APFS/macOS, NTFS/ReFS/Windows).
// Stale-lock recovery: if the lock is older than staleAfter, it is assumed
// to be left over from a crashed process and is stolen (removed + retried)
// rather than blocking forever.
package lock

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	fileName    = "cs-sync.lock"
	retryDelay  = 20 * time.Millisecond
	staleAfter  = 30 * time.Second
	acquireWait = 10 * time.Second // give up and steal if we can't get in cleanly by then
)

// Lock is a held advisory lock; release it with Unlock().
type Lock struct {
	path string
}

// Acquire takes the lock for backupdataDir (the .backupdata directory
// already created by state.Dir). Blocks with short retries until acquired,
// stealing a stale lock (older than staleAfter) if one is found.
func Acquire(backupdataDir string) (*Lock, error) {
	p := filepath.Join(backupdataDir, fileName)
	deadline := time.Now().Add(acquireWait)

	for {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return &Lock{path: p}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create lock %s: %w", p, err)
		}

		// lock file exists -- check staleness
		if fi, statErr := os.Stat(p); statErr == nil {
			if time.Since(fi.ModTime()) > staleAfter {
				os.Remove(p) // steal: crashed holder never cleaned up
				continue
			}
		}

		if time.Now().After(deadline) {
			// last resort: steal anyway rather than deadlock the process.
			// A held-too-long lock is itself a bug elsewhere; refusing to
			// ever proceed would turn one wedged process into two.
			os.Remove(p)
			continue
		}
		time.Sleep(retryDelay)
	}
}

// Unlock releases the lock. Safe to call on a nil Lock (no-op).
func (l *Lock) Unlock() {
	if l == nil {
		return
	}
	os.Remove(l.path)
}
