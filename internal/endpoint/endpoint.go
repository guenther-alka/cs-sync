// Package endpoint implements cs-sync v3.0's unified endpoint syntax
// (sync-3.0-design.info): a single string, used for --primary,
// --secondary, and --backup alike, that self-describes as one of three
// kinds -- a local filesystem path, a remote cs-sync service (wire
// protocol), or a RustFS/S3 bucket -- with no separate flag needed to
// say which.
//
// Grammar (checked in this order, first match wins):
//
//  1. ^[A-Za-z]:[\\/]   Windows path       D:\data, C:/share
//  2. ^/                Unix absolute path /tank/data
//  3. ^\.\.?[/\\]        explicit relative  ./data, ../data
//  4. otherwise: host[:port][/bucket] -- ALWAYS, never guessed as a
//     relative path, even for a single-label word with no "/" at all
//     (e.g. "data" or "omnio46" parses as a KindCSSync host, not a
//     relative-path error). host matches [A-Za-z0-9.-]+ (IPv4 literal
//     or DNS name); error only if it doesn't.
//     - no "/bucket" suffix -> KindCSSync, default port 9010
//     - "/bucket" suffix     -> KindRustFS, default port 9000
//
// Consequence (deliberate, not a gap): a BARE relative local path with
// no ./ prefix, e.g. "data/subdir", is syntactically indistinguishable
// from a remote host+bucket spec ("data" as a one-label DNS host,
// "subdir" as the bucket) and is therefore always parsed as the latter,
// never as an error and never guessed as local -- local relative paths
// MUST use the explicit ./ or ../ prefix from rule 3. This is the one
// place this grammar is stricter than it has to be, specifically to
// keep the host[:port][/bucket] branch simple and unambiguous rather
// than trying to heuristically detect "this was probably meant as a
// path".
package endpoint

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Kind int

const (
	KindLocal Kind = iota
	KindCSSync
	KindRustFS
)

func (k Kind) String() string {
	switch k {
	case KindLocal:
		return "local"
	case KindCSSync:
		return "cs-sync"
	case KindRustFS:
		return "rustfs"
	}
	return "?"
}

// Endpoint is a parsed --primary/--secondary/--backup spec.
type Endpoint struct {
	Kind Kind
	Raw  string // original spec, unmodified -- for error messages and state-dir naming

	Path string // KindLocal only, as given (caller is responsible for filepath.Abs)

	Host   string // KindCSSync/KindRustFS
	Port   int    // KindCSSync/KindRustFS -- default filled in if omitted
	Bucket string // KindRustFS only
}

// IsLocal reports whether a KindCSSync/KindRustFS endpoint's Host refers
// to the local machine. Only "localhost"/"127.0.0.1" are recognized --
// deliberately narrow, same as the v2.1 rustfs.Endpoint.IsLocal it
// replaces (member-name aliases from _cfg/group/ are a possible future
// extension, not implemented here). Always false for KindLocal (a local
// path's "locality" is meaningless/inapplicable, not true).
func (e Endpoint) IsLocal() bool {
	return e.Kind != KindLocal && (e.Host == "localhost" || e.Host == "127.0.0.1")
}

// HostPort renders "host:port" (port always explicit, defaults already
// resolved by Parse) -- the form used for e.g. wire.Dial or as an rclone
// endpoint URL host component.
func (e Endpoint) HostPort() string {
	return fmt.Sprintf("%s:%d", e.Host, e.Port)
}

const (
	defaultCSSyncPort = 9010
	defaultRustFSPort = 9000
)

var (
	winPathRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)
	relPathRe = regexp.MustCompile(`^\.\.?[/\\]`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)
)

// Parse parses one endpoint spec per the package doc grammar.
func Parse(spec string) (Endpoint, error) {
	raw := spec
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Endpoint{}, fmt.Errorf("empty endpoint")
	}

	if winPathRe.MatchString(spec) || strings.HasPrefix(spec, "/") || relPathRe.MatchString(spec) {
		return Endpoint{Kind: KindLocal, Raw: raw, Path: spec}, nil
	}

	// Not a recognized local-path form -- must be host[:port][/bucket].
	// Split off an optional "/bucket" suffix first (bucket names never
	// contain "/", so the FIRST "/" is the split point).
	hostport := spec
	bucket := ""
	isRustFS := false
	if i := strings.Index(spec, "/"); i >= 0 {
		hostport = spec[:i]
		bucket = spec[i+1:]
		isRustFS = true
		if bucket == "" {
			return Endpoint{}, fmt.Errorf("invalid endpoint %q: \"/\" present but bucket name is empty", raw)
		}
		if strings.Contains(bucket, "/") {
			return Endpoint{}, fmt.Errorf("invalid endpoint %q: bucket name must not contain \"/\"", raw)
		}
	}

	host := hostport
	port := 0
	if i := strings.LastIndex(hostport, ":"); i >= 0 {
		host = hostport[:i]
		portStr := hostport[i+1:]
		p, err := strconv.Atoi(portStr)
		if err != nil || p <= 0 || p > 65535 {
			return Endpoint{}, fmt.Errorf("invalid endpoint %q: bad port %q", raw, portStr)
		}
		port = p
	}

	if host == "" || !hostRe.MatchString(host) {
		return Endpoint{}, fmt.Errorf(
			"invalid endpoint %q: not a recognized local path (must be absolute, start with ./ or ../, "+
				"or be a Windows drive path) and not a valid host[:port][/bucket] "+
				"(host must be an IPv4 literal or DNS name: letters, digits, dots, hyphens only)", raw)
	}

	if isRustFS {
		if port == 0 {
			port = defaultRustFSPort
		}
		return Endpoint{Kind: KindRustFS, Raw: raw, Host: host, Port: port, Bucket: bucket}, nil
	}
	if port == 0 {
		port = defaultCSSyncPort
	}
	return Endpoint{Kind: KindCSSync, Raw: raw, Host: host, Port: port}, nil
}
