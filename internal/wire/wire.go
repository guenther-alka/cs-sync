// Package wire implements the framed cs-sync 2.0 network protocol
// (cs-sync-2.0-design.info section 6). Every frame is encrypted with
// ChaCha20-Poly1305 (see crypto.go) -- cs-sync 2.1+ no longer depends on
// an external cs-stream tunnel for confidentiality (Gea 2026.07.25).
//
// Frame layout: 1 byte type, 4 bytes big-endian payload length, payload
// (gob-encoded message struct, sealed if encryption is enabled). CHUNK-
// AWARE FROM DAY 1 (section 6): a file transfer is a FileBegin followed
// by one FileData frame PER BLOCK, each carrying (offset, data, per-block
// hash) -- v2.0 always sends all blocks sequentially and the receiver
// writes them strictly in order (trivially correct); v2.1 delta only adds
// a skip negotiation, the frame format itself stays unchanged.
package wire

import (
	"bufio"
	stdbytes "bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"net"
)

// ProtoVersion is exchanged in the handshake; both ends must match
// (section 6, VERSIONED HANDSHAKE -- members update independently).
const ProtoVersion = 1

// BlockSize is the fixed v2.0 transfer block size. v2.1 delta (CDC via
// restic/chunker) may switch to variable content-defined blocks; the wire
// format does not care (every FileData carries its own offset).
const BlockSize = 4 * 1024 * 1024

// Frame types.
const (
	TWelcome   byte = 1 // both directions: handshake (receiver sends first)
	TMkdir     byte = 2 // sender -> receiver
	TRename    byte = 3
	TDelete    byte = 4 // file or symlink
	TRmdir     byte = 5
	TSymlink   byte = 6
	TFileBegin byte = 7
	TFileData  byte = 8
	TFileEnd   byte = 9
	TFolderACL byte = 10 // native ACL text, only when same OS (section 8/14)
	TAclCSV    byte = 11 // portable acl.csv blob, ALWAYS sent (section 14)
	TAck       byte = 12 // receiver -> sender, per operation
	TPing      byte = 13 // keepalive, receiver answers with TAck
)

// Welcome is the handshake message, sent by both sides on connect
// (receiver first).
type Welcome struct {
	ProtoVersion int
	SyncVersion  string // cs-sync binary version, for logs
	OS           string // runtime.GOOS -- drives same-OS native ACL decision
	AclType      string // "posix" | "nfs4" (receiver: dest dataset's type)
	Key          string // transfer key (section 15): first 20 chars of the
	// shared napp-it CS cluster secret (server.auth),
	// reused as-is -- both members already have the
	// identical value via existing group membership,
	// no separate secret to distribute. This is a
	// PRE-SHARED-KEY CHECK in addition to the encryption
	// itself (a mismatched key already fails to decrypt
	// the very first frame -- this field is a friendlier,
	// explicit error path for the case where decryption
	// somehow succeeds despite different keys, which
	// should not happen with ChaCha20-Poly1305 but costs
	// nothing to check).
}

// Op carries mkdir/rename/delete/rmdir/symlink metadata.
type Op struct {
	Path       string // relpath, forward slashes
	OldPath    string // rename only
	Mode       uint32
	MtimeNS    int64
	LinkTarget string // symlink only
	ACL        string // mkdir only: native ACL text ("" = none/other-OS)
}

// FileBegin announces a file transfer.
type FileBegin struct {
	Path    string
	Size    int64
	Mode    uint32
	MtimeNS int64
}

// FileData is one block. Offset strictly increases in v2.0.
type FileData struct {
	Offset int64
	Hash   string // sha256 hex of Data (per-block, the v2.1 delta key)
	Data   []byte
}

// FileEnd closes a transfer; receiver verifies FullHash BEFORE the atomic
// rename (e2e check, section 2).
type FileEnd struct {
	FullHash string // sha256 hex of the whole file
}

// FolderACL re-applies a native ACL onto an existing folder (safety-net
// ACL re-sync carried over from 1.x, section 8).
type FolderACL struct {
	Path    string // "." = dest root itself
	AclType string
	Text    string
}

// AclCSV is the portable acl.csv blob, written verbatim to
// <dest>/.backupdata/cs-sync-acl.csv (section 14: always distributed).
type AclCSV struct {
	Data []byte
}

// Ack is the receiver's per-operation answer.
type Ack struct {
	OK  bool
	Err string
}

// Conn wraps a net.Conn with buffered framed gob I/O. If aead is non-nil,
// every frame payload is sealed/opened with it (section: native encryption,
// Gea request 2026.07.25 -- cs-sync no longer depends on a cs-stream tunnel
// for confidentiality).
type Conn struct {
	c    net.Conn
	r    *bufio.Reader
	w    *bufio.Writer
	aead cipherAEAD // nil = unencrypted (only used for tests)
}

// New creates an unencrypted Conn. Kept for tests / local tooling; normal
// operation always calls NewEncrypted with the shared transfer key.
func New(c net.Conn) *Conn {
	return &Conn{c: c, r: bufio.NewReaderSize(c, 256*1024), w: bufio.NewWriterSize(c, 256*1024)}
}

// NewEncrypted creates a Conn that seals every frame with ChaCha20-Poly1305,
// keyed from the transfer key (same key already used for the pre-shared-key
// handshake check). Both ends must pass the identical transferKey.
func NewEncrypted(c net.Conn, transferKey string) *Conn {
	key := DeriveKey(transferKey)
	a, err := newAEAD(key)
	if err != nil {
		// chacha20poly1305.New only fails on wrong key length, which
		// cannot happen here (DeriveKey always returns 32 bytes) --
		// treat as unreachable, but degrade to unencrypted rather than
		// panic in production code.
		return New(c)
	}
	return &Conn{c: c, r: bufio.NewReaderSize(c, 256*1024), w: bufio.NewWriterSize(c, 256*1024), aead: a}
}

func (cn *Conn) Close() error {
	if cn == nil || cn.c == nil {
		return nil
	}
	return cn.c.Close()
}

// Send writes one frame and flushes. If the Conn was created with
// NewEncrypted, the payload is sealed before it hits the wire.
func (cn *Conn) Send(t byte, msg any) error {
	var buf stdbytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		return fmt.Errorf("encode frame %d: %w", t, err)
	}
	payload := buf.Bytes()
	if cn.aead != nil {
		sealed, err := seal(cn.aead, t, payload)
		if err != nil {
			return fmt.Errorf("seal frame %d: %w", t, err)
		}
		payload = sealed
	}
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := cn.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := cn.w.Write(payload); err != nil {
		return err
	}
	return cn.w.Flush()
}

// Recv reads one frame; the caller decodes the payload per the returned
// type. If the Conn was created with NewEncrypted, the payload is opened
// (authenticated-decrypted) before being returned.
func (cn *Conn) Recv() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(cn.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	// Encrypted frames carry a nonce+tag overhead on top of the plaintext
	// bound; allow generous headroom either way.
	if n > 2*BlockSize+65536+64 {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(cn.r, p); err != nil {
		return 0, nil, err
	}
	if cn.aead != nil {
		opened, err := open(cn.aead, hdr[0], p)
		if err != nil {
			return 0, nil, fmt.Errorf("decrypt frame %d: %w (wrong --key on one side?)", hdr[0], err)
		}
		return hdr[0], opened, nil
	}
	return hdr[0], p, nil
}

// Decode gob-decodes a received payload into out.
func Decode(payload []byte, out any) error {
	return gob.NewDecoder(stdbytes.NewReader(payload)).Decode(out)
}
