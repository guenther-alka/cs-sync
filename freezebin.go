package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// resolveFreezeBin finds cs-freeze4snap: an explicit --freeze-bin path, else the
// same folder as cs-sync (standalone setup), else the napp-it tools layout
// (../../cs-freeze4snap/<os>.<arch>/ next to the cs-sync/<os>.<arch>/ folder),
// else PATH. Returns "" if it is not there: the freeze is then skipped.
func resolveFreezeBin(flagVal string) string {
	name := "cs-freeze4snap"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	isFile := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.Mode().IsRegular()
	}
	if flagVal != "" && strings.ContainsAny(flagVal, `/\`) {
		if isFile(flagVal) {
			return flagVal
		}
		return ""
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, p := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "..", "..", "cs-freeze4snap", filepath.Base(dir), name),
		} {
			if isFile(p) {
				return p
			}
		}
	}
	if flagVal == "" {
		flagVal = name
	}
	if p, err := exec.LookPath(flagVal); err == nil {
		return p
	}
	return ""
}

func frozenVal(v string) bool { return v == "app" || v == "paused" }

// consistencyOf reads the cs-freeze4snap result: "app" if every guest was really
// frozen (guest agent, container freeze, memory snapshot, ESXi quiesce/memory),
// "paused" if a VM had to be paused (crash-consistent), "" otherwise.
func consistencyOf(out []byte) string {
	var r struct {
		Guests []struct {
			Strategy string `json:"strategy"`
		} `json:"guests"`
	}
	i := strings.Index(string(out), "{")
	if i < 0 || json.Unmarshal(out[i:], &r) != nil || len(r.Guests) == 0 {
		return ""
	}
	res := "app"
	for _, g := range r.Guests {
		s := g.Strategy
		switch {
		case s == "qmp-pause":
			res = "paused"
		case s == "qga" || s == "fsfreeze" || s == "cgroup" || s == "esxi-quiesce" || s == "esxi-mem" || strings.Contains(s, "mem") || strings.Contains(s, "vmstate"):
		default:
			return ""
		}
	}
	return res
}
