package rustfs

import (
	"os"
	"path/filepath"
	"strings"
)

// xlMetaFile is RustFS/MinIO's per-object metadata file. Its presence in
// a directory is what marks that directory as an S3 object's root on
// disk. This is an internal RustFS on-disk convention, not a stable
// public API -- accepted deliberately for this design, see
// sync-2.1-design.info section 4.2 and section 6 (scope: local RustFS
// only, a controlled deployment this project already manages, not a
// third-party API surface).
const xlMetaFile = "xl.meta"

// ResolveObjectKey walks UP from changedPath (an arbitrary path somewhere
// under bucketRoot, as reported by a raw filesystem watch event) until it
// finds a directory that directly contains an xl.meta file, and returns
// that directory's path relative to bucketRoot (forward slashes) as the
// S3 object key. Returns ok=false if changedPath is not under bucketRoot,
// or no xl.meta is found before reaching bucketRoot itself (e.g. the
// event was for some other non-object artifact).
//
// This is NOT on cs-sync's critical trigger path (see doc comment on
// watch integration in watch_integration.go) -- the existing watcher
// already collapses all events into a debounced "something changed"
// signal, and Scan (scan.go) rebuilds the RustFS-side tree from scratch
// via `rclone lsjson` on every pass regardless of which specific object
// changed, matching cs-sync's own v1 "dumb trigger, full rescan"
// philosophy (see internal/watch's package doc). ResolveObjectKey exists
// for diagnostics/logging ("what object triggered this pass") and as a
// building block for a possible future more granular dispatch, not
// because the current design requires per-event object identification.
func ResolveObjectKey(changedPath, bucketRoot string) (key string, ok bool) {
	changedPath = filepath.Clean(changedPath)
	bucketRoot = filepath.Clean(bucketRoot)

	rel, err := filepath.Rel(bucketRoot, changedPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false // changedPath is not under bucketRoot at all
	}

	dir := changedPath
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		dir = filepath.Dir(dir)
	}

	for {
		if dir == bucketRoot {
			return "", false // reached the bucket root without finding xl.meta
		}
		if _, err := os.Stat(filepath.Join(dir, xlMetaFile)); err == nil {
			rel, err := filepath.Rel(bucketRoot, dir)
			if err != nil {
				return "", false
			}
			return filepath.ToSlash(rel), true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false // hit filesystem root, safety guard against infinite loop
		}
		dir = parent
	}
}
