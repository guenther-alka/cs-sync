package rustfs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/model"
)

// rcloneEntry mirrors the fields `rclone lsjson --recursive` emits that
// this package needs. rclone documents this JSON shape as stable; only a
// subset of its fields are decoded here.
type rcloneEntry struct {
	Path    string `json:"Path"`
	Size    int64  `json:"Size"`
	ModTime string `json:"ModTime"` // RFC3339Nano
	IsDir   bool   `json:"IsDir"`
}

// Scan builds a model.Tree for a Target's bucket, mirroring
// scanner.Scan's signature so both can be used interchangeably by the
// caller depending on which side (filesystem vs RustFS) is being
// scanned.
//
// Deliberately NOT populated: Entry.Ino / Entry.Dev (both left zero).
// This is intentional, not an oversight: rustfs-2.1-design.info section
// 3.2/9 established via a live inode comparison that RustFS has no real
// rename at the storage layer -- every "move" is a fresh create with a
// brand-new random shard UUID, not a preserved identity. Leaving Ino/Dev
// at zero means reconcile's Dev+Ino rename-correlation (cs-sync-2.0
// design doc section 8) will simply never match an RustFS-side entry
// against anything, which is correct: there is no cheaper operation to
// detect on this side, a perceived "rename" is unavoidably a delete+
// create, and reconcile should treat it as exactly that.
//
// Entry.ACL is also left empty -- S3 has no filesystem-ACL concept (see
// sync-2.1-design.info section 10); the destination side's own
// aclinherit=passthrough handles inheritance for newly created files
// with zero help needed from this package.
func Scan(t Target) (model.Tree, error) {
	// --no-check-certificate: see transfer.go's runRclone doc comment
	// (live-verified cs_26.08.11 against a real self-signed-cert RustFS
	// instance) -- same rationale applies here.
	remote := t.root()
	cmd := exec.Command("rclone", "lsjson", "--recursive", remote, "--no-check-certificate")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rclone lsjson %s: %w (%s)", remote, err, strings.TrimSpace(stderr.String()))
	}
	tree, err := parseLsjson(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("rclone lsjson %s: %w", remote, err)
	}
	return tree, nil
}

// parseLsjson contains the actual `rclone lsjson` output parsing, kept
// separate from the exec.Command call above so it can be unit tested
// with canned JSON instead of a real rclone/RustFS instance.
func parseLsjson(data []byte) (model.Tree, error) {
	tree := model.Tree{}

	var entries []rcloneEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode output: %w", err)
	}

	for _, e := range entries {
		if e.Path == "" {
			continue
		}
		rel := strings.TrimPrefix(e.Path, "/")

		entry := model.Entry{
			Path: rel,
		}
		if e.IsDir {
			entry.Type = model.TypeDir
		} else {
			entry.Type = model.TypeFile
			entry.Size = e.Size
			if t, err := time.Parse(time.RFC3339Nano, e.ModTime); err == nil {
				entry.MtimeNS = t.UnixNano()
			}
			// Ino/Dev intentionally left zero -- see doc comment above.
		}
		tree[rel] = entry
	}
	return tree, nil
}
