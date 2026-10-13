// zfsnet.go -- LAN service pair for --mode zfs: zfssend (Primary side,
// "run --mode zfs --backup-host host:port") and zfspull (Backup side,
// "cs-sync zfspull", a listener doing zfs receive). Local single-service
// case is in zfs.go. The primary never queries snapshots of the backup:
// it offers its own GUIDs (ZfsHello), the backup answers with a plan.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/logging"
	"github.com/guenther-alka/cs-sync/internal/wire"
)

const zfsChunkSize = 1 << 20

// ---------------------------------------------------------------- sender

func (c *zcfg) dial() (*wire.Conn, error) {
	nc, err := net.DialTimeout("tcp", c.remoteAddr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	cn := wire.NewEncrypted(nc, c.key)
	t, p, err := cn.Recv()
	if err != nil || t != wire.TWelcome {
		nc.Close()
		return nil, fmt.Errorf("handshake: t=%d err=%v", t, err)
	}
	var peer wire.Welcome
	if err := wire.Decode(p, &peer); err != nil {
		nc.Close()
		return nil, err
	}
	if peer.ProtoVersion != wire.ProtoVersion {
		nc.Close()
		return nil, fmt.Errorf("protocol version mismatch (peer=%d local=%d)", peer.ProtoVersion, wire.ProtoVersion)
	}
	if err := cn.Send(wire.TWelcome, wire.Welcome{ProtoVersion: wire.ProtoVersion, SyncVersion: version, OS: runtime.GOOS, Key: c.key}); err != nil {
		nc.Close()
		return nil, err
	}
	at, ap, err := cn.Recv()
	if err != nil || at != wire.TAck {
		nc.Close()
		return nil, fmt.Errorf("handshake accept-ack: t=%d err=%v", at, err)
	}
	var ack wire.Ack
	if err := wire.Decode(ap, &ack); err != nil || !ack.OK {
		nc.Close()
		return nil, fmt.Errorf("REFUSED by backup: %s", ack.Err)
	}
	return cn, nil
}

// streamTo runs "zfs <sendArgs>" and ships its stdout as chunks, then End.
func (c *zcfg) streamTo(cn *wire.Conn, sendArgs []string) error {
	cmd := exec.Command("zfs", sendArgs...)
	var se strings.Builder
	cmd.Stderr = &se
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	buf := make([]byte, zfsChunkSize)
	for {
		n, rerr := out.Read(buf)
		if n > 0 {
			if err := cn.Send(wire.TZfsChunk, wire.ZfsChunk{Data: buf[:n]}); err != nil {
				cmd.Process.Kill()
				cmd.Wait()
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			cmd.Process.Kill()
			cmd.Wait()
			return rerr
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("zfs send: %v: %s", err, strings.TrimSpace(se.String()))
	}
	return cn.Send(wire.TZfsChunk, wire.ZfsChunk{}) // empty chunk = end of stream
}

func recvDone(cn *wire.Conn) (wire.ZfsDone, error) {
	var d wire.ZfsDone
	t, p, err := cn.Recv()
	if err != nil {
		return d, err
	}
	if t != wire.TZfsDone {
		return d, fmt.Errorf("expected ZfsDone, got frame %d", t)
	}
	if err := wire.Decode(p, &d); err != nil {
		return d, err
	}
	if !d.OK {
		return d, errors.New("backup: " + d.Err)
	}
	return d, nil
}

func (c *zcfg) cycleRemote() (bool, error) {
	pl, err := c.listSnaps(c.primary)
	if err != nil {
		return false, err
	}
	if findSlot(pl, "n0") != nil && findSlot(pl, "new") == nil && !c.changed() {
		return false, nil // nothing changed since n0
	}
	cn, err := c.dial()
	if err != nil {
		return false, err
	}
	defer cn.Close()
	h := wire.ZfsHello{Nr: c.nr, FsGuid: zfsGet("guid", c.primary), Recursive: c.recursive}
	for _, s := range pl {
		h.Snaps = append(h.Snaps, wire.ZfsSnapRef{Slot: s.slot, Guid: s.guid})
	}
	if err := cn.Send(wire.TZfsHello, h); err != nil {
		return false, err
	}
	t, p, err := cn.Recv()
	if err != nil || t != wire.TZfsPlan {
		return false, fmt.Errorf("expected plan, got frame %d: %v", t, err)
	}
	var plan wire.ZfsPlan
	if err := wire.Decode(p, &plan); err != nil {
		return false, err
	}
	switch plan.Kind {
	case "error":
		return false, errors.New("backup: " + plan.Msg)
	case "blocked":
		c.status("blocked: " + plan.Msg)
		return false, nil
	case "rotate":
		return false, c.rotate(c.primary, c.versions) // backup completed an interrupted cycle
	case "resume":
		c.log.Printf("resuming interrupted receive on backup")
		if err := c.streamTo(cn, []string{"send", "-t", plan.Token}); err != nil {
			return false, err
		}
		_, err := recvDone(cn)
		return false, err
	}
	if pn := findSlot(pl, "new"); pn != nil { // stale, backup does not hold it
		c.destroySnap(pn.full)
	}
	pnew := c.primary + "@" + c.prefix + "new"
	appCons, err := c.takeSnapshot(pnew)
	if err != nil {
		return false, err
	}
	fail := func(e error) (bool, error) {
		c.destroySnap(pnew)
		return false, e
	}
	args := c.sendArgs(pnew)
	c.lastFull = plan.Kind != "inc"
	if plan.Kind == "inc" {
		var base *zsnap
		for i := range pl {
			if pl[i].guid == plan.Base {
				base = &pl[i]
			}
		}
		if base == nil {
			return fail(fmt.Errorf("backup chose unknown base %s", plan.Base))
		}
		args = c.sendArgs("-i", base.full, pnew)
	}
	if err := c.streamTo(cn, args); err != nil {
		return fail(err)
	}
	done, err := recvDone(cn)
	if err != nil {
		return fail(err)
	}
	pl2, _ := c.listSnaps(c.primary)
	pn := findSlot(pl2, "new")
	if pn == nil || pn.guid != done.NewGuid {
		return fail(fmt.Errorf("backup did not confirm snapshot"))
	}
	if appCons {
		zfsRun("set", "cs:consistent=app", pn.full)
	}
	if err := c.rotate(c.primary, c.versions); err != nil {
		return true, err
	}
	if err := cn.Send(wire.TZfsRotate, wire.ZfsRotate{App: appCons}); err != nil {
		return true, err
	}
	_, err = recvDone(cn)
	return true, err
}

// --------------------------------------------------------------- backup

type zfsRecv struct {
	dataset, nr, key, allowIP, serviceID string
	rollbackAuto                         bool
	keepHours, keepDays                  int
	log                                  *logging.Logger
}

func (rv *zfsRecv) cfg(h wire.ZfsHello) *zcfg {
	return &zcfg{backup: rv.dataset, nr: h.Nr, recursive: h.Recursive, prefix: "sync_" + h.Nr + "_" + h.FsGuid + "_",
		rollbackAuto: rv.rollbackAuto, keepHours: rv.keepHours, keepDays: rv.keepDays,
		serviceID: rv.serviceID, log: rv.log}
}

// planBackup decides what the primary has to send. It also completes or
// discards a half-finished cycle of the backup side.
func (c *zcfg) planBackup(h wire.ZfsHello) wire.ZfsPlan {
	perr := func(m string) wire.ZfsPlan { return wire.ZfsPlan{Kind: "error", Msg: m} }
	if !zfsExists(c.backup) {
		return wire.ZfsPlan{Kind: "full"}
	}
	if have, want := zfsGet("cs:sync", c.backup), h.Nr+":"+h.FsGuid; have != want {
		return perr(fmt.Sprintf("cs:sync=%q on %s, expected %q (wrong target dataset?)", have, c.backup, want))
	}
	if tok := zfsGet("receive_resume_token", c.backup); tok != "" && tok != "-" {
		if !c.recursive {
			return wire.ZfsPlan{Kind: "resume", Token: tok}
		}
		zfsRun("recv", "-A", c.backup) // a single token cannot resume a -R stream: discard, start again
	}
	bl, err := c.listSnaps(c.backup)
	if err != nil {
		return perr(err.Error())
	}
	if bn := findSlot(bl, "new"); bn != nil {
		slot := ""
		for _, s := range h.Snaps {
			if s.Guid == bn.guid {
				slot = s.Slot
			}
		}
		switch slot {
		case "new": // both sides received/sent, nobody rotated yet
			if err := c.rotate(c.backup, true); err != nil {
				return perr(err.Error())
			}
			return wire.ZfsPlan{Kind: "rotate"}
		case "n0": // primary already rotated
			if err := c.rotate(c.backup, true); err != nil {
				return perr(err.Error())
			}
		default:
			c.destroySnap(bn.full)
		}
		if bl, err = c.listSnaps(c.backup); err != nil {
			return perr(err.Error())
		}
	}
	if len(bl) == 0 {
		return perr("backup dataset has no sync snapshots (rebase needed)")
	}
	have := map[string]bool{}
	for _, b := range bl {
		have[b.guid] = true
	}
	base := ""
	for i := len(h.Snaps) - 1; i >= 0; i-- {
		if h.Snaps[i].Slot != "new" && have[h.Snaps[i].Guid] {
			base = h.Snaps[i].Guid
			break
		}
	}
	if base == "" {
		return perr("no common base snapshot")
	}
	newest := bl[len(bl)-1]
	if base != newest.guid && !c.rollbackAuto && !c.approved(base, newest.guid) {
		c.setBlocked(base, newest.guid)
		return wire.ZfsPlan{Kind: "blocked", Msg: "rollback needed (newest snapshot on backup is newer than the common base) - approve in frontend"}
	}
	return wire.ZfsPlan{Kind: "inc", Base: base}
}

func (rv *zfsRecv) receiveStream(cn *wire.Conn, c *zcfg, h wire.ZfsHello, plan wire.ZfsPlan) {
	args := []string{"recv", "-s", "-u", c.backup}
	if plan.Kind == "inc" {
		args = []string{"recv", "-s", "-F", "-u", c.backup}
	}
	cmd := exec.Command("zfs", args...)
	var se strings.Builder
	cmd.Stderr = &se
	stdin, err := cmd.StdinPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		cn.Send(wire.TZfsDone, wire.ZfsDone{Err: err.Error()})
		return
	}
	for {
		t, p, err := cn.Recv()
		if err != nil { // sender gone: partial state stays resumable (recv -s)
			stdin.Close()
			cmd.Wait()
			rv.log.Printf("stream aborted: %v", err)
			return
		}
		var ch wire.ZfsChunk
		if t != wire.TZfsChunk || wire.Decode(p, &ch) != nil {
			stdin.Close()
			cmd.Wait()
			return
		}
		if len(ch.Data) == 0 {
			break
		}
		if _, err := stdin.Write(ch.Data); err != nil {
			break
		}
	}
	stdin.Close()
	werr := cmd.Wait()
	done := wire.ZfsDone{OK: werr == nil}
	if werr != nil {
		done.Err = fmt.Sprintf("zfs recv: %v: %s", werr, strings.TrimSpace(se.String()))
	} else {
		if plan.Kind == "full" {
			zfsRun("set", "readonly=on", c.backup)
			zfsRun("set", "canmount=noauto", c.backup)
			zfsRun("set", "cs:sync="+h.Nr+":"+h.FsGuid, c.backup)
		}
		if c.recursive {
			c.fixTree(c.backup)
		}
		if plan.Kind == "inc" && rv.serviceID != "" {
			os.Remove(approvePath(rv.serviceID)) // one-shot
		}
		if bl, err := c.listSnaps(c.backup); err == nil {
			if bn := findSlot(bl, "new"); bn != nil {
				done.NewGuid = bn.guid
			}
		}
	}
	cn.Send(wire.TZfsDone, done)
}

func (rv *zfsRecv) handle(nc net.Conn) {
	defer nc.Close()
	cn := wire.NewEncrypted(nc, rv.key)
	if err := cn.Send(wire.TWelcome, wire.Welcome{ProtoVersion: wire.ProtoVersion, SyncVersion: version, OS: runtime.GOOS}); err != nil {
		return
	}
	t, p, err := cn.Recv()
	var w wire.Welcome
	if err != nil || t != wire.TWelcome || wire.Decode(p, &w) != nil {
		return
	}
	if w.ProtoVersion != wire.ProtoVersion || w.Key != rv.key {
		cn.Send(wire.TAck, wire.Ack{OK: false, Err: "protocol version or transfer key mismatch"})
		rv.log.Printf("REFUSING %s: protocol version or key mismatch", nc.RemoteAddr())
		return
	}
	cn.Send(wire.TAck, wire.Ack{OK: true})
	var cur *zcfg
	var hello wire.ZfsHello
	for {
		t, p, err := cn.Recv()
		if err != nil {
			return
		}
		switch t {
		case wire.TPing:
			cn.Send(wire.TAck, wire.Ack{OK: true})
		case wire.TZfsHello:
			if wire.Decode(p, &hello) != nil {
				return
			}
			var plan wire.ZfsPlan
			if hello.Nr != rv.nr {
				plan = wire.ZfsPlan{Kind: "error", Msg: "nr mismatch: primary " + hello.Nr + ", backup service " + rv.nr}
			} else {
				cur = rv.cfg(hello)
				plan = cur.planBackup(hello)
			}
			cn.Send(wire.TZfsPlan, plan)
			switch plan.Kind {
			case "full", "inc", "resume":
				rv.receiveStream(cn, cur, hello, plan)
			case "blocked":
				cur.status("blocked: " + plan.Msg)
			case "error":
				rv.log.Printf("error: %s", plan.Msg)
				writeStatus(rv.serviceID, "error: "+plan.Msg)
			}
		case wire.TZfsRotate:
			var r wire.ZfsRotate
			wire.Decode(p, &r)
			d := wire.ZfsDone{OK: true}
			if cur == nil {
				d = wire.ZfsDone{Err: "rotate without hello"}
			} else if bl, err := cur.listSnaps(cur.backup); err != nil || findSlot(bl, "new") == nil {
				d = wire.ZfsDone{Err: "no new snapshot to rotate"}
			} else {
				if r.App {
					zfsRun("set", "cs:consistent=app", findSlot(bl, "new").full)
				}
				if err := cur.rotate(cur.backup, true); err != nil {
					d = wire.ZfsDone{Err: err.Error()}
				} else {
					cur.status("ok " + time.Now().Format("15:04:05"))
				}
			}
			cn.Send(wire.TZfsDone, d)
		}
	}
}

func zfspullCmd(args []string) {
	fs := flag.NewFlagSet("zfspull", flag.ExitOnError)
	backup := fs.String("backup", "", "backup ZFS dataset on this host (required)")
	listen := fs.String("listen", "127.0.0.1:9010", "listen address")
	key := fs.String("key", "", "REQUIRED: pre-shared transfer key")
	allowIP := fs.String("allow_ip", "", "REQUIRED: the single primary IP allowed to connect")
	nr := fs.String("nr", "1", "service number, must match the primary")
	keep := fs.String("keep", "hours:24,days:30", "retention of kept snapshots, hours:N,days:N")
	rollback := fs.String("rollback", "ask", "auto|ask: recv -F when the common base is older than the newest backup snapshot")
	serviceID := fs.String("service-id", "", "status file _cfg/sync/<id>.last, approval file <id>.approve")
	logPath := fs.String("log", "", "log file path")
	fs.Parse(args)
	if *backup == "" || *key == "" || *allowIP == "" {
		fmt.Fprintln(os.Stderr, "error: zfspull needs --backup <dataset>, --key and --allow_ip")
		os.Exit(2)
	}
	log, err := logging.New(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log:", err)
		os.Exit(1)
	}
	rv := &zfsRecv{dataset: *backup, nr: *nr, key: *key, allowIP: *allowIP, serviceID: *serviceID,
		rollbackAuto: *rollback == "auto", keepHours: 24, keepDays: 30, log: log}
	parseKeep(*keep, &rv.keepHours, &rv.keepDays)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Printf("FATAL: %v", err)
		writeStatus(*serviceID, "error: "+err.Error())
		os.Exit(1)
	}
	log.Printf("cs-sync zfspull %s: backup=%s nr=%s listening on %s (encrypted, allow-ip=%s)", version, *backup, *nr, *listen, *allowIP)
	writeStatus(*serviceID, fmt.Sprintf("started (%s) zfspull %s", time.Now().Format("2006-01-02 15:04:05"), *backup))
	for {
		nc, err := ln.Accept()
		if err != nil {
			log.Printf("FATAL: %v", err)
			os.Exit(1)
		}
		host, _, serr := net.SplitHostPort(nc.RemoteAddr().String())
		if serr != nil || host != *allowIP {
			log.Printf("REFUSING connection from %s: not the allow-listed IP (%s)", nc.RemoteAddr(), *allowIP)
			nc.Close()
			continue
		}
		rv.handle(nc) // sequential by design
	}
}
