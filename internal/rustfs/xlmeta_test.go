package rustfs

import (
	"os"
	"path/filepath"
	"testing"
)

// mkObject creates a fake RustFS object directory (xl.meta + a UUID-named
// shard subdir with part.1) under bucketRoot, matching the real on-disk
// layout confirmed live against omnio46~192.168.2.189 in this design
// session (see sync-2.1-design.info section 14).
func mkObject(t *testing.T, bucketRoot, key string) {
	t.Helper()
	objDir := filepath.Join(bucketRoot, filepath.FromSlash(key))
	if err := os.MkdirAll(objDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objDir, "xl.meta"), []byte("XL2fake"), 0644); err != nil {
		t.Fatal(err)
	}
	shardDir := filepath.Join(objDir, "3fb79281-4c00-4bc6-b5e2-91fee0df7f6f")
	if err := os.MkdirAll(shardDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shardDir, "part.1"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveObjectKey_FlatKey(t *testing.T) {
	root := t.TempDir()
	mkObject(t, root, "rufus-3.20p.exe")

	// Event fires on the deepest changed file (part.1), the most common
	// real case (see section 14's live test).
	changed := filepath.Join(root, "rufus-3.20p.exe", "3fb79281-4c00-4bc6-b5e2-91fee0df7f6f", "part.1")
	key, ok := ResolveObjectKey(changed, root)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if key != "rufus-3.20p.exe" {
		t.Errorf("got key=%q, want %q", key, "rufus-3.20p.exe")
	}
}

func TestResolveObjectKey_XlMetaItself(t *testing.T) {
	root := t.TempDir()
	mkObject(t, root, "zfstest.bat")

	changed := filepath.Join(root, "zfstest.bat", "xl.meta")
	key, ok := ResolveObjectKey(changed, root)
	if !ok || key != "zfstest.bat" {
		t.Errorf("got key=%q ok=%v, want %q true", key, ok, "zfstest.bat")
	}
}

func TestResolveObjectKey_NestedKey(t *testing.T) {
	root := t.TempDir()
	// S3 keys may contain "/" (virtual folder structure) -- section 4.2.
	mkObject(t, root, "documents/reports/q1.pdf")

	changed := filepath.Join(root, "documents", "reports", "q1.pdf", "xl.meta")
	key, ok := ResolveObjectKey(changed, root)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if key != "documents/reports/q1.pdf" {
		t.Errorf("got key=%q, want %q", key, "documents/reports/q1.pdf")
	}
}

func TestResolveObjectKey_NoXlMetaFound(t *testing.T) {
	root := t.TempDir()
	// A plain file with no xl.meta anywhere above it up to root -- e.g.
	// the "rufus-4.7p.exe" case from section 2/14 (a file placed directly
	// on the ZFS path, invisible to the S3 API).
	if err := os.WriteFile(filepath.Join(root, "rufus-4.7p.exe"), []byte("data"), 0644); err != nil {
		t.Fatal(err)
	}
	_, ok := ResolveObjectKey(filepath.Join(root, "rufus-4.7p.exe"), root)
	if ok {
		t.Error("expected ok=false for a path with no xl.meta anywhere above it")
	}
}

func TestResolveObjectKey_PathOutsideBucketRoot(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	_, ok := ResolveObjectKey(filepath.Join(other, "somefile"), root)
	if ok {
		t.Error("expected ok=false for a path outside bucketRoot")
	}
}

func TestResolveObjectKey_ChangedPathIsBucketRootItself(t *testing.T) {
	root := t.TempDir()
	_, ok := ResolveObjectKey(root, root)
	if ok {
		t.Error("expected ok=false when changedPath is the bucket root itself")
	}
}
