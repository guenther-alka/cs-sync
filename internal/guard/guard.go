// Package guard implements the two sender-side safety mechanisms of
// cs-sync-2.0-design.info section 5 (MASS-DELETE PROTECTION) plus the
// per-file retry quarantine of section 3 / section 10.
package guard

import (
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ---- section 5a: source-liveness check -------------------------------------

// SourceID identifies the filesystem the source root lived on at setup
// time. On ZFS every dataset is its own filesystem with its own device
// id, so if the dataset fails to mount and cs-sync sees the bare
// mountpoint directory of the PARENT filesystem instead, Dev differs and
// the pass is refused before any delete can propagate. Cross-platform via
// os.Stat (no cgo, no statvfs) -- on Windows Dev is 0 and the check is a
// no-op (no ZFS acltype on Windows anyway, section 2 of the 1.x doc).
type SourceID struct {
	Dev uint64
}

// FreezeFile is created in the pair's state dir when the delete budget
// trips; while it exists every pass aborts (deliberately annoying,
// deliberately fail-closed -- section 5b). The operator confirms the mass
// delete by RENAMING it to ApproveFile: merely deleting FreezeFile would
// re-trip the same budget on the very next pass (found live in sandbox
// testing 2026.07.24 -- endless freeze loop). ApproveFile is a ONE-SHOT
// pass-through: the next pass skips the budget check once and consumes
// (deletes) the file. Restart-safe, explicit, cannot linger.
const FreezeFile = "delete-freeze.txt"

// ApproveFile: see FreezeFile.
const ApproveFile = "delete-approve.txt"

// CheckSource compares the current device id of root against the recorded
// one. recorded==0 means "not recorded yet" (first run): record now.
func CheckSource(root string, recorded *SourceID, dev func(string) (uint64, error)) error {
	d, err := dev(root)
	if err != nil {
		return fmt.Errorf("source liveness: cannot stat %s: %w", root, err)
	}
	if d == 0 { // platform without dev ids (Windows) -- check disabled
		return nil
	}
	if recorded.Dev == 0 {
		recorded.Dev = d
		return nil
	}
	if recorded.Dev != d {
		return fmt.Errorf("source liveness: %s is on device %d, expected %d -- source dataset not mounted? REFUSING pass", root, d, recorded.Dev)
	}
	return nil
}

// ---- section 5b: delete budget ---------------------------------------------

// Budget holds the delete thresholds (defaults per section 10 decisions
// log: count>1000 OR percent>20, OR-combined).
type Budget struct {
	MaxCount   int // 0 disables the count check
	MaxPercent int // 0 disables the percent check
}

// Check returns an error when nDeletes exceeds the budget relative to the
// known tree size. treeSize is the number of baseline entries BEFORE the
// deletes.
func (b Budget) Check(nDeletes, treeSize int) error {
	if nDeletes == 0 {
		return nil
	}
	if b.MaxCount > 0 && nDeletes > b.MaxCount {
		return fmt.Errorf("delete budget: pass wants to delete %d entries (> max %d)", nDeletes, b.MaxCount)
	}
	if b.MaxPercent > 0 && treeSize > 0 {
		pct := nDeletes * 100 / treeSize
		if pct > b.MaxPercent {
			return fmt.Errorf("delete budget: pass wants to delete %d of %d entries (%d%% > max %d%%)", nDeletes, treeSize, pct, b.MaxPercent)
		}
	}
	return nil
}

// Freeze writes the freeze marker; Frozen reports whether it exists.
func Freeze(stateDir, reason string) error {
	p := filepath.Join(stateDir, FreezeFile)
	body := fmt.Sprintf("cs-sync delete-budget FREEZE %s\n%s\n\nAll sync passes for this relationship are PAUSED.\nIf the mass delete is INTENTIONAL, rename this file to %s --\nthe next pass will then run once without the delete budget and\nconsume that file. Merely deleting this file re-freezes on the\nnext pass (the pending deletes still exceed the budget).\n", time.Now().Format(time.RFC3339), reason, ApproveFile)
	return os.WriteFile(p, []byte(body), 0600)
}

func Frozen(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, FreezeFile))
	return err == nil
}

// ConsumeApproval reports whether the operator placed ApproveFile, and
// consumes it (one-shot). It also clears a still-present FreezeFile so
// the relationship resumes cleanly.
func ConsumeApproval(stateDir string) bool {
	p := filepath.Join(stateDir, ApproveFile)
	if _, err := os.Stat(p); err != nil {
		return false
	}
	os.Remove(p)
	os.Remove(filepath.Join(stateDir, FreezeFile))
	return true
}

// ---- section 3 / 10: per-file retry quarantine ------------------------------

// MaxAttempts is N from the section 10 decisions log.
const MaxAttempts = 10

// RetryState tracks failing paths across restarts (persisted per pair).
type RetryState struct {
	Items map[string]*RetryItem
}

type RetryItem struct {
	Attempts    int
	NextTryUnix int64
	Quarantined bool
	LastErr     string
}

// Backoff returns the wait before the next retry: 1s, 2s, 4s ... capped
// at 5min (section 10).
func Backoff(attempts int) time.Duration {
	d := time.Second << uint(attempts)
	if d > 5*time.Minute || d <= 0 {
		d = 5 * time.Minute
	}
	return d
}

// Fail records a failure for path; returns true if it just got quarantined.
func (r *RetryState) Fail(path, errText string) bool {
	if r.Items == nil {
		r.Items = map[string]*RetryItem{}
	}
	it := r.Items[path]
	if it == nil {
		it = &RetryItem{}
		r.Items[path] = it
	}
	it.Attempts++
	it.LastErr = errText
	it.NextTryUnix = time.Now().Add(Backoff(it.Attempts)).Unix()
	if it.Attempts >= MaxAttempts && !it.Quarantined {
		it.Quarantined = true
		return true
	}
	return false
}

// Skip reports whether path should be skipped this pass (quarantined, or
// still inside its backoff window).
func (r *RetryState) Skip(path string) bool {
	if r.Items == nil {
		return false
	}
	it, ok := r.Items[path]
	if !ok {
		return false
	}
	if it.Quarantined {
		return true
	}
	return time.Now().Unix() < it.NextTryUnix
}

// Clear forgets a path after a successful transfer.
func (r *RetryState) Clear(path string) {
	if r.Items != nil {
		delete(r.Items, path)
	}
}

// QuarantinedCount feeds the single aggregate GUI alert (section 10).
func (r *RetryState) QuarantinedCount() int {
	n := 0
	for _, it := range r.Items {
		if it.Quarantined {
			n++
		}
	}
	return n
}

const retryFile = "retry.state"

// LoadRetry / SaveRetry persist the retry state in the pair's state dir.
func LoadRetry(stateDir string) *RetryState {
	r := &RetryState{Items: map[string]*RetryItem{}}
	f, err := os.Open(filepath.Join(stateDir, retryFile))
	if err != nil {
		return r
	}
	defer f.Close()
	_ = gob.NewDecoder(f).Decode(r) // corrupt -> start fresh
	if r.Items == nil {
		r.Items = map[string]*RetryItem{}
	}
	return r
}

func SaveRetry(stateDir string, r *RetryState) error {
	tmp := filepath.Join(stateDir, retryFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, filepath.Join(stateDir, retryFile))
}
