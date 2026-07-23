# cs-sync

Realtime bidirectional folder sync service for ZFS hosts, built for
[napp-it CS](https://napp-it.org) (csweb-gui). Same platform matrix and
build model as [cs-stream](https://github.com/guenther-alka/cs-stream).

Typical use case: keep a local `primary_smb` folder (SMB share, users work
here) and a local `secondary_s3` folder (RustFS S3 object storage export)
in sync, with RustFS bucket replication carrying `secondary_s3` on to a
remote site for disaster recovery.

Full concept and design decisions:
`/opt/csweb-gui/data/howto.ai/cs-sync.info`

## Status

v1 implementation. Core sync engine (three-way merge, rename detection,
conflict handling, folder-ACL sync, crash-safe copy) is implemented and
unit/manually tested. Known v1 gaps (see cs-sync.info section 14 and code
comments):

- Echo suppression (own writes triggering extra reconcile passes) is not
  implemented -- harmless (idempotent, converges to "no changes") but not
  as efficient as the concept doc describes.
- FreeBSD `nfs4_setfacl` "replace whole ACL" flag needs verification
  against the target release's man page.
- Menu integration (`action.pl` under
  `data/menues/03_System/02_Services/25_Realtime_Sync/`)

illumos/Solaris use native Event Ports (`port_create`/`port_associate`/
`port_get` via `golang.org/x/sys/unix`, no cgo) for real event-driven
change detection -- not polling. FEN (File Events Notification)
associations fire once and are re-armed after each event; see
`internal/watch/watch_eventport_illumossolaris.go`.

## Build

```
go build -o cs-sync .
```

Cross-compile (see `.github/workflows/release.yml` for the full matrix):

```
GOOS=linux   GOARCH=amd64 go build -o cs-sync-linux-amd64 .
GOOS=windows GOARCH=amd64 go build -o cs-sync-windows-amd64.exe .
GOOS=illumos GOARCH=amd64 go build -o cs-sync-illumos-amd64 .
GOOS=freebsd GOARCH=amd64 go build -o cs-sync-freebsd-amd64 .
```

GitHub Actions builds all platforms automatically on tag push (`v*`).

## Usage

```
cs-sync run  --primary <path> --secondary <path> [--mode bidir|oneway]
cs-sync scan --primary <path> --secondary <path>   # dry-run report
cs-sync version
```

`--mode oneway`: `--primary` is always the source, `--secondary` is the
mirror (overwritten/deleted to match). For the reverse DR leg, swap
which folder you pass as `--primary`/`--secondary`.

ACL bootstrap (no CLI flag): on every `run` startup, cs-sync auto-detects
which `acl.csv` (if any) is authoritative -- primary's own copy, else
secondary's (recovery case), else none (fresh live scan) -- and restores/
propagates it once before the normal reconcile loop starts. See
cs-sync.info section 10.

See cs-sync.info for the full CLI option list, ACL preconditions (ZFS
acltype must be posix or nfs4, not off), and the site-replication
topology this is designed to slot into.

## License

BSD 2-Clause License -- Copyright (c) 2026 Guenther Alka / napp-it.org.
See [LICENSE](LICENSE) for full terms.
