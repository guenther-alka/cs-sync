//go:build freebsd

package acl

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strings"
)

// BUG FIX 2026.07.24 (real-host validation on FreeBSD 15.1, 192.168.2.191,
// tank/data2 acltype=nfsv4): this file used to be combined with linux in
// acl_nfs4_linuxfreebsd.go, calling nfs4_getfacl/nfs4_setfacl on both --
// but FreeBSD has NO nfs4_getfacl/nfs4_setfacl at all (`which` found
// nothing; only Linux ships the nfs4-acl-tools package). FreeBSD's own
// getfacl/setfacl handle NFSv4 ACLs natively (same binaries as for
// posix1e ACLs) -- confirmed live: `getfacl <dir>` on an nfsv4 dataset
// prints the same owner@/group@/everyone@/user:name@ ACE format already
// assumed by the (correct) parsing logic below. ACL sync on FreeBSD was
// 100% non-functional before this fix (every call errored with "exec:
// nfs4_getfacl: executable file not found in $PATH").
//
// "replace whole ACL" flag (the other previously-flagged open question):
// FreeBSD's setfacl has NO Linux-style "-S file" (replace-from-file) verb
// -- its model is merge-only (-m/-M add or modify listed entries, existing
// entries not mentioned are left alone). Verified live round-trip:
//  1. setfacl -b <path>       -- reset to the trivial 3-entry base ACL
//                                (owner@/group@/everyone@ only, all
//                                extended ACEs removed)
//  2. setfacl -M <file> <path> -- (re)apply the full ACE list from file
// Confirmed byte-identical getfacl output after steps 1+2 vs. the
// original ACL (incl. a non-trivial user:daemon:... ACE) on the real
// host. This two-step reset+merge is therefore FreeBSD's equivalent of
// Linux's single-step nfs4_setfacl -S.
func readNFS4ACL(path string) (string, error) {
	out, err := exec.Command("getfacl", path).Output()
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}

func applyNFS4ACL(path, text string) error {
	// step 1: reset to trivial base -- otherwise -M only merges/adds and
	// can never remove an ACE that existed before but is not in `text`
	if err := exec.Command("setfacl", "-b", path).Run(); err != nil {
		return err
	}

	tmp, err := os.CreateTemp("", "cs-sync-nfs4-*.acl")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	// step 2: apply the full ACE list from file
	cmd := exec.Command("setfacl", "-M", tmp.Name(), path)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
