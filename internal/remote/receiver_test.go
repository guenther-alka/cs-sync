package remote

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFullValidPaths asserts that legitimate wire relpaths resolve to a
// location strictly inside Dest.
func TestFullValidPaths(t *testing.T) {
	rv := &Receiver{Dest: t.TempDir()}
	cases := []string{
		"foo.txt",
		"a/b/c.txt",
		".",
		"dir with spaces/file.txt",
	}
	for _, rel := range cases {
		full, err := rv.full(rel)
		if err != nil {
			t.Errorf("full(%q): unexpected error: %v", rel, err)
			continue
		}
		rel2, err := filepath.Rel(rv.Dest, full)
		if err != nil {
			t.Errorf("full(%q): Rel(%q,%q) failed: %v", rel, rv.Dest, full, err)
			continue
		}
		if rel2 == ".." || strings.HasPrefix(rel2, ".."+string(filepath.Separator)) {
			t.Errorf("full(%q): result %q escaped dest", rel, full)
		}
	}
}

// TestFullRejectsTraversal asserts the S1 fix: ".." components must never
// be allowed to escape the sync root.
func TestFullRejectsTraversal(t *testing.T) {
	rv := &Receiver{Dest: t.TempDir()}
	cases := []string{
		"../x",
		"foo/../../x",
		"../../etc/passwd",
		`..\..\windows`, // backslash form
	}
	for _, rel := range cases {
		if _, err := rv.full(rel); err == nil {
			t.Errorf("full(%q): expected traversal error, got none", rel)
		}
	}
}

// TestFullRejectsAbsolute asserts absolute paths (drive letter on Windows,
// rooted path elsewhere) are refused rather than joined under Dest.
func TestFullRejectsAbsolute(t *testing.T) {
	rv := &Receiver{Dest: t.TempDir()}
	var abs string
	if runtime.GOOS == "windows" {
		abs = `C:\evil\file.txt`
	} else {
		abs = "/etc/passwd"
	}
	if _, err := rv.full(abs); err == nil {
		t.Errorf("full(%q): expected absolute-path error, got none", abs)
	}
}
