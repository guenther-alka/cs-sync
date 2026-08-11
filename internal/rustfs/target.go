package rustfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Target describes a RustFS bucket to sync with: host/port/bucket plus
// inline S3 credentials (v3.0 -- replaces the v2.1 model of a manually
// pre-configured named rclone remote, see endpoint.Endpoint and the
// --key/--primary_key/--secondary_key/--backup_key flags in main.go).
//
// Under the hood this package still drives rclone via a NAMED remote
// (not a hand-built ":s3,param=val,...:bucket" inline connection
// string) -- deliberately: the endpoint URL itself contains a ":"
// (https://host:port), and rclone's inline-connection-string parser's
// exact quoting/escaping rules for that case were not something this
// package could live-verify with the time available, whereas the named-
// remote path is the exact mechanism already live-tested successfully
// against a real RustFS instance (192.168.2.189, cs_26.08.11). See
// EnsureRemote: cs-sync creates/updates that named remote itself, once,
// non-interactively -- so this is still fully automatic from the
// operator's point of view, just safer under the hood.
type Target struct {
	Host   string
	Port   int
	Bucket string
	Access string
	Secret string
}

// remoteName derives a deterministic, filesystem/rclone-safe remote name
// from Host+Port, so the same Target always maps to the same rclone
// remote (idempotent EnsureRemote calls, no collision between different
// RustFS endpoints used by the same cs-sync process or across separate
// relationships on the same host). Not derived from Bucket -- several
// buckets on the same RustFS instance legitimately share one remote.
func (t Target) remoteName() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", t.Host, t.Port)))
	return "cssync_" + hex.EncodeToString(h[:])[:16]
}

// EnsureRemote creates or updates this Target's rclone remote
// non-interactively (idempotent -- "config create" overwrites an
// existing same-named remote's fields rather than erroring). Must be
// called at least once before any Scan/CopyTo/CopyFrom/Delete/Resync/
// PullSync call using this Target -- callers in main.go do this once at
// startup per configured RustFS-typed endpoint.
func (t Target) EnsureRemote() error {
	return runRclone("config", "create", t.remoteName(), "s3",
		"provider=Other",
		"access_key_id="+t.Access,
		"secret_access_key="+t.Secret,
		"endpoint=https://"+t.HostPort(),
		"--non-interactive",
	)
}

// HostPort renders "host:port".
func (t Target) HostPort() string {
	return fmt.Sprintf("%s:%d", t.Host, t.Port)
}

// root returns "<remote-name>:<bucket>", the rclone path for the bucket
// root (used by Scan/Resync/PullSync).
func (t Target) root() string {
	return t.remoteName() + ":" + t.Bucket
}

// object returns "<remote-name>:<bucket>/<objectPath>" for one object
// (used by CopyTo/CopyFrom/Delete/rename handling in executor_rustfs.go
// and sender_rustfs.go).
func (t Target) object(objectPath string) string {
	return t.root() + "/" + objectPath
}
