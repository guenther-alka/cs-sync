package main

// Proxmox VM/CT configuration as ZFS user properties (cs-sync v3.2.0).
//
// A replicated zvol alone is not a restorable VM: hardware, network, boot
// order and the disk assignment live in /etc/pve/qemu-server/<id>.conf (or
// lxc/<id>.conf). With --pve-config the sender reads the config of every VM
// or container whose disk datasets (vm-<id>-disk-N, subvol-<id>-disk-N,
// base-<id>-disk-N) are part of the sync and stores it as gzip+base64 in the
// user property cs:pveconf (continued in cs:pveconf.1, .2 ... because one
// property value is limited to about 8 KB):
//   - on the disk dataset itself: the current config, also updated when only
//     the config changes (no zfs send)
//   - on every sync snapshot: the config at the time of that snapshot
// The properties are set on both sides, independent of the backup OS.
// "cs-sync pveconf" shows, lists and restores them.

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const confChunk = 6000

var diskRe = regexp.MustCompile(`^(vm|subvol|base)-(\d+)-disk-\d+`)

func pveAvailable() bool {
	st, err := os.Stat("/etc/pve/nodes")
	return err == nil && st.IsDir()
}

func confProp(i int) string {
	if i == 0 {
		return "cs:pveconf"
	}
	return fmt.Sprintf("cs:pveconf.%d", i)
}

func findConfFile(sub, id string) string {
	p := "/etc/pve/" + sub + "/" + id + ".conf"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	m, _ := filepath.Glob("/etc/pve/nodes/*/" + sub + "/" + id + ".conf")
	if len(m) > 0 {
		sort.Strings(m)
		return m[0]
	}
	return ""
}

// encodeConf packs "#cs-pve <kind> <id>" + the config file. The id travels inside the
// value because the backup dataset names need not contain it (tank/_dr/disk1).
func encodeConf(kind, id string, b []byte) string {
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression) // header without timestamp: same input = same output
	fmt.Fprintf(zw, "#cs-pve %s %s\n", kind, id)
	zw.Write(b)
	zw.Close()
	return "gz1:" + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func decodeConf(s string) (kind, id string, body []byte, err error) {
	if !strings.HasPrefix(s, "gz1:") {
		return "", "", nil, fmt.Errorf("unknown cs:pveconf format")
	}
	raw, err := base64.StdEncoding.DecodeString(s[4:])
	if err != nil {
		return
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return
	}
	all, err := io.ReadAll(zr)
	if err != nil {
		return
	}
	nl := bytes.IndexByte(all, '\n')
	f := strings.Fields(string(all[:max(nl, 0)]))
	if nl < 0 || len(f) != 3 || f[0] != "#cs-pve" {
		return "", "", nil, fmt.Errorf("cs:pveconf without header")
	}
	return f[1], f[2], all[nl+1:], nil
}

func joinDS(base, rel string) string {
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

func unset(v string) bool { return v == "" || v == "-" }

// getConfProp reads the (possibly chunked) config property of a dataset or snapshot.
func getConfProp(target string) string {
	var sb strings.Builder
	for i := 0; ; i++ {
		v := zfsGet(confProp(i), target)
		if unset(v) {
			break
		}
		sb.WriteString(v)
	}
	return sb.String()
}

func setConfProp(target, enc string) error {
	n := 0
	for i := 0; i*confChunk < len(enc); i++ {
		end := (i + 1) * confChunk
		if end > len(enc) {
			end = len(enc)
		}
		if _, err := zfsRun("set", confProp(i)+"="+enc[i*confChunk:end], target); err != nil {
			return err
		}
		n++
	}
	if !strings.Contains(target, "@") { // dataset: drop chunks left over from a longer config
		for i := n; ; i++ {
			if unset(zfsGet(confProp(i), target)) {
				break
			}
			zfsRun("inherit", confProp(i), target)
		}
	}
	return nil
}

// pveConfs returns relative dataset name ("" = the primary itself) -> encoded config.
func (c *zcfg) pveConfs() map[string]string {
	names := []string{c.primary}
	if c.recursive {
		if out, err := zfsRun("list", "-r", "-H", "-o", "name", "-t", "filesystem,volume", c.primary); err == nil {
			names = strings.Fields(out)
		}
	}
	m := map[string]string{}
	for _, n := range names {
		mt := diskRe.FindStringSubmatch(n[strings.LastIndex(n, "/")+1:])
		if mt == nil {
			continue
		}
		sub := "qemu-server"
		if mt[1] == "subvol" {
			sub = "lxc"
		}
		f := findConfFile(sub, mt[2])
		if f == "" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil || len(b) == 0 {
			continue
		}
		kind := "qemu"
		if sub == "lxc" {
			kind = "lxc"
		}
		m[strings.TrimPrefix(strings.TrimPrefix(n, c.primary), "/")] = encodeConf(kind, mt[2], b)
	}
	return m
}

func (c *zcfg) confsIfOn() map[string]string {
	if !c.pveConf {
		return nil
	}
	return c.pveConfs()
}

// confDelta: configs that differ from the last stored/sent state.
func (c *zcfg) confDelta() map[string]string {
	if !c.pveConf {
		return nil
	}
	cur := c.pveConfs()
	if c.confSeen == nil {
		c.confSeen = map[string]string{}
		for rel := range cur {
			if v := getConfProp(joinDS(c.primary, rel)); v != "" {
				c.confSeen[rel] = v
			}
		}
	}
	d := map[string]string{}
	for rel, enc := range cur {
		if c.confSeen[rel] != enc {
			d[rel] = enc
		}
	}
	return d
}

func (c *zcfg) confMarkSeen(m map[string]string) {
	if c.confSeen == nil {
		c.confSeen = map[string]string{}
	}
	for k, v := range m {
		c.confSeen[k] = v
	}
}

// applyConfs sets the properties below base: on the dataset and, if snap != "", on its snapshot.
func (c *zcfg) applyConfs(base string, confs map[string]string, snap string) {
	for rel, enc := range confs {
		t := joinDS(base, rel)
		if err := setConfProp(t, enc); err != nil {
			c.log.Printf("WARN: pve config on %s: %v", t, err)
		}
		if snap != "" {
			if err := setConfProp(t+"@"+snap, enc); err != nil {
				c.log.Printf("WARN: pve config on %s@%s: %v", t, snap, err)
			}
		}
	}
}

// confOnlyLocal: nothing to send, but the VM config may have changed.
func (c *zcfg) confOnlyLocal() {
	d := c.confDelta()
	if len(d) == 0 {
		return
	}
	c.applyConfs(c.primary, d, "")
	if zfsExists(c.backup) {
		c.applyConfs(c.backup, d, "")
	}
	c.confMarkSeen(d)
	c.log.Printf("pve config changed: %d VM/CT updated (no zfs send)", len(d))
}

// ---------------------------------------------------------------- pveconf command

func pveconfCmd(args []string) {
	if len(args) < 1 {
		pveconfUsage()
	}
	sub := args[0]
	fs := flag.NewFlagSet("pveconf", flag.ExitOnError)
	ds := fs.String("dataset", "", "disk dataset (e.g. tank/vm-105-disk-0) or, with --recursive, its parent")
	snap := fs.String("snapshot", "", "snapshot name, slot (n0, kf1013, kh101321 ...) or empty = current config of the dataset")
	rec := fs.String("recursive", "n", "y|n: include child datasets")
	to := fs.String("to", ".", "restore: target directory (e.g. /etc/pve/qemu-server or /etc/pve/lxc)")
	mp := fs.String("map", "", "restore: storage name mapping old=new[,old2=new2]")
	force := fs.Bool("force", false, "restore: overwrite an existing <id>.conf")
	newid := fs.String("newid", "", "restore: use this VM/CT id instead of the original (disk names vm-<id>-disk-N / subvol-<id>-disk-N are renamed in the config)")
	fs.Parse(args[1:])
	if *ds == "" || (sub != "list" && sub != "show" && sub != "restore") {
		pveconfUsage()
	}
	names := []string{*ds}
	if *rec == "y" || sub == "list" {
		out, err := zfsRun("list", "-r", "-H", "-o", "name", "-t", "filesystem,volume", *ds)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		names = strings.Fields(out)
	}
	found := 0
	for _, n := range names {
		target := n
		if *snap != "" {
			t, err := resolveSnap(n, *snap)
			if err != nil {
				fmt.Fprintln(os.Stderr, "skip", n+":", err)
				continue
			}
			target = t
		}
		enc := getConfProp(target)
		if enc == "" {
			continue
		}
		found++
		kind, id, b, err := decodeConf(enc)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", target+":", err)
			os.Exit(1)
		}
		switch sub {
		case "list":
			fmt.Printf("%s\tid=%s\t%s\t%d bytes\n", target, id, kind, len(b))
			if ss, err := zfsRun("list", "-H", "-o", "name", "-t", "snapshot", "-d", "1", "-s", "createtxg", n); err == nil {
				for _, s := range strings.Fields(ss) {
					if !unset(zfsGet(confProp(0), s)) {
						fmt.Printf("\t%s\n", s)
					}
				}
			}
		case "show":
			fmt.Printf("# %s (id %s, %s)\n%s", target, id, kind, b)
		case "restore":
			text := string(b)
			for _, p := range strings.Split(*mp, ",") {
				kv := strings.SplitN(strings.TrimSpace(p), "=", 2)
				if len(kv) == 2 && kv[0] != "" {
					text = regexp.MustCompile(`(?m)^(\S+:\s*)`+regexp.QuoteMeta(kv[0])+`:`).ReplaceAllString(text, "${1}"+kv[1]+":")
				}
			}
			if *newid != "" {
				if !regexp.MustCompile(`^[0-9]+$`).MatchString(*newid) {
					fmt.Fprintln(os.Stderr, "error: --newid must be a number")
					os.Exit(1)
				}
				text = regexp.MustCompile(`\b(vm|subvol|base)-`+regexp.QuoteMeta(id)+`-(disk|cloudinit)`).ReplaceAllString(text, "${1}-"+*newid+"-${2}")
				fmt.Fprintf(os.Stderr, "note: id %s -> %s; the disk datasets must be named accordingly (vm-%s-disk-N); check MAC addresses/IPs of the copy\n", id, *newid, *newid)
				id = *newid
			}
			out := filepath.Join(*to, id+".conf")
			if _, err := os.Stat(out); err == nil && !*force {
				fmt.Fprintf(os.Stderr, "error: %s exists (VM/CT id %s already in use here?); choose another --to or use --force\n", out, id)
				os.Exit(1)
			}
			if err := os.WriteFile(out, []byte(text), 0640); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			fmt.Printf("written %s (from %s)\n", out, target)
		}
	}
	if found == 0 {
		fmt.Fprintln(os.Stderr, "no cs:pveconf found on", *ds, "(source without Proxmox, --pve-config n, or no sync with a VM disk dataset yet)")
		os.Exit(1)
	}
}

// resolveSnap: "n0", "kf1013" ... or a full snapshot name -> ds@snapshot.
func resolveSnap(ds, s string) (string, error) {
	if strings.Contains(s, "@") {
		return s, nil
	}
	out, err := zfsRun("list", "-H", "-o", "name", "-t", "snapshot", "-d", "1", "-s", "createtxg", ds)
	if err != nil {
		return "", err
	}
	// several services can have snapshots with the same slot (n0): newest one that carries a config
	res := ""
	for _, n := range strings.Fields(out) {
		at := n[strings.Index(n, "@")+1:]
		if at == s || strings.HasSuffix(at, "_"+s) {
			if res == "" || !unset(zfsGet(confProp(0), n)) {
				res = n
			}
		}
	}
	if res != "" {
		return res, nil
	}
	return "", fmt.Errorf("no snapshot %q", s)
}

func pveconfUsage() {
	fmt.Fprint(os.Stderr, `cs-sync pveconf list    --dataset <ds>                                  VM/CT configs stored on the dataset tree
cs-sync pveconf show    --dataset <disk-ds> [--snapshot n0|kf1013|<name>] [--recursive y]
cs-sync pveconf restore --dataset <disk-ds> [--snapshot ...] [--recursive y] --to /etc/pve/qemu-server|/etc/pve/lxc
                        [--map oldstorage=newstorage] [--newid N] [--force]
`)
	os.Exit(2)
}
