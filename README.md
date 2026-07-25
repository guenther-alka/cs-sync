# cs-sync

Realtime folder sync service for ZFS hosts, built for
[napp-it CS](https://napp-it.org) (csweb-gui). Same platform matrix and
build model as [cs-stream](https://github.com/guenther-alka/cs-stream).

One binary, one relationship model, two ways to use it:

| Relationship             | Direction                | Scope |
|---------------------------|----------------------------|-------|
| **primary ↔ secondary**   | uni- or **bi**directional  | local only -- both folders on the same host |
| **primary → backup**      | **uni**directional only    | local (same host) **or** remote (another cluster member, over the network) |

`primary`/`secondary` is a *working pair* (e.g. an SMB share kept in
sync with an S3-export folder) and may go either direction. `backup` is
always a one-way archival copy -- deliberately never bidirectional, even
when the target happens to be local, because a backup that could also
receive writes back stops being a backup.

## Remote backup: who listens where

A remote `primary → backup` relationship is two independent one-sided
configs, one per member -- there is no shared config file:

- The **backup target** runs `cs-sync serve --dest <path> --listen
  <ip:port>` -- it listens.
- The **primary** runs `cs-sync run --primary <path> --remote
  <ip:port>` -- it dials out and pushes.

Which member does which is just a question of which command you run
where. In napp-it CS's Cross-Member Sync menu this is phrased as one
choice per member, from that member's own point of view:

- **to ip** -- this member is the primary; pick the remote member to
  back up *to*. This member stays a client (`run --remote`), the far
  member must independently be configured with **from ip** pointing
  back (so it runs `serve` and is actually listening).
- **from ip** -- this member is the backup target; pick the remote
  member to receive *from*. This member listens (`serve`), the far
  member must independently be configured with **to ip** pointing here.

Both halves have to exist for the relationship to actually run --
setting only one side leaves a dangling client with nothing to dial, or
a listener nobody ever connects to. The GUI flags this (see Status
column below) rather than silently doing nothing.

cs-sync itself does **not** encrypt the connection -- `--remote` /
`--listen` are meant to run through a
[cs-stream](https://github.com/guenther-alka/cs-stream) tunnel
(`tunnel-listen` / `tunnel-send`). A non-loopback `--listen` without a
tunnel in front logs a warning at startup.

Full concept and design decisions:
`/opt/csweb-gui/data/menues/03_System/02_Services/25_Realtime_Sync/cs-sync-2.0-design.info`
(v1 groundwork: `cs-sync.info` in the same folder)

## Status

**v2.0.** Local `primary ↔ secondary` (v1 engine, unchanged) and
`primary → backup` (new: local one-way, and remote over the network)
are both implemented. Remote transfer is chunk-framed, per-block +
whole-file (sha256) hash verified before the atomic rename, torn-copy
protected, and covered by a mass-delete guard (aborts + freezes the
relationship if a pass would delete an unexpectedly large share of the
known tree -- e.g. an unmounted source dataset masquerading as an empty
folder). See `cs-sync-2.0-design.info` sections 3-6 for the full
rationale.

Known gaps:

- Echo suppression (own writes triggering extra reconcile passes) is
  not implemented -- harmless (idempotent, converges to "no changes")
  but not as efficient as the design doc describes.
- FreeBSD `nfs4_setfacl` "replace whole ACL" flag needs verification
  against the target release's man page.
- Delta transfer for large partially-changed files (CDC via
  `restic/chunker`) is deferred to v2.1 by design -- v2.0 always sends
  full file content over the wire.
- Two-way sync is permanently local-only by design (see table above) --
  there is no bidirectional remote mode planned.
- Menu integration (`action.pl` under
  `data/menues/03_System/02_Services/25_Realtime_Sync/`) is in
  progress.

illumos/Solaris use native Event Ports (`port_create`/`port_associate`/
`port_get` via `golang.org/x/sys/unix`, no cgo) for real event-driven
change detection -- not polling. FEN (File Events Notification)
associations fire once and are re-armed after each event; see
`internal/watch/watch_eventport_illumossolaris.go`.

## Download

Pre-built binaries for all platforms in [Releases](https://github.com/guenther-alka/cs-sync/releases/latest).

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

### primary ↔ secondary (local pair, uni or bidirectional)

```
cs-sync run  --primary <path> --secondary <path> [--mode bidir|oneway]
cs-sync scan --primary <path> --secondary <path>   # dry-run report
```

`--mode oneway`: `--primary` is always the source, `--secondary` is the
mirror (overwritten/deleted to match). `--mode bidir` (default): either
side may change, three-way merge with rename detection and conflict
handling. For the reverse DR leg of a oneway pair, swap which folder you
pass as `--primary`/`--secondary`.

### primary → backup, local

Same as above with `--mode oneway`, using a third folder as
`--secondary` -- a plain second `run` process (or, once the menu ships,
a second relationship entry) alongside any `primary ↔ secondary` pair on
the same primary.

### primary → backup, remote

On the backup target:

```
cs-sync serve --dest <path> --listen 127.0.0.1:9010   # put a cs-stream tunnel in front for anything beyond loopback
```

On the primary:

```
cs-sync run --primary <path> --remote <ip:port> \
  [--remote-name <id>] [--bwlimit <bytes/s>] \
  [--max-delete-count 1000] [--max-delete-percent 20]
```

`--remote` can be combined with a local `--secondary` in the same `run`
process -- they are independent relationships with independent state
(the local pair keeps its own `.backupdata/`, the remote leg its own
`.backupdata/remote_<name>/`).

`--max-delete-count` / `--max-delete-percent` are the mass-delete guard
thresholds (OR-combined: either one tripping freezes the relationship).
If it trips, `.backupdata/remote_<name>/delete-freeze.txt` explains why
and how to resume: rename it to `delete-approve.txt` to let the *next*
pass through once, budget check skipped for that pass only. Deleting the
freeze file outright does **not** work -- the same pending deletes would
just re-trip the budget on the next pass.

### common

```
cs-sync version
```

ACL bootstrap (no CLI flag): on every `run` startup, cs-sync auto-detects
which `acl.csv` (if any) is authoritative -- primary's own copy, else
secondary's (recovery case), else none (fresh live scan) -- and restores/
propagates it once before the normal reconcile loop starts. Over a
remote leg, `acl.csv` is always pushed to the backup target; native
folder ACLs are additionally applied when both ends auto-detect the same
OS and ACL type (no flag -- negotiated in the connection handshake). See
`cs-sync-2.0-design.info` sections 8 and 14.

See `cs-sync-2.0-design.info` for the full CLI option list, ACL
preconditions (ZFS acltype must be posix or nfs4, not off), the wire
protocol, and the site-replication topology this is designed to slot
into.

## License

BSD 2-Clause License -- Copyright (c) 2026 Guenther Alka / napp-it.org.
See [LICENSE](LICENSE) for full terms.
