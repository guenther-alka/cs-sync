package rustfs

import (
	"testing"

	"github.com/guenther-alka/cs-sync/internal/model"
)

func TestParseLsjson(t *testing.T) {
	// Representative of real `rclone lsjson --recursive` output against
	// an S3/RustFS remote: files and (for nested keys) implicit directory
	// entries.
	input := `[
		{"Path":"rufus-3.20p.exe","Name":"rufus-3.20p.exe","Size":1395336,"MimeType":"application/x-msdownload","ModTime":"2026-08-10T15:37:41.000000000Z","IsDir":false},
		{"Path":"documents","Name":"documents","Size":-1,"MimeType":"inode/directory","ModTime":"2026-08-10T15:00:00.000000000Z","IsDir":true},
		{"Path":"documents/reports","Name":"reports","Size":-1,"MimeType":"inode/directory","ModTime":"2026-08-10T15:00:00.000000000Z","IsDir":true},
		{"Path":"documents/reports/q1.pdf","Name":"q1.pdf","Size":45678,"MimeType":"application/pdf","ModTime":"2026-08-10T16:00:00.000000000Z","IsDir":false}
	]`

	tree, err := parseLsjson([]byte(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(tree) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(tree), tree)
	}

	f, ok := tree["rufus-3.20p.exe"]
	if !ok {
		t.Fatal("expected entry for rufus-3.20p.exe")
	}
	if f.Type != model.TypeFile {
		t.Errorf("rufus-3.20p.exe: got Type=%q, want %q", f.Type, model.TypeFile)
	}
	if f.Size != 1395336 {
		t.Errorf("rufus-3.20p.exe: got Size=%d, want 1395336", f.Size)
	}
	if f.MtimeNS == 0 {
		t.Error("rufus-3.20p.exe: MtimeNS was not parsed (still zero)")
	}
	// Ino/Dev must stay zero -- see scan.go's doc comment (no rename
	// correlation is possible or attempted for RustFS-side entries).
	if f.Ino != 0 || f.Dev != 0 {
		t.Errorf("rufus-3.20p.exe: expected Ino/Dev to stay zero, got Ino=%d Dev=%d", f.Ino, f.Dev)
	}

	d, ok := tree["documents"]
	if !ok || d.Type != model.TypeDir {
		t.Errorf("documents: got %+v, want Type=%q", d, model.TypeDir)
	}

	nested, ok := tree["documents/reports/q1.pdf"]
	if !ok || nested.Type != model.TypeFile || nested.Size != 45678 {
		t.Errorf("documents/reports/q1.pdf: got %+v", nested)
	}
}

func TestParseLsjson_Empty(t *testing.T) {
	tree, err := parseLsjson([]byte(`[]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(tree) != 0 {
		t.Errorf("expected empty tree, got %+v", tree)
	}
}

func TestParseLsjson_InvalidJSON(t *testing.T) {
	_, err := parseLsjson([]byte(`not json`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestParseLsjson_LeadingSlashStripped(t *testing.T) {
	// rclone lsjson paths are normally relative already, but strip a
	// leading slash defensively in case a particular backend/version
	// emits one.
	tree, err := parseLsjson([]byte(`[{"Path":"/foo.txt","Size":1,"ModTime":"2026-08-10T15:00:00Z","IsDir":false}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tree["foo.txt"]; !ok {
		t.Errorf("expected key %q (leading slash stripped), got keys: %+v", "foo.txt", tree)
	}
}
