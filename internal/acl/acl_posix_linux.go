//go:build linux

package acl

import (
	"bytes"
	"os"
	"os/exec"
)

// readPosixACL uses getfacl (acl package, Linux only). Output includes
// comment lines (# file:, # owner:, # group:) which are stripped so the
// stored text is just the ACL entries -- directly re-appliable via setfacl.
func readPosixACL(path string) (string, error) {
	out, err := exec.Command("getfacl", "-p", "--omit-header", path).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// applyPosixACL replaces the folder's ACL from the stored entries.
func applyPosixACL(path, text string) error {
	cmd := exec.Command("setfacl", "--set-file=-", path)
	cmd.Stdin = bytes.NewReader([]byte(text))
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
