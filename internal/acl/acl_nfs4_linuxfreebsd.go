//go:build linux || freebsd

package acl

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strings"
)

// readNFS4ACL uses nfs4_getfacl (Linux: nfs4-acl-tools package; FreeBSD:
// native). Comment lines (leading '#') are stripped, leaving one ACE
// per line in the canonical form nfs4_setfacl expects back.
func readNFS4ACL(path string) (string, error) {
	out, err := exec.Command("nfs4_getfacl", path).Output()
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

// applyNFS4ACL replaces the folder's full ACL from the stored ACE lines.
// NOTE (v1 TODO): -S (Linux nfs4-acl-tools) replaces the whole ACL from a
// file. FreeBSD's nfs4_setfacl flag for "replace whole ACL from file" may
// differ slightly (verify against the target FreeBSD release's man page
// before production use) -- flagged here rather than silently assumed.
func applyNFS4ACL(path, text string) error {
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

	cmd := exec.Command("nfs4_setfacl", "-S", tmp.Name(), path)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
