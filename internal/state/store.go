// Package state persists the sync baseline (section 5 of cs-sync.info).
package state

import (
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/model"
)

const FileName = "cs-sync.state"

// Dir returns the .backupdata directory under root, creating it if needed.
func Dir(root string) (string, error) {
	d := filepath.Join(root, ".backupdata")
	if err := os.MkdirAll(d, 0700); err != nil {
		return "", fmt.Errorf("create %s: %w", d, err)
	}
	return d, nil
}

// Load reads the baseline state from primaryRoot/.backupdata/cs-sync.state.
// Missing or unreadable file is treated as "first run" (empty state, no error).
func Load(primaryRoot string) (*model.State, error) {
	dir, err := Dir(primaryRoot)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, FileName)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &model.State{Baseline: model.Tree{}}, nil
		}
		return &model.State{Baseline: model.Tree{}}, nil // corrupt -> first run, do not fail startup
	}
	defer f.Close()

	var st model.State
	if err := gob.NewDecoder(f).Decode(&st); err != nil {
		return &model.State{Baseline: model.Tree{}}, nil // corrupt -> first run
	}
	if st.Baseline == nil {
		st.Baseline = model.Tree{}
	}
	return &st, nil
}

// Save writes the baseline atomically: write to a temp file, fsync, rename.
func Save(primaryRoot string, st *model.State) error {
	dir, err := Dir(primaryRoot)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, FileName)
	tmp := final + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create tmp state: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(st); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("fsync state: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}
