package endpoint

import "testing"

func TestParseLocal(t *testing.T) {
	cases := []string{
		`D:\data`, `D:/data`, `C:\`, `/tank/data`, `/`, `./data`, `../data`, `./`, `..\data`,
	}
	for _, c := range cases {
		e, err := Parse(c)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", c, err)
			continue
		}
		if e.Kind != KindLocal {
			t.Errorf("Parse(%q): kind=%v, want KindLocal", c, e.Kind)
		}
		if e.Path != c {
			t.Errorf("Parse(%q): path=%q, want %q", c, e.Path, c)
		}
	}
}

func TestParseCSSync(t *testing.T) {
	e, err := Parse("192.168.2.189")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindCSSync || e.Host != "192.168.2.189" || e.Port != defaultCSSyncPort {
		t.Errorf("got %+v", e)
	}

	e, err = Parse("192.168.2.189:9500")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindCSSync || e.Port != 9500 {
		t.Errorf("got %+v", e)
	}

	e, err = Parse("my-w11.local")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindCSSync || e.Host != "my-w11.local" {
		t.Errorf("got %+v", e)
	}
}

func TestParseRustFS(t *testing.T) {
	e, err := Parse("192.168.2.189/s3share")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindRustFS || e.Host != "192.168.2.189" || e.Port != defaultRustFSPort || e.Bucket != "s3share" {
		t.Errorf("got %+v", e)
	}

	e, err = Parse("192.168.2.189:9000/s3share")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindRustFS || e.Port != 9000 || e.Bucket != "s3share" {
		t.Errorf("got %+v", e)
	}

	e, err = Parse("rustfs.example.com/bucket-name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindRustFS || e.Host != "rustfs.example.com" || e.Bucket != "bucket-name" {
		t.Errorf("got %+v", e)
	}
}

func TestParseErrors(t *testing.T) {
	cases := []string{
		"",             // empty
		"host:abc",     // non-numeric port
		"host:0",       // out-of-range port
		"host:99999",   // out-of-range port
		"host//bucket", // empty first segment before //, invalid host
	}
	for _, c := range cases {
		if _, err := Parse(c); err == nil {
			t.Errorf("Parse(%q): expected error, got none", c)
		}
	}
	// "/a/b/:9000" starts with "/" -> must be treated as a (weird but
	// valid-looking) local path, not host:port -- local-path rules win.
	e, err := Parse("/a/b/:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindLocal {
		t.Errorf("Parse(%q): kind=%v, want KindLocal (leading / always wins)", "/a/b/:9000", e.Kind)
	}
}

// TestParseBareRelativeIsRemoteNotError documents the deliberate
// consequence noted in the package doc comment: a bare relative path
// with no ./ prefix ("data/subdir") is syntactically indistinguishable
// from a remote host+bucket spec and is ALWAYS parsed as the latter --
// never an error, never guessed as local. Local relative paths must use
// the explicit ./ prefix (see TestParseLocal).
func TestParseBareRelativeIsRemoteNotError(t *testing.T) {
	e, err := Parse("data/subdir")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindRustFS || e.Host != "data" || e.Bucket != "subdir" {
		t.Errorf("got %+v, want KindRustFS host=data bucket=subdir (documented deliberate behavior)", e)
	}
}

func TestParseWindowsDriveNotMistakenForHostPort(t *testing.T) {
	// The one documented trap: a typo like "D:9000" (missing slash) is
	// NOT a Windows path per our regex (needs \ or / right after the
	// colon) and IS syntactically a valid host:port ("D" as a DNS label,
	// port 9000) -- host label "D" matches hostRe. This is expected/
	// documented fallthrough behavior, not a bug: we assert it here so
	// a future grammar change doesn't silently alter it without a
	// deliberate decision.
	e, err := Parse("D:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e.Kind != KindCSSync || e.Host != "D" || e.Port != 9000 {
		t.Errorf("got %+v (documented trap case)", e)
	}
}

func TestIsLocal(t *testing.T) {
	e, _ := Parse("localhost/bucket")
	if !e.IsLocal() {
		t.Error("localhost should be IsLocal")
	}
	e, _ = Parse("127.0.0.1:9000/bucket")
	if !e.IsLocal() {
		t.Error("127.0.0.1 should be IsLocal")
	}
	e, _ = Parse("192.168.2.189/bucket")
	if e.IsLocal() {
		t.Error("192.168.2.189 should not be IsLocal")
	}
	e, _ = Parse("/tank/data")
	if e.IsLocal() {
		t.Error("a local path itself should report IsLocal()==false (meaningless/inapplicable, not true)")
	}
}

func TestHostPort(t *testing.T) {
	e, _ := Parse("192.168.2.189/s3share")
	if got := e.HostPort(); got != "192.168.2.189:9000" {
		t.Errorf("HostPort()=%q, want 192.168.2.189:9000", got)
	}
}
