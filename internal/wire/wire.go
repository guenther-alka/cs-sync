// Package wire implements the framed cs-sync 2.0 network protocol
// (cs-sync-2.0-design.info section 6). Encryption/auth is NOT done here:
// the connection is expected to run through a cs-stream encrypted tunnel
// (tunnel-listen / tunnel-send); cs-sync itself binds loopback by default.
//
// Frame layout: 1 byte type, 4 bytes big-endian payload length, payload
// (gob-encoded message struct). CHUNK-AWARE FROM DAY 1 (section 6): a file
// transfer is a FileBegin followed by one FileData frame PER BLOCK, each
// carrying (offset, data, per-block hash) -- v2.0 always sends all blocks
// sequentially and the receiver writes them strictly in order (trivially
// correct); v2.1 delta only adds a skip negotiation, the frame format
// itself stays unchanged.
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

// Conn wraps a net.Conn with buffered framed gob I/O.
type Conn struct {
	c net.Conn
	r *bufio.Reader
	w *bufio.Writer
}

func New(c net.Conn) *Conn {
	return &Conn{c: c, r: bufio.NewReaderSize(c, 256*1024), w: bufio.NewWriterSize(c, 256*1024)}
}

func (cn *Conn) Close() error {
	if cn == nil || cn.c == nil {
		return nil
	}
	return cn.c.Close()
}

// Send writes one frame and flushes.
func (cn *Conn) Send(t byte, msg any) error {
	var buf stdbytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(msg); err != nil {
		return fmt.Errorf("encode frame %d: %w", t, err)
	}
	var hdr [5]byte
	hdr[0] = t
	binary.BigEndian.PutUint32(hdr[1:], uint32(buf.Len()))
	if _, err := cn.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := cn.w.Write(buf.Bytes()); err != nil {
		return err
	}
	return cn.w.Flush()
}

// Recv reads one frame; the caller decodes the payload per the returned type.
func (cn *Conn) Recv() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(cn.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > 2*BlockSize+65536 {
		return 0, nil, fmt.Errorf("frame too large: %d", n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(cn.r, p); err != nil {
		return 0, nil, err
	}
	return hdr[0], p, nil
}

// Decode gob-decodes a received payload into out.
func Decode(payload []byte, out any) error {
	return gob.NewDecoder(stdbytes.NewReader(payload)).Decode(out)
}
