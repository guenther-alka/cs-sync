//go:build illumos || solaris

package acl

import (
	"bufio"
	"bytes"
	"os/exec"
	"strings"
)

// illumos/Solaris have no nfs4_getfacl/nfs4_setfacl; the native tools are
// `ls -V` (read) and `chmod A=...` (write). `ls -V` prints one ACE per
// line as "N:entry" -- stripping the "N:" index and joining with commas
// gives exactly the argument `chmod A=` expects back, so the stored text
// is that already-stripped, comma-joined form (canonical storage form).
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
		if idx := strings.Index(line, ":"); idx >= 0 {
			entries = append(entries, line[idx+1:])
		}
	}
	return strings.Join(entries, ","), nil
}

func applyNFS4ACL(path, text string) error {
	cmd := exec.Command("chmod", "A="+text, path)
	return cmd.Run()
}
