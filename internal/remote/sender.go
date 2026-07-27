package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/acl"
	"github.com/guenther-alka/cs-sync/internal/guard"
	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/model"
	"github.com/guenther-alka/cs-sync/internal/reconcile"
	"github.com/guenther-alka/cs-sync/internal/wire"
)

// Sender pushes the primary tree to a remote receiver, one-way only
// (section 12: remote is ALWAYS unidir, primary -> remote).
//
// KISS QUEUE INSIGHT (section 3, implementation note): the persisted
// remote BASELINE (last state acked by the receiver) doubles as the
// coalesced pending queue. Anything whose current primary state differs
// from that baseline IS pending, automatically coalesced per path (100
// changes while offline = 1 diff entry), and it survives restarts because
// the baseline is persisted. No separate queue file is needed -- only the
// retry/quarantine state (guard.RetryState) is stored in addition.
type Sender struct {
	Primary     string
	Addr        string // receiver address (usually a local cs-stream tunnel-send endpoint)
	StateDir    string // per-pair dir, holds remote baseline + retry state + freeze marker
	AclType     string
	TransferKey string // section 15: sent in the handshake, "" = none
	Budget      guard.Budget
	Limiter     *Limiter // nil = unlimited
	Version     string
	Log         *logging.Logger

	conn     *wire.Conn
	peer     wire.Welcome
	sameOS   bool
	source   guard.SourceID
	retry    *guard.RetryState
	baseline model.Tree
}

// Init loads persisted per-pair state (remote baseline + retry state).
func (s *Sender) Init() error {
	if err := os.MkdirAll(s.StateDir, 0700); err != nil {
		return err
	}
	s.baseline = loadRemoteBaseline(s.StateDir)
	s.retry = guard.LoadRetry(s.StateDir)
	s.source = loadSourceID(s.StateDir)
	return nil
}

// connect dials (or re-dials) the receiver and performs the handshake.
// Transient failure is NORMAL operation (section 3), returned as error --
// the caller just retries on the next pass.
func (s *Sender) connect() error {
	if s.conn != nil {
		// cheap liveness probe; reconnect on failure. roundtrip() already
		// closes + nils s.conn on any transport error (found via sandbox
		// kill-the-receiver test 2026.07.24: closing again here was a nil
		// deref panic), so only close if it survived with a protocol error.
		if err := s.roundtrip(wire.TPing, wire.Op{}); err == nil {
			return nil
		}
		if s.conn != nil {
			s.conn.Close()
			s.conn = nil
		}
	}
	c, err := net.DialTimeout("tcp", s.Addr, 10*time.Second)
	if err != nil {
		return err
	}
	cn := wire.NewEncrypted(c, s.TransferKey)
	t, p, err := cn.Recv()
	if err != nil || t != wire.TWelcome {
		c.Close()
		return fmt.Errorf("handshake: t=%d err=%v", t, err)
	}
	if err := wire.Decode(p, &s.peer); err != nil {
		c.Close()
		return err
	}
	if s.peer.ProtoVersion != wire.ProtoVersion {
		c.Close()
		return fmt.Errorf("REFUSING: protocol version mismatch (peer=%d local=%d) -- update both members", s.peer.ProtoVersion, wire.ProtoVersion)
	}
	if err := cn.Send(wire.TWelcome, wire.Welcome{
		ProtoVersion: wire.ProtoVersion, SyncVersion: s.Version,
		OS: runtime.GOOS, AclType: s.AclType, Key: s.TransferKey,
	}); err != nil {
		c.Close()
		return err
	}
	// wait for the receiver's explicit accept-ack (see receiver.go handle()
	// comment) -- without this, a key mismatch was only discovered on the
	// first real op, and this function had already logged success.
	at, ap, err := cn.Recv()
	if err != nil {
		c.Close()
		return fmt.Errorf("handshake accept-ack: %w", err)
	}
	if at != wire.TAck {
		c.Close()
		return fmt.Errorf("handshake: expected accept-ack, got frame %d", at)
	}
	var aack wire.Ack
	if err := wire.Decode(ap, &aack); err != nil {
		c.Close()
		return err
	}
	if !aack.OK {
		c.Close()
		return fmt.Errorf("REFUSED by receiver: %s", aack.Err)
	}
	s.conn = cn
	// section 14: native ACL application ADDITIONALLY when both ends run
	// the same OS (and acltype family) -- auto-detected, no flag.
	s.sameOS = s.peer.OS == runtime.GOOS && s.peer.AclType == s.AclType
	s.Log.Printf("remote connected: %s (cs-sync %s, os=%s, acltype=%s, nativeACL=%v)",
		s.Addr, s.peer.SyncVersion, s.peer.OS, s.peer.AclType, s.sameOS)
	return nil
}

// Pass runs one remote reconcile pass: diff primaryTree against the
// remote baseline, guard-check, send ops, update baseline per ack.
// primaryTree must already carry folder ACLs (populateACL ran).
func (s *Sender) Pass(primaryTree model.Tree, reason string, rootACL string, aclCSV []byte) {
	approved := guard.ConsumeApproval(s.StateDir)
	if approved {
		s.Log.Printf("remote pass (%s): operator APPROVED pending mass delete (%s consumed) -- budget check skipped for this pass", reason, guard.ApproveFile)
	} else if guard.Frozen(s.StateDir) {
		s.Log.Printf("remote pass (%s): FROZEN -- rename %s to %s to approve the pending mass delete", reason, guard.FreezeFile, guard.ApproveFile)
		return
	}
	// section 5a: refuse everything if the source dataset is not the one
	// recorded at setup (unmounted dataset -> empty dir -> mass delete).
	if err := guard.CheckSource(s.Primary, &s.source, rootDev); err != nil {
		s.Log.Printf("ERROR remote pass (%s): %v", reason, err)
		return
	}
	saveSourceID(s.StateDir, s.source)

	if err := s.connect(); err != nil {
		s.Log.Printf("remote pass (%s): not connected (%v) -- pending changes stay queued, will retry", reason, err)
		return
	}

	// one-way diff: baseline = last acked remote state (see Sender doc).
	res := reconcile.Reconcile(s.baseline, primaryTree, cloneTree(s.baseline))

	// keep only ops that target "secondary" (= remote); with baseline ==
	// secondaryTree, reconcile only ever produces primary->secondary ops.
	ops := res.Ops

	// section 5b: delete budget BEFORE anything is sent.
	nDel := 0
	for _, op := range ops {
		if op.Kind == reconcile.OpDelete || op.Kind == reconcile.OpRmdir {
			nDel++
		}
	}
	if err := s.Budget.Check(nDel, len(s.baseline)); err != nil && !approved {
		s.Log.Printf("ALARM remote pass (%s): %v -- FREEZING relationship", reason, err)
		if ferr := guard.Freeze(s.StateDir, err.Error()); ferr != nil {
			s.Log.Printf("ERROR writing freeze marker: %v", ferr)
		}
		return
	}
	if len(ops) == 0 {
		if s.sameOS && (reason == "safety-net" || reason == "sighup") {
			s.resyncFolderACLs(primaryTree, rootACL)
		}
		return
	}
	s.Log.Printf("remote pass (%s): %d ops (%d deletes) pending", reason, len(ops), nDel)

	// creates ascending (parents first), deletes descending (children first)
	var creates, deletes []reconcile.Op
	for _, op := range ops {
		if op.Kind == reconcile.OpDelete || op.Kind == reconcile.OpRmdir {
			deletes = append(deletes, op)
		} else {
			creates = append(creates, op)
		}
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i].Path < creates[j].Path })
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].Path > deletes[j].Path })

	// section 4 ORDERING UNDER RETRY: a failed mkdir BLOCKS its children.
	var blocked []string
	isBlocked := func(p string) bool {
		for _, b := range blocked {
			if strings.HasPrefix(p, b) {
				return true
			}
		}
		return false
	}

	sent, failed := 0, 0
	for _, op := range append(creates, deletes...) {
		if s.retry.Skip(op.Path) || isBlocked(op.Path) {
			continue
		}
		err := s.sendOp(op, primaryTree)
		if err != nil {
			failed++
			if op.Kind == reconcile.OpMkdir {
				blocked = append(blocked, op.Path+"/")
			}
			if s.retry.Fail(op.Path, err.Error()) {
				s.Log.Printf("QUARANTINE: %s after %d attempts (last: %v)", op.Path, guard.MaxAttempts, err)
			} else {
				s.Log.Printf("WARN: %s failed (%v), will retry with backoff", op.Path, err)
			}
			if s.conn == nil {
				break // connection died -- rest stays pending for next pass
			}
			continue
		}
		s.retry.Clear(op.Path)
		s.applyToBaseline(op, primaryTree)
		sent++
	}

	// acl.csv is ALWAYS distributed (section 14), after content ops.
	if s.conn != nil && len(aclCSV) > 0 {
		if err := s.roundtrip(wire.TAclCSV, wire.AclCSV{Data: aclCSV}); err != nil {
			s.Log.Printf("WARN: acl.csv push failed: %v", err)
		}
	}
	if s.conn != nil && s.sameOS && rootACL != "" {
		if err := s.roundtrip(wire.TFolderACL, wire.FolderACL{Path: ".", AclType: s.AclType, Text: rootACL}); err != nil {
			s.Log.Printf("WARN: root ACL push failed: %v", err)
		}
	}

	saveRemoteBaseline(s.StateDir, s.baseline)
	if err := guard.SaveRetry(s.StateDir, s.retry); err != nil {
		s.Log.Printf("WARN: could not save retry state: %v", err)
	}
	q := s.retry.QuarantinedCount()
	s.Log.Printf("remote pass (%s): %d sent, %d failed, %d quarantined total", reason, sent, failed, q)
}

// sendOp transmits one operation and waits for its ack.
func (s *Sender) sendOp(op reconcile.Op, primaryTree model.Tree) error {
	e := primaryTree[op.Path]
	switch op.Kind {
	case reconcile.OpMkdir:
		w := wire.Op{Path: op.Path, Mode: e.Mode, MtimeNS: e.MtimeNS}
		if s.sameOS {
			w.ACL = e.ACL
		}
		return s.roundtrip(wire.TMkdir, w)
	case reconcile.OpRename:
		return s.roundtrip(wire.TRename, wire.Op{Path: op.Path, OldPath: op.OldPath})
	case reconcile.OpDelete:
		return s.roundtrip(wire.TDelete, wire.Op{Path: op.Path})
	case reconcile.OpRmdir:
		return s.roundtrip(wire.TRmdir, wire.Op{Path: op.Path})
	case reconcile.OpCopy:
		if e.Type == model.TypeSymlink {
			return s.roundtrip(wire.TSymlink, wire.Op{Path: op.Path, LinkTarget: e.LinkTarget})
		}
		return s.sendFile(op.Path, e)
	case reconcile.OpConflictRename:
		// cannot happen in one-way mode (baseline == secondary tree ->
		// secondary never appears changed); defensive no-op.
		return nil
	}
	return fmt.Errorf("unknown op kind %d", op.Kind)
}

// sendFile streams one file in BlockSize blocks with per-block + full
// hashes, with torn-copy detection (section 4): source size+mtime are
// compared before and after the read; a mismatch aborts and the file
// stays pending (still differs from baseline -> auto-requeued).
func (s *Sender) sendFile(rel string, e model.Entry) error {
	full := filepath.Join(s.Primary, filepath.FromSlash(rel))
	fi0, err := os.Stat(full)
	if err != nil {
		return err
	}
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := s.roundtrip(wire.TFileBegin, wire.FileBegin{
		Path: rel, Size: fi0.Size(), Mode: e.Mode, MtimeNS: e.MtimeNS,
	}); err != nil {
		return err
	}
	fullHash := sha256.New()
	buf := make([]byte, wire.BlockSize)
	var off int64
	for {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if s.Limiter != nil {
				s.Limiter.Wait(n)
			}
			blk := buf[:n]
			sum := sha256.Sum256(blk)
			fullHash.Write(blk)
			if err := s.roundtrip(wire.TFileData, wire.FileData{
				Offset: off, Hash: hex.EncodeToString(sum[:]), Data: blk,
			}); err != nil {
				return err
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	// torn-copy check BEFORE FileEnd -- if the source changed mid-read,
	// tell the receiver to discard (send mismatching hash is wrong; abort
	// by closing the transfer with an error instead).
	fi1, err := os.Stat(full)
	if err != nil {
		return err
	}
	if fi1.Size() != fi0.Size() || !fi1.ModTime().Equal(fi0.ModTime()) {
		// receiver aborts its temp file when the connection state resets;
		// cleanest inside the protocol: send FileEnd with an impossible
		// hash so the receiver discards the temp file and errors this op.
		_ = s.roundtrip(wire.TFileEnd, wire.FileEnd{FullHash: "torn"})
		return fmt.Errorf("torn copy: %s changed during transfer, requeued", rel)
	}
	return s.roundtrip(wire.TFileEnd, wire.FileEnd{FullHash: hex.EncodeToString(fullHash.Sum(nil))})
}

// resyncFolderACLs mirrors 1.x's safety-net existing-folder ACL re-sync
// onto the remote (section 8): re-push every folder ACL, idempotent.
func (s *Sender) resyncFolderACLs(primaryTree model.Tree, rootACL string) {
	n := 0
	for rel, e := range primaryTree {
		if e.Type != model.TypeDir || e.ACL == "" {
			continue
		}
		if err := s.roundtrip(wire.TFolderACL, wire.FolderACL{Path: rel, AclType: s.AclType, Text: e.ACL}); err != nil {
			s.Log.Printf("WARN: remote ACL re-sync %s: %v", rel, err)
			if s.conn == nil {
				return
			}
			continue
		}
		n++
	}
	if rootACL != "" {
		_ = s.roundtrip(wire.TFolderACL, wire.FolderACL{Path: ".", AclType: s.AclType, Text: rootACL})
	}
	if n > 0 {
		s.Log.Printf("remote existing-folder ACL re-sync: %d folder(s)", n)
	}
}

// roundtrip sends one frame and waits for its ack; a transport error
// drops the connection (it will be re-dialed on the next pass).
func (s *Sender) roundtrip(t byte, msg any) error {
	if s.conn == nil {
		return fmt.Errorf("not connected")
	}
	if err := s.conn.Send(t, msg); err != nil {
		s.conn.Close()
		s.conn = nil
		return err
	}
	rt, p, err := s.conn.Recv()
	if err != nil {
		s.conn.Close()
		s.conn = nil
		return err
	}
	if rt != wire.TAck {
		s.conn.Close()
		s.conn = nil
		return fmt.Errorf("expected ack, got frame %d", rt)
	}
	var a wire.Ack
	if err := wire.Decode(p, &a); err != nil {
		return err
	}
	if !a.OK {
		return fmt.Errorf("remote: %s", a.Err)
	}
	return nil
}

// applyToBaseline records a successfully acked op in the remote baseline.
func (s *Sender) applyToBaseline(op reconcile.Op, primaryTree model.Tree) {
	switch op.Kind {
	case reconcile.OpDelete, reconcile.OpRmdir:
		delete(s.baseline, op.Path)
		if op.Kind == reconcile.OpRmdir { // children went with RemoveAll
			pfx := op.Path + "/"
			for p := range s.baseline {
				if strings.HasPrefix(p, pfx) {
					delete(s.baseline, p)
				}
			}
		}
	case reconcile.OpRename:
		if e, ok := s.baseline[op.OldPath]; ok {
			delete(s.baseline, op.OldPath)
			e.Path = op.Path
			s.baseline[op.Path] = e
		}
		// a renamed dir carries its children
		pfx := op.OldPath + "/"
		for p, e := range s.baseline {
			if strings.HasPrefix(p, pfx) {
				delete(s.baseline, p)
				np := op.Path + "/" + strings.TrimPrefix(p, pfx)
				e.Path = np
				s.baseline[np] = e
			}
		}
	default:
		if e, ok := primaryTree[op.Path]; ok {
			s.baseline[op.Path] = e
		}
	}
}

// Quarantined exposes the aggregate count for status/GUI (section 10).
func (s *Sender) Quarantined() int { return s.retry.QuarantinedCount() }

func cloneTree(t model.Tree) model.Tree {
	n := make(model.Tree, len(t))
	for k, v := range t {
		n[k] = v
	}
	return n
}

// rootDev returns the device id of a path via the scanner's stat helper
// semantics (0 on Windows -> liveness check disabled there).
func rootDev(root string) (uint64, error) {
	fi, err := os.Stat(root)
	if err != nil {
		return 0, err
	}
	return devOf(fi), nil
}

// ensure acl import is used even if a future refactor drops other uses
var _ = acl.Read
