package rustfs

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// runRclone runs an rclone subcommand and returns a wrapped error
// including stderr on failure, matching the error-wrapping style used
// throughout this package's other files (and the project's existing
// internal/acl subprocess callers).
//
// --no-check-certificate is always appended (live-verified cs_26.08.11
// against a real RustFS instance on 192.168.2.189: RustFS in this
// cluster serves HTTPS with a self-signed certificate, and rclone hangs
// indefinitely -- not a clean error -- on the TLS handshake without this
// flag). This matches the established convention already used
// throughout the rest of napp-it CS's own rclone/S3 tooling (get_async.pl,
// job-s3_backup.pl, job-s3_restore.pl, s3_worker_backup_template.pl) --
// admin.pl's changelog documents the same failure mode as a live
// incident there (2026-06-24, "InvalidCertificate(...CaUsedAsEndEntity)").
// Unconditional, not configurable: this package is only ever used for
// RustFS endpoints (see package doc comment), never a generic S3
// provider, so there is no case where a real (non-self-signed) cert
// would need strict verification here.
func runRclone(args ...string) error {
	args = append(args, "--no-check-certificate")
	cmd := exec.Command("rclone", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rclone %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CopyTo pushes one local file to the given target's object path (e.g.
// bucket "s3share", objectPath "reports/q1.pdf"), for the local-changed
// -> RustFS-side direction of a Secondary=rustfs pair (sync-2.1-design.info
// section 4.3).
func CopyTo(t Target, localPath, objectPath string) error {
	return runRclone("copyto", localPath, t.object(objectPath))
}

// CopyFrom pulls one RustFS object down to a local file path, for the
// RustFS-changed -> local-side direction.
func CopyFrom(t Target, objectPath, localPath string) error {
	return runRclone("copyto", t.object(objectPath), localPath)
}

// Delete removes a single object on the RustFS side.
func Delete(t Target, objectPath string) error {
	return runRclone("deletefile", t.object(objectPath))
}

// Resync establishes the initial baseline between a local folder and a
// RustFS bucket at service start, before switching to the event-driven
// model (sync-2.1-design.info section 7). Uses rclone's own bisync
// --resync rather than reimplementing baseline establishment -- rclone
// already understands the S3 side natively (ETags, multipart, etc.).
// ONLY appropriate for a genuinely BIDIRECTIONAL relationship
// (Secondary=rustfs, local-only) -- for a one-way --pull relationship,
// use PullSync instead (bisync could otherwise push local-only content
// INTO the remote bucket, which a pull source must never receive from
// this side).
func Resync(localPath string, t Target) error {
	return runRclone("bisync", localPath, t.root(), "--resync")
}

// PullSync performs a one-way initial mirror FROM a RustFS bucket INTO a
// local folder (sync-2.1-design.info section 11, --pull): makes
// localPath match the bucket exactly, including deleting local-only
// content that isn't in the bucket. Used once at startup for a --pull
// relationship, before switching to the event-driven model (webhook
// and/or --rescan) -- the ongoing ops after this go through
// executor_rustfs.go's normal per-object CopyFrom/Delete instead.
func PullSync(t Target, localPath string) error {
	return runRclone("sync", t.root(), localPath)
}
