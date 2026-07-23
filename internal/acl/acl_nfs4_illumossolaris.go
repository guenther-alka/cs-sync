//go:build illumos || solaris

package acl

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
)

// illumos/Solaris have no nfs4_getfacl/nfs4_setfacl; the native tools are
// `ls -V` (read) and `chmod A=...` (write). VERIFIED 2026.07.23 on a real
// OmniOS r151056 host: `ls -Vd` renders TWO different styles depending on
// whether the ACL is "trivial" (unmodified default, matches plain POSIX
// permission bits) or non-trivial (real ACEs present):
//
//	trivial:     "     0:user::rwx"          (leading "N:" index)
//	non-trivial: "owner@:rwxp-DaARWcCos:-------:allow"   (no index)
//
// An earlier version of this file assumed the trivial form always and
// stripped up to the first colon unconditionally -- which silently
// truncated the owner@/group@/everyone@ prefix on non-trivial ACLs.
// Fixed: only strip a leading "N:" when the line actually starts with
// digits followed by a colon; otherwise use the line as-is. Round-trip
// (read from one folder, chmod A= onto another) confirmed byte-identical
// on the real host for the non-trivial case. `ls -v` (lowercase) is NOT
// more compact -- it spells out permission names in full across multiple
// lines; `-V` (uppercase) is the compact, chmod-compatible form.
func readNFS4ACL(path string) (string, error) {
	out, err := exec.Command("ls", "-Vd", path).Output()
	if err != nil {
		return "", err
	}
	var entries []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			first = false
			continue // first line is the "ls -l"-style listing, not an ACE
		}
		if line == "" {
			continue
		}
		entries = append(entries, stripTrivialIndex(line))
	}
	return strings.Join(entries, ","), nil
}

// stripTrivialIndex removes a leading "N:" (digits + colon) ONLY when
// present -- the trivial-ACL display form. Non-trivial ACE lines
// (owner@:..., group@:..., user:name@:...) have no such prefix and are
// returned unchanged.
func stripTrivialIndex(line string) string {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i < len(line) && line[i] == ':' {
		return line[i+1:]
	}
	return line
}

func applyNFS4ACL(path, text string) error {
	cmd := exec.Command("chmod", "A="+text, path)
	return cmd.Run()
}
