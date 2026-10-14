// zfs.go -- cs-sync --mode zfs: event-based realtime ZFS replication,
// LOCAL single-service case (Primary dataset -> Backup dataset on the
// same host, zfs send | zfs recv via pipe, no network).
// Spec: csweb-gui/data/howto.ai/cs-sync-zfs-realtime.info
// Not yet implemented: LAN service pair (zfssend/zfsreceive), zfs holds,
// resume of a half-finished receive from a different base.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/guenther-alka/cs-sync/internal/logging"
)

type zsnap struct {
	full    string // dataset@name
	slot    string // name without sync_<nr>_<guid>_ prefix
	guid    string
	created int64
}

type zcfg struct {
	primary, backup, nr, prefix string
	versions                    bool
	keepHours, keepDays         int
	rollbackAuto                bool
	serviceID                   string
	log                         *logging.Logger
	freeze, freezeBin           string // freeze=hh:mm (daily app-consistent snapshot), tool path
	freezeArgs                  []string
	freezeTried                 string            // YYYYMMDD of the last freeze attempt
	lastFull                    bool              // last cycle was an initial full send (no adaptive wait afterwards)
	recursive                   bool              // replicate child datasets too (send -R)
	pveConf                     bool              // store Proxmox VM/CT configs as cs:pveconf properties
	confSeen                    map[string]string // last stored/sent config per relative dataset
	remoteAddr, key             string            // LAN pair: backup host:port + transfer key ("" = local backup)
}

func (c *zcfg) sendArgs(a ...string) []string {
	if c.recursive {
		return append([]string{"send", "-R"}, a...)
	}
	return append([]string{"send"}, a...)
}

func isZfsMode(args []string) bool {
	for i, a := range args {
		if (a == "--mode" || a == "-mode") && i+1 < len(args) && args[i+1] == "zfs" {
			return true
		}
		if a == "--mode=zfs" || a == "-mode=zfs" {
			return true
		}
	}
	return false
}

func zfsRun(args ...string) (string, error) {
	out, err := exec.Command("zfs", args...).CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		return s, fmt.Errorf("zfs %s: %v: %s", strings.Join(args, " "), err, s)
	}
	return s, nil
}

func zfsExists(ds string) bool {
	_, err := zfsRun("list", "-H", "-o", "name", ds)
	return err == nil
}

func zfsGet(prop, ds string) string {
	s, err := zfsRun("get", "-H", "-o", "value", prop, ds)
	if err != nil {
		return ""
	}
	return s
}

// listSnaps returns the sync_<nr>_<guid>_ snapshots of ds, oldest first.
func (c *zcfg) listSnaps(ds string) ([]zsnap, error) {
	out, err := zfsRun("list", "-H", "-p", "-t", "snapshot", "-d", "1", "-o", "name,guid,creation", "-s", "createtxg", ds)
	if err != nil {
		return nil, err
	}
	var res []zsnap
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(ln), "\t")
		if len(f) < 3 {
			continue
		}
		i := strings.Index(f[0], "@")
		if i < 0 || !strings.HasPrefix(f[0][i+1:], c.prefix) {
			continue
		}
		cr, _ := strconv.ParseInt(f[2], 10, 64)
		res = append(res, zsnap{full: f[0], slot: f[0][i+1+len(c.prefix):], guid: f[1], created: cr})
	}
	return res, nil
}

func findSlot(l []zsnap, slot string) *zsnap {
	for i := range l {
		if l[i].slot == slot {
			return &l[i]
		}
	}
	return nil
}

// pipeSendRecv runs "zfs <sendArgs> | zfs <recvArgs>".
func pipeSendRecv(sendArgs, recvArgs []string) error {
	snd := exec.Command("zfs", sendArgs...)
	rcv := exec.Command("zfs", recvArgs...)
	var se, re strings.Builder
	snd.Stderr = &se
	rcv.Stderr = &re
	p, err := snd.StdoutPipe()
	if err != nil {
		return err
	}
	rcv.Stdin = p
	if err := rcv.Start(); err != nil {
		return err
	}
	if err := snd.Start(); err != nil {
		rcv.Process.Kill()
		rcv.Wait()
		return err
	}
	e1 := snd.Wait()
	e2 := rcv.Wait()
	if e1 != nil || e2 != nil {
		return fmt.Errorf("send|recv failed: send=%v (%s) recv=%v (%s)", e1, strings.TrimSpace(se.String()), e2, strings.TrimSpace(re.String()))
	}
	return nil
}

func blockedPath(serviceID string) string {
	return filepath.Join(syncCfgDir(), serviceID+".blocked")
}

// setBlocked records the GUID pair a rollback approval has to name; the
// frontend ("approve rollback") copies <id>.blocked to <id>.approve.
func (c *zcfg) setBlocked(base, newest string) {
	if c.serviceID != "" {
		os.WriteFile(blockedPath(c.serviceID), []byte(base+" "+newest+"\n"), 0644)
	}
}

func (c *zcfg) clearBlocked() {
	if c.serviceID != "" {
		os.Remove(blockedPath(c.serviceID))
	}
}

func approvePath(serviceID string) string {
	return filepath.Join(syncCfgDir(), serviceID+".approve")
}

// approved reports whether the frontend released a rollback for exactly
// this base/newest-backup GUID pair (one-shot, see spec section 6).
func (c *zcfg) approved(baseGuid, newestGuid string) bool {
	if c.serviceID == "" {
		return false
	}
	b, err := os.ReadFile(approvePath(c.serviceID))
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	return len(f) >= 2 && f[0] == baseGuid && f[1] == newestGuid
}

func (c *zcfg) rename(ds string, from, to string) error {
	args := []string{"rename"}
	if c.recursive {
		args = append(args, "-r")
	}
	_, err := zfsRun(append(args, ds+"@"+c.prefix+from, ds+"@"+c.prefix+to)...)
	return err
}

func (c *zcfg) destroySnap(full string) (string, error) {
	if c.recursive {
		return zfsRun("destroy", "-r", full)
	}
	return zfsRun("destroy", full)
}

// changed reports whether data changed since n0 (written@n0; for a
// recursive tree any child dataset counts, errors count as changed).
func (c *zcfg) changed() bool {
	prop := "written@" + c.prefix + "n0"
	if !c.recursive {
		// -p: exact bytes ("0", not "0B" as Linux prints without it), otherwise an idle dataset always counts as changed
		v, err := zfsRun("get", "-H", "-p", "-o", "value", prop, c.primary)
		return err != nil || strings.TrimSpace(v) != "0"
	}
	out, err := zfsRun("get", "-H", "-p", "-r", "-t", "filesystem,volume", "-o", "value", prop, c.primary)
	if err != nil {
		return true
	}
	for _, v := range strings.Fields(out) {
		if v != "0" && v != "-" { // "-": property not applicable (some platforms also list snapshots)
			return true
		}
	}
	return false
}

// fixTree keeps every dataset of a received tree read-only and unmountable
// (children arrive with the primary's canmount/readonly values).
func (c *zcfg) fixTree(ds string) {
	out, err := zfsRun("list", "-r", "-H", "-o", "name,readonly,canmount", ds)
	if err != nil {
		return
	}
	for _, ln := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(ln), "\t")
		if len(f) < 3 {
			continue
		}
		if f[1] != "on" {
			zfsRun("set", "readonly=on", f[0])
		}
		if f[2] != "noauto" {
			zfsRun("set", "canmount=noauto", f[0])
		}
	}
}

// rotate: new -> n0, n0 -> n1; old n1 is kept as kd<MMDD>/kh<MMDDHH> (if
// keep is on and the slot is free) or destroyed. Then prune kept snaps.
func (c *zcfg) rotate(ds string, keep bool) error {
	l, err := c.listSnaps(ds)
	if err != nil {
		return err
	}
	if findSlot(l, "new") == nil {
		return fmt.Errorf("rotate %s: no new snapshot", ds)
	}
	if n1 := findSlot(l, "n1"); n1 != nil {
		t := time.Unix(n1.created, 0)
		kd, kh := "kd"+t.Format("0102"), "kh"+t.Format("010215")
		kf := "kf" + t.Format("0102")
		switch {
		case zfsGet("cs:consistent", n1.full) == "app" && findSlot(l, kf) == nil:
			err = c.rename(ds, "n1", kf) // app-consistent daily snapshot: kept on both sides
		case keep && findSlot(l, kd) == nil:
			err = c.rename(ds, "n1", kd)
		case keep && findSlot(l, kh) == nil:
			err = c.rename(ds, "n1", kh)
		default:
			_, err = c.destroySnap(n1.full)
		}
		if err != nil {
			return err
		}
	}
	if findSlot(l, "n0") != nil {
		if err := c.rename(ds, "n0", "n1"); err != nil {
			return err
		}
	}
	if err := c.rename(ds, "new", "n0"); err != nil {
		return err
	}
	c.prune(ds)
	c.clearBlocked()
	return nil
}

func (c *zcfg) prune(ds string) {
	l, err := c.listSnaps(ds)
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, s := range l {
		age := now - s.created
		if (strings.HasPrefix(s.slot, "kh") && age > int64(c.keepHours)*3600) ||
			((strings.HasPrefix(s.slot, "kd") || strings.HasPrefix(s.slot, "kf")) && age > int64(c.keepDays)*86400) {
			if _, err := c.destroySnap(s.full); err != nil {
				c.log.Printf("WARN: prune: %v", err)
			}
		}
	}
}

func (c *zcfg) status(line string) {
	c.log.Printf("%s", line)
	writeStatus(c.serviceID, line)
}

// recover completes or discards a half-finished cycle (crash between
// recv and rotate). State is derived from ZFS itself, not from files.
func (c *zcfg) recoverCycle() error {
	pl, err := c.listSnaps(c.primary)
	if err != nil {
		return err
	}
	pn := findSlot(pl, "new")
	if !zfsExists(c.backup) {
		if pn != nil {
			_, err = c.destroySnap(pn.full)
		}
		return err
	}
	bl, err := c.listSnaps(c.backup)
	if err != nil {
		return err
	}
	bn := findSlot(bl, "new")
	switch {
	case pn != nil && bn != nil && pn.guid == bn.guid:
		c.log.Printf("recovery: completing rotation of interrupted cycle")
		if err := c.rotate(c.primary, c.versions); err != nil {
			return err
		}
		return c.rotate(c.backup, true)
	case bn != nil:
		for _, p := range pl {
			if p.guid == bn.guid && p.slot == "n0" { // primary already rotated
				c.log.Printf("recovery: completing backup rotation")
				return c.rotate(c.backup, true)
			}
		}
		_, err = c.destroySnap(bn.full)
		if err != nil {
			return err
		}
	}
	if pn != nil {
		_, err = c.destroySnap(pn.full)
	}
	return err
}

// cycle runs one replication cycle. Returns true if data was sent.
func (c *zcfg) cycle() (bool, error) {
	if c.remoteAddr != "" {
		return c.cycleRemote()
	}
	if err := c.recoverCycle(); err != nil {
		return false, err
	}
	pl, err := c.listSnaps(c.primary)
	if err != nil {
		return false, err
	}
	if n0 := findSlot(pl, "n0"); n0 != nil && !c.changed() {
		c.confOnlyLocal()
		return false, nil // nothing changed since n0
	}
	pnew := c.primary + "@" + c.prefix + "new"
	appCons, err := c.takeSnapshot(pnew)
	if err != nil {
		return false, err
	}
	confs := c.confsIfOn()
	fail := func(e error) (bool, error) {
		c.destroySnap(pnew)
		return false, e
	}
	c.lastFull = !zfsExists(c.backup)
	if !zfsExists(c.backup) {
		c.log.Printf("backup %s does not exist: full send", c.backup)
		if err := pipeSendRecv(c.sendArgs(pnew), []string{"recv", "-s", "-u", c.backup}); err != nil {
			return fail(err)
		}
		zfsRun("set", "readonly=on", c.backup)
		zfsRun("set", "canmount=noauto", c.backup)
		zfsRun("set", "cs:sync="+c.nr+":"+zfsGet("guid", c.primary), c.backup)
		if c.recursive {
			c.fixTree(c.backup)
		}
	} else {
		if tok := zfsGet("receive_resume_token", c.backup); tok != "" && tok != "-" {
			c.log.Printf("resuming interrupted receive")
			if err := pipeSendRecv([]string{"send", "-t", tok}, []string{"recv", "-s", "-u", c.backup}); err != nil {
				zfsRun("recv", "-A", c.backup) // token unusable: discard partial state
				c.log.Printf("WARN: resume failed, partial state discarded: %v", err)
			}
		}
		want := c.nr + ":" + zfsGet("guid", c.primary)
		if have := zfsGet("cs:sync", c.backup); have != want {
			return fail(fmt.Errorf("backup %s: cs:sync=%q, expected %q (refusing, wrong target?)", c.backup, have, want))
		}
		bl, err := c.listSnaps(c.backup)
		if err != nil {
			return fail(err)
		}
		bg := map[string]bool{}
		for _, b := range bl {
			bg[b.guid] = true
		}
		var base *zsnap
		for i := len(pl) - 1; i >= 0; i-- { // newest first, "new" not yet in pl
			if bg[pl[i].guid] {
				base = &pl[i]
				break
			}
		}
		if base == nil {
			return fail(fmt.Errorf("no common base snapshot"))
		}
		newest := bl[len(bl)-1]
		if base.guid != newest.guid && !c.rollbackAuto && !c.approved(base.guid, newest.guid) {
			c.setBlocked(base.guid, newest.guid)
			c.status("blocked: rollback needed (base " + base.slot + ", newest on backup " + newest.slot + ") - approve in frontend")
			return fail(nil)
		}
		if err := pipeSendRecv(c.sendArgs("-i", base.full, pnew), []string{"recv", "-s", "-F", "-u", c.backup}); err != nil {
			return fail(err)
		}
		if c.recursive {
			c.fixTree(c.backup)
		}
		if c.serviceID != "" {
			os.Remove(approvePath(c.serviceID)) // one-shot
		}
	}
	// confirm: backup must hold the snapshot with the same GUID
	pl2, _ := c.listSnaps(c.primary)
	pn := findSlot(pl2, "new")
	bl2, err := c.listSnaps(c.backup)
	if err != nil || pn == nil || findSlot(bl2, "new") == nil || findSlot(bl2, "new").guid != pn.guid {
		return fail(fmt.Errorf("backup did not confirm snapshot"))
	}
	if appCons { // snapshot user properties are not part of a plain send: set on both sides
		zfsRun("set", "cs:consistent=app", pn.full)
		zfsRun("set", "cs:consistent=app", findSlot(bl2, "new").full)
	}
	if len(confs) > 0 {
		c.applyConfs(c.primary, confs, c.prefix+"new")
		c.applyConfs(c.backup, confs, c.prefix+"new")
		c.confMarkSeen(confs)
	}
	if err := c.rotate(c.primary, c.versions); err != nil {
		return true, err
	}
	return true, c.rotate(c.backup, true)
}

// freezeDue: freeze=hh:mm is set, that time has passed today and no
// freeze attempt was made yet today.
func (c *zcfg) freezeDue() bool {
	if c.freeze == "" {
		return false
	}
	now := time.Now()
	if c.freezeTried == now.Format("20060102") {
		return false
	}
	t, err := time.Parse("15:04", c.freeze)
	if err != nil {
		return false
	}
	due := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
	return !now.Before(due)
}

// takeSnapshot creates the cycle snapshot. Once a day (freeze=hh:mm) it
// goes through cs-freeze4snap (freeze -> snapshot -> thaw); the tool never
// blocks the snapshot itself. Returns true if the freeze tool took it.
func (c *zcfg) snapshotRec(full string) (string, error) {
	if c.recursive {
		return zfsRun("snapshot", "-r", full)
	}
	return zfsRun("snapshot", full)
}

func (c *zcfg) takeSnapshot(full string) (bool, error) {
	if c.freezeDue() {
		c.freezeTried = time.Now().Format("20060102")
		i := strings.Index(full, "@")
		args := append([]string{"snap", "--dataset", full[:i], "--name", full[i+1:], "--recursive=" + strconv.FormatBool(c.recursive)}, c.freezeArgs...)
		cmd := exec.Command(c.freezeBin, args...)
		out, err := cmd.CombinedOutput()
		c.log.Printf("cs-freeze4snap: %s", strings.TrimSpace(string(out)))
		if err == nil {
			return true, nil
		}
		c.log.Printf("WARN: freeze failed (%v), taking a plain snapshot", err)
		c.destroySnap(full) // tool may have left a snapshot behind
	}
	_, err := c.snapshotRec(full)
	return false, err
}

func parseKeep(s string, hours, days *int) {
	for _, p := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(p), ":", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.Atoi(kv[1])
		if err != nil {
			continue
		}
		switch kv[0] {
		case "hours":
			*hours = n
		case "days":
			*days = n
		}
	}
}

func zfsCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.String("mode", "zfs", "zfs")
	primary := fs.String("primary", "", "primary ZFS dataset (required)")
	backup := fs.String("backup", "", "backup ZFS dataset on this host (required)")
	nr := fs.String("nr", "1", "service number, part of the snapshot names")
	interval := fs.Duration("interval", 3*time.Second, "minimum pause between cycles (min 1s)")
	versions := fs.String("versions", "y", "y|n: keep hourly/daily snapshots on the primary too")
	recursive := fs.String("recursive", "n", "y|n: replicate child datasets too (zfs send -R, e.g. Proxmox VM disks below rpool/data)")
	keep := fs.String("keep", "hours:24,days:30", "retention of kept snapshots, hours:N,days:N")
	fs.String("hold", "2s", "last N base snapshots (n0,n1) -- fixed at 2s for now")
	rollback := fs.String("rollback", "ask", "auto|ask: recv -F when the common base is older than the newest backup snapshot")
	freeze := fs.String("freeze", "", "hh:mm: once a day take the cycle snapshot via cs-freeze4snap (Proxmox/ESXi), empty=off")
	freezeBin := fs.String("freeze-bin", "cs-freeze4snap", "path of the cs-freeze4snap binary")
	freezeArgs := fs.String("freeze-args", "", "extra arguments for cs-freeze4snap snap, space separated (e.g. --policy memkeep=3)")
	backupHost := fs.String("backup-host", "", "LAN pair: host[:port] of the cs-sync zfsreceive on the backup member (instead of a local --backup)")
	key := fs.String("key", "", "transfer key for --backup-host")
	serviceID := fs.String("service-id", "", "status file _cfg/sync/<id>.last")
	logPath := fs.String("log", "", "log file path")
	fs.Parse(args)
	if *primary == "" || (*backup == "") == (*backupHost == "") || *primary == *backup {
		fmt.Fprintln(os.Stderr, "error: zfs mode needs --primary <dataset> and either a different local --backup <dataset> or --backup-host host[:port] --key <k>")
		os.Exit(2)
	}
	if *backupHost != "" {
		if *key == "" {
			fmt.Fprintln(os.Stderr, "error: --backup-host needs --key")
			os.Exit(2)
		}
		if !strings.Contains(*backupHost, ":") {
			*backupHost += ":9010"
		}
	}
	if *interval < time.Second {
		*interval = time.Second
	}
	log, err := logging.New(*logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot open log:", err)
		os.Exit(1)
	}
	c := &zcfg{primary: *primary, backup: *backup, nr: *nr, versions: *versions != "n",
		keepHours: 24, keepDays: 30, rollbackAuto: *rollback == "auto", serviceID: *serviceID, log: log}
	parseKeep(*keep, &c.keepHours, &c.keepDays)
	c.freeze, c.freezeBin, c.freezeArgs = *freeze, *freezeBin, strings.Fields(*freezeArgs)
	c.remoteAddr, c.key, c.recursive = *backupHost, *key, *recursive == "y"
	c.pveConf = pveAvailable() // fixed behaviour: a Proxmox source always carries the VM/CT config along
	if c.remoteAddr != "" {
		c.backup = c.remoteAddr
	}
	g := zfsGet("guid", *primary)
	if g == "" {
		log.Printf("FATAL: primary dataset %s not found", *primary)
		writeStatus(*serviceID, "error: primary dataset not found")
		os.Exit(1)
	}
	c.prefix = "sync_" + *nr + "_" + g + "_"
	if c.pveConf {
		log.Printf("Proxmox source detected: VM/CT configs of the synced disks are stored as cs:pveconf")
	}
	c.status(fmt.Sprintf("started (%s) zfs %s -> %s", time.Now().Format("2006-01-02 15:04:05"), *primary, *backup))
	var lastDur time.Duration
	for {
		t0 := time.Now()
		sent, err := c.cycle()
		lastDur = time.Since(t0)
		switch {
		case err != nil:
			c.status("error: " + err.Error())
		case sent:
			c.status("ok " + time.Now().Format("15:04:05") + " (" + lastDur.Round(time.Millisecond).String() + ")")
		}
		wait := *interval
		if 2*lastDur > wait && sent && !c.lastFull { // adaptive: slow incremental sends widen the interval (not the initial full send: the next write must sync at once)
			wait = 2 * lastDur
		}
		if err != nil && wait < 10*time.Second {
			wait = 10 * time.Second
		}
		time.Sleep(wait)
	}
}

var _ = sort.Strings
