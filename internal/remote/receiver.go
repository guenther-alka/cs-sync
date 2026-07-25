// Package remote implements the cs-sync 2.0 network leg: the receiver
// ("cs-sync serve", this file) and the sender (sender.go). See
// cs-sync-2.0-design.info sections 3, 4, 6.
package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/acl"
	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/state"
	"github.com/guenther-alka/cs-sync/internal/wire"
)

// Receiver applies incoming operations under Dest. One connection at a
// time (KISS; the sender is a single loop anyway -- queue-level
// parallelism is a later optimization, section 13).
type Receiver struct {
	Dest        string
	AclType     string
	Version     string
	TransferKey string // section 15: pre-shared key check, "" disables it
	Log         *logging.Logger
}

// Serve listens on addr and handles connections until a fatal listener
// error. Non-loopback binds are allowed (cs-stream tunnel-listen usually
// forwards to loopback, but direct LAN use is possible) -- a WARN reminds
// the operator that cs-sync itself does not encrypt (section 6).
func (rv *Receiver) Serve(addr string) error {
	if !strings.HasPrefix(addr, "127.") && !strings.HasPrefix(addr, "localhost") {
		rv.Log.Printf("WARN: listening on %s -- cs-sync does NOT encrypt; use a cs-stream tunnel for anything beyond loopback", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	rv.Log.Printf("cs-sync serve %s: dest=%s acltype=%s listening on %s", rv.Version, rv.Dest, rv.AclType, addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		rv.handle(c) // sequential by design
	}
}

func (rv *Receiver) handle(c net.Conn) {
	defer c.Close()
	cn := wire.New(c)
	rv.Log.Printf("connection from %s", c.RemoteAddr())

	// handshake: receiver first (section 6)
	if err := cn.Send(wire.TWelcome, wire.Welcome{
		ProtoVersion: wire.ProtoVersion, SyncVersion: rv.Version,
		OS: runtime.GOOS, AclType: rv.AclType,
	}); err != nil {
		rv.Log.Printf("handshake send failed: %v", err)
		return
	}
	t, p, err := cn.Recv()
	if err != nil || t != wire.TWelcome {
		rv.Log.Printf("handshake recv failed: t=%d err=%v", t, err)
		return
	}
	var hello wire.Welcome
	if err := wire.Decode(p, &hello); err != nil {
		rv.Log.Printf("handshake decode failed: %v", err)
		return
	}
	if hello.ProtoVersion != wire.ProtoVersion {
		rv.Log.Printf("REFUSING: protocol version mismatch (peer=%d local=%d) -- update both members", hello.ProtoVersion, wire.ProtoVersion)
		_ = cn.Send(wire.TAck, wire.Ack{OK: false, Err: fmt.Sprintf("protocol version mismatch: peer=%d local=%d", hello.ProtoVersion, wire.ProtoVersion)})
		return
	}
	if rv.TransferKey != "" && hello.Key != rv.TransferKey {
		rv.Log.Printf("REFUSING connection from %s: transfer key mismatch", c.RemoteAddr())
		_ = cn.Send(wire.TAck, wire.Ack{OK: false, Err: "transfer key mismatch"})
		return
	}
	// explicit accept ack -- the sender's connect() waits for this before
	// declaring itself connected (bug found in sandbox testing 2026.07.25:
	// without it, the sender logged "remote connected" and only discovered
	// the key mismatch on its FIRST actual data roundtrip, which was
	// confusing/misleading even though nothing was ever transferred).
	if err := cn.Send(wire.TAck, wire.Ack{OK: true}); err != nil {
		rv.Log.Printf("handshake accept-ack send failed: %v", err)
		return
	}
	rv.Log.Printf("peer: cs-sync %s os=%s proto=%d", hello.SyncVersion, hello.OS, hello.ProtoVersion)

	// per-connection transfer state
	var (
		tmpFile *os.File
		tmpPath string
		curPath string
		curMeta wire.FileBegin
		hasher  hash.Hash
	)
	abortTransfer := func() {
		if tmpFile != nil {
			tmpFile.Close()
			os.Remove(tmpPath)
			tmpFile = nil
		}
	}
	defer abortTransfer()

	ack := func(err error) {
		a := wire.Ack{OK: err == nil}
		if err != nil {
			a.Err = err.Error()
		}
		if serr := cn.Send(wire.TAck, a); serr != nil {
			rv.Log.Printf("ack send failed: %v", serr)
		}
	}

	for {
		t, p, err := cn.Recv()
		if err != nil {
			rv.Log.Printf("connection closed: %v", err)
			return
		}
		switch t {
		case wire.TPing:
			ack(nil)

		case wire.TMkdir:
			var op wire.Op
			if err := wire.Decode(p, &op); err != nil {
				ack(err)
				continue
			}
			err := rv.mkdir(op)
			if err != nil {
				rv.Log.Printf("ERROR mkdir %s: %v", op.Path, err)
			}
			ack(err)

		case wire.TRename:
			var op wire.Op
			if err := wire.Decode(p, &op); err != nil {
				ack(err)
				continue
			}
			oldp := rv.full(op.OldPath)
			newp := rv.full(op.Path)
			err := os.MkdirAll(filepath.Dir(newp), 0755)
			if err == nil {
				err = os.Rename(oldp, newp)
			}
			if err != nil {
				rv.Log.Printf("ERROR rename %s -> %s: %v", op.OldPath, op.Path, err)
			}
			ack(err)

		case wire.TDelete:
			var op wire.Op
			if err := wire.Decode(p, &op); err != nil {
				ack(err)
				continue
			}
			err := os.Remove(rv.full(op.Path))
			if os.IsNotExist(err) {
				err = nil
			}
			if err != nil {
				rv.Log.Printf("ERROR delete %s: %v", op.Path, err)
			}
			ack(err)

		case wire.TRmdir:
			var op wire.Op
			if err := wire.Decode(p, &op); err != nil {
				ack(err)
				continue
			}
			err := os.RemoveAll(rv.full(op.Path))
			if err != nil {
				rv.Log.Printf("ERROR rmdir %s: %v", op.Path, err)
			}
			ack(err)

		case wire.TSymlink:
			var op wire.Op
			if err := wire.Decode(p, &op); err != nil {
				ack(err)
				continue
			}
			full := rv.full(op.Path)
			os.Remove(full) // replace if exists
			err := os.MkdirAll(filepath.Dir(full), 0755)
			if err == nil {
				err = os.Symlink(op.LinkTarget, full)
			}
			if err != nil {
				rv.Log.Printf("ERROR symlink %s: %v", op.Path, err)
			}
			ack(err)

		case wire.TFileBegin:
			abortTransfer()
			if err := wire.Decode(p, &curMeta); err != nil {
				ack(err)
				continue
			}
			curPath = curMeta.Path
			full := rv.full(curPath)
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				ack(err)
				continue
			}
			// section 4 ATOMIC WRITES: temp in the target dir, rename after
			// hash verification. Same *.cs-sync.tmp.* pattern as 1.x so the
			// existing startup cleanup finds leftovers.
			f, err := os.CreateTemp(filepath.Dir(full), ".cs-sync.tmp.*")
			if err != nil {
				ack(err)
				continue
			}
			tmpFile, tmpPath = f, f.Name()
			hasher = sha256.New()
			ack(nil)

		case wire.TFileData:
			var fd wire.FileData
			if err := wire.Decode(p, &fd); err != nil {
				abortTransfer()
				ack(err)
				continue
			}
			if tmpFile == nil {
				ack(fmt.Errorf("FileData without FileBegin"))
				continue
			}
			// per-block hash check (the future v2.1 delta key doubles as an
			// immediate integrity check)
			sum := sha256.Sum256(fd.Data)
			if hex.EncodeToString(sum[:]) != fd.Hash {
				abortTransfer()
				ack(fmt.Errorf("block hash mismatch at offset %d", fd.Offset))
				continue
			}
			// v2.0: strictly sequential writes (section 6) -- offset must
			// equal current file position; WriteAt keeps this future-proof.
			if _, err := tmpFile.WriteAt(fd.Data, fd.Offset); err != nil {
				abortTransfer()
				ack(err)
				continue
			}
			hasher.Write(fd.Data)
			ack(nil)

		case wire.TFileEnd:
			var fe wire.FileEnd
			if err := wire.Decode(p, &fe); err != nil {
				abortTransfer()
				ack(err)
				continue
			}
			if tmpFile == nil {
				ack(fmt.Errorf("FileEnd without FileBegin"))
				continue
			}
			err := rv.finishFile(tmpFile, tmpPath, curPath, curMeta, hasher, fe.FullHash)
			tmpFile = nil
			if err != nil {
				rv.Log.Printf("ERROR file %s: %v", curPath, err)
			} else {
				rv.Log.Printf("received %s (%d bytes)", curPath, curMeta.Size)
			}
			ack(err)

		case wire.TFolderACL:
			var fa wire.FolderACL
			if err := wire.Decode(p, &fa); err != nil {
				ack(err)
				continue
			}
			full := rv.Dest
			if fa.Path != "." {
				full = rv.full(fa.Path)
			}
			err := acl.Apply(full, fa.AclType, fa.Text)
			if err != nil {
				rv.Log.Printf("WARN: folder ACL %s: %v", fa.Path, err)
			}
			ack(err)

		case wire.TAclCSV:
			var ac wire.AclCSV
			if err := wire.Decode(p, &ac); err != nil {
				ack(err)
				continue
			}
			err := rv.writeAclCSV(ac.Data)
			if err != nil {
				rv.Log.Printf("ERROR writing acl.csv: %v", err)
			}
			ack(err)

		default:
			ack(fmt.Errorf("unknown frame type %d", t))
		}
	}
}

// finishFile verifies the e2e hash BEFORE the atomic rename (section 4),
// then sets mode + mtime and renames into place.
func (rv *Receiver) finishFile(f *os.File, tmpPath, relPath string, meta wire.FileBegin, hasher hash.Hash, wantHash string) error {
	defer os.Remove(tmpPath) // no-op after successful rename
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if got != wantHash {
		return fmt.Errorf("e2e hash mismatch (got %s want %s) -- file NOT installed", got[:12], wantHash[:12])
	}
	if err := os.Chmod(tmpPath, os.FileMode(meta.Mode)); err != nil {
		return err
	}
	mt := time.Unix(0, meta.MtimeNS)
	if err := os.Chtimes(tmpPath, mt, mt); err != nil {
		return err
	}
	return os.Rename(tmpPath, rv.full(relPath))
}

func (rv *Receiver) mkdir(op wire.Op) error {
	full := rv.full(op.Path)
	if err := os.MkdirAll(full, os.FileMode(op.Mode)); err != nil {
		return err
	}
	if op.ACL != "" { // same-OS native ACL (section 14)
		if err := acl.Apply(full, rv.AclType, op.ACL); err != nil {
			rv.Log.Printf("WARN: mkdir ACL %s: %v", op.Path, err)
		}
	}
	return nil
}

func (rv *Receiver) writeAclCSV(data []byte) error {
	dir, err := state.Dir(rv.Dest)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, state.AclCsvName+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, state.AclCsvName))
}

func (rv *Receiver) full(rel string) string {
	return filepath.Join(rv.Dest, filepath.FromSlash(rel))
}
