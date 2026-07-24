package remote

import (
	"encoding/gob"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/guenther-alka/cs-sync/internal/guard"
	"github.com/guenther-alka/cs-sync/internal/model"
)

// Per-pair persisted files inside StateDir (.backupdata/remote_<name>/):
//   remote.state  -- last receiver-acked tree (baseline == coalesced queue)
//   retry.state   -- guard.RetryState (backoff + quarantine)
//   source.id     -- guard.SourceID (section 5a liveness anchor)
//   delete-freeze.txt -- guard freeze marker (section 5b)

const (
	remoteBaselineFile = "remote.state"
	sourceIDFile       = "source.id"
)

func loadRemoteBaseline(dir string) model.Tree {
	f, err := os.Open(filepath.Join(dir, remoteBaselineFile))
	if err != nil {
		return model.Tree{}
	}
	defer f.Close()
	var t model.Tree
	if gob.NewDecoder(f).Decode(&t) != nil || t == nil {
		return model.Tree{}
	}
	return t
}

func saveRemoteBaseline(dir string, t model.Tree) {
	tmp := filepath.Join(dir, remoteBaselineFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if gob.NewEncoder(f).Encode(t) != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	f.Sync()
	f.Close()
	os.Rename(tmp, filepath.Join(dir, remoteBaselineFile))
}

func loadSourceID(dir string) guard.SourceID {
	var s guard.SourceID
	f, err := os.Open(filepath.Join(dir, sourceIDFile))
	if err != nil {
		return s
	}
	defer f.Close()
	_ = gob.NewDecoder(f).Decode(&s)
	return s
}

func saveSourceID(dir string, s guard.SourceID) {
	tmp := filepath.Join(dir, sourceIDFile+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	if gob.NewEncoder(f).Encode(s) != nil {
		f.Close()
		os.Remove(tmp)
		return
	}
	f.Close()
	os.Rename(tmp, filepath.Join(dir, sourceIDFile))
}

// devOf extracts the device id from a FileInfo, mirroring
// scanner.devIno's platform split without exporting it.
func devOf(fi fs.FileInfo) uint64 { return devOfPlatform(fi) }
