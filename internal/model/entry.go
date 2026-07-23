// Package model defines the metadata cs-sync tracks per filesystem entry.
package model

const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
)

// Entry is the metadata cs-sync tracks per path (relative to a sync root).
// See cs-sync.info section 4 (METADATA MODEL).
type Entry struct {
	Path       string // relative to sync root, forward slashes
	Type       string // file | dir | symlink
	Size       int64
	MtimeNS    int64  // mtime in nanoseconds since epoch
	Mode       uint32 // permission bits incl. executable bit
	Dev        uint64 // device id, for rename detection guard
	Ino        uint64 // inode number, for rename detection (section 8)
	LinkTarget string // symlink target, if Type == symlink
	ACL        string // folder ACL text, dirs only (section 10)
	Hash       string // lazy content hash (BLAKE3 hex), empty until computed
}

// Tree is a full snapshot of one side: relpath -> Entry.
type Tree map[string]Entry

// State is the persisted baseline (cs-sync.state, see section 5).
type State struct {
	Baseline Tree
	// AclType is the ZFS acltype detected at startup ("posix" | "nfs4").
	AclType string
}

// SameIdentity reports whether two entries are identical by the fast-path
// check used for change detection: size + mtime + type (section 4).
// Hash is intentionally NOT part of the fast path (lazy hashing decision).
func SameIdentity(a, b Entry) bool {
	if a.Type != b.Type {
		return false
	}
	switch a.Type {
	case TypeDir:
		return true // dir identity is its existence + ACL, checked separately
	case TypeSymlink:
		return a.LinkTarget == b.LinkTarget
	default:
		return a.Size == b.Size && a.MtimeNS == b.MtimeNS
	}
}
