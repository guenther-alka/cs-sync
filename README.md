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
  <ip:port> --key <key> --allow-ip <ip>` -- it listens.
- The **primary** runs `cs-sync run --primary <path> --remote
  <ip:port> --key <key>` -- it dials out and pushes.

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

cs-sync **encrypts natively as of v2.1** (ChaCha20-Poly1305, keyed from
`--key` -- the same key must be given to both `run --remote` and
`serve`). `--key` and, on the receiver, `--allow-ip` are both
**mandatory**: an empty key or a missing source-IP allowlist would let
an arbitrary host on the LAN attempt a sync. No separate
[cs-stream](https://github.com/guenther-alka/cs-stream) tunnel is
required for encryption anymore (cs-stream remains useful for other
transports, but is no longer needed to secure cs-sync's own wire
traffic).

Full concept and design decisions:
`/opt/csweb-gui/data/menues/03_System/02_Services/25_Realtime_Sync/cs-sync-2.0-design.info`
(v1 groundwork: `cs-sync.info` in the same folder)

## Status

**v2.1.1.** Local `primary ↔ secondary` (v1 engine, unchanged) and
`primary → backup` (local one-way, and remote over the network, natively
encrypted) are both implemented and live-deployed to the official
napp-it CS menu path
(`data/cs_server/tools/cs-sync/<os>.amd64/`) on all reachable cluster
members. Remote transfer is chunk-framed, per-block + whole-file
(sha256) hash verified before the atomic rename, torn-copy protected,
and covered by a mass-delete guard (aborts + freezes the relationship if
a pass would delete an unexpectedly large share of the known tree --
e.g. an unmounted source dataset masquerading as an empty folder). See
`cs-sync-2.0-design.info` sections 3-6 for the full rationale.

### Changelog

**v2.1.1** (2026-07-28) -- illumos cross-process race condition fix.

Two `cs-sync run` processes sharing the same `--primary` path -- the
`bidir` process plus a local-path `Backup` `oneway` process, exactly the
pattern napp-it CS's Realtime Sync menu spawns for that configuration --
both called `state.Save()` / `state.WriteACLCSV()`, which wrote to a
**fixed** temp filename (`cs-sync.state.tmp`, `cs-sync-acl.csv.tmp`) with
no coordination between processes. A concurrent `Save()` from one
process could interleave with or get overwritten by the other's,
silently corrupting the persisted baseline -- **no I/O error at all**,
since the corruption is purely logical (wrong content on disk, not a
failed write), so nothing in the existing error handling ever caught
it. Live-reproduced on OmniOS/illumos as complete silent data loss (a
test file vanished from primary, secondary, *and* backup, zero
WARN/ERROR in either process's log). Not illumos-specific in cause --
purely timing-dependent; illumos happened to hit the window first in a
small test sample.

Fix: new `internal/lock` package -- a simple advisory file lock
(`O_CREATE|O_EXCL` on `<primary>/.backupdata/cs-sync.lock`, no cgo, no
per-OS syscall, atomic on every target filesystem: ZFS/illumos,
ZFS/FreeBSD, ext4, APFS, NTFS/ReFS). `main.go`'s `doPass()` now holds
this lock for the **entire** load→scan→reconcile→apply→save critical
section of a pass, not just around the individual `Load`/`Save` calls
(an early draft only locked those two functions, which left the
scan+reconcile window between them unprotected -- caught during
implementation review). Stale-lock recovery: a lock older than 30s is
assumed to be from a crashed process and is stolen rather than blocking
forever. Belt-and-suspenders: temp filenames are now PID-suffixed
(`cs-sync.state.tmp.<pid>`) so even a caller that somehow bypassed the
lock can't collide with a concurrent process on the exact same temp
file.

Verified: `go build`/`go vet` clean; a standalone concurrency test (4
goroutines racing `lock.Acquire` on the same directory, 20 iterations
each) confirmed max-concurrent-holders=1 across all 80 acquisitions,
zero violations; a stale-lock test confirmed microsecond recovery from
a simulated crashed process rather than waiting out the 10s timeout.
The original OmniOS failure scenario was then re-run live and passed
clean -- see **OS test results** below.

**v2.1.0** (2026-07-27) -- native encryption, mandatory `--allow-ip`,
service status, cross-host loop detection. `--key` now also natively
encrypts the wire transfer (ChaCha20-Poly1305) instead of relying solely
on an external cs-stream tunnel; `serve` requires `--allow-ip`
(single-source-IP allowlist); `--service-id` writes status/loop-marker
files consumable by the napp-it CS Jobs table; oneway/remote
relationships detect sync-chain loops (a service's own marker returning
to its primary via a -> b -> ... -> a) and stop with a clear error
instead of looping forever.

### OS test results (v2.1.1)

Live-tested across the full napp-it CS cluster platform matrix. All
platforms confirmed on **v2.1.1** via `cs-sync version`.

| Host | OS | Solo (single relationship) | Parallel bidir + local backup (shared `--primary`) |
|---|---|---|---|
| my-w11 | Windows | clean | not applicable (single-host dev/test only) |
| .112 | Linux (Proxmox) | clean | clean (prior v2.1.0 round) |
| .191 | FreeBSD | clean | clean (prior v2.1.0 round) |
| .196 | macOS | clean | clean (prior v2.1.0 round) |
| **.189** | **OmniOS/illumos** | clean | **clean -- was the original failure case, now fixed** |

The OmniOS re-test is the one that matters: same parallel `bidir` +
local-path `oneway` "Backup" pattern that previously produced complete
silent data loss now produces byte-for-byte consistent results across
primary/secondary/backup on every pass, confirmed both immediately after
a burst of writes and after a full 10-second run, with zero
WARN/ERROR/FATAL log lines.

Known gaps (unchanged from v2.0/v2.1.0):

- Echo suppression (own writes triggering extra reconcile passes) is
  not implemented -- harmless (idempotent, converges to "no changes")
  but not as efficient as the design doc describes.
- FreeBSD `nfs4_setfacl` "replace whole ACL" flag needs verification
  against the target release's man page.
- Delta transfer for large partially-changed files (CDC via
  `restic/chunker`) is deferred by design -- content is always sent in
  full over the wire.
- Two-way sync is permanently local-only by design (see table above) --
  there is no bidirectional remote mode planned.

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

```
cs-sync run  --primary <path> [--secondary <path>] [--remote host:port] [options]
cs-sync scan --primary <path> --secondary <path> [options]   (dry-run report)
cs-sync serve --dest <path> --listen 127.0.0.1:9010          (2.0+ receiver)
cs-sync version
```

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
`--secondary` -- a second `run` process alongside any `primary ↔
secondary` pair on the same primary (this is exactly the "Backup =
local path" option in napp-it CS's Realtime Sync menu, and the scenario
the v2.1.1 lock fix addresses -- both processes safely share one
`--primary` now).

### primary → backup, remote

On the backup target:

```
cs-sync serve --dest <path> --listen 127.0.0.1:9010 \
  --key <transfer-key> --allow-ip <sender-ip>
```

On the primary:

```
cs-sync run --primary <path> --remote <ip:port> --key <transfer-key> \
  [--remote-name <id>] [--bwlimit <bytes/s>] \
  [--max-delete-count 1000] [--max-delete-percent 20] \
  [--service-id <id>]
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

### All options

**`run`** (primary/secondary/remote, `apply=true`; `scan` is the same
flag set with `apply=false`, dry-run only):

| Flag | Default | Meaning |
|---|---|---|
| `--primary` | *(required)* | primary folder |
| `--secondary` | | secondary/backup folder (local pair or local backup) |
| `--mode` | `bidir` | `bidir` \| `oneway` |
| `--debounce` | `500ms` | event debounce window |
| `--rescan` | `24h` | safety-net full rescan interval |
| `--max-watched-dirs` | `0` | 0=unlimited; FreeBSD suggested 50000 |
| `--log` | `<primary>/.backupdata/cs-sync.log` | log file path |
| `--remote` | | 2.0+: receiver `host:port` (one-way primary -> remote) |
| `--remote-name` | | 2.0+: per-pair state dir name (default: sanitized address) |
| `--bwlimit` | `0` | 2.0+: remote rate limit bytes/s, 0=unlimited |
| `--max-delete-count` | `1000` | delete-budget guard, 0=off |
| `--max-delete-percent` | `20` | delete-budget guard, 0=off |
| `--key` | | **required if `--remote` is set**: pre-shared transfer/encryption key |
| `--service-id` | | status (`_cfg/sync/<id>.last`) + loop-detection (oneway/remote) |

**`serve`** (2.0+ receiver):

| Flag | Default | Meaning |
|---|---|---|
| `--dest` | *(required)* | destination folder on ZFS |
| `--listen` | `127.0.0.1:9010` | listen address |
| `--log` | `<dest>/.backupdata/cs-sync.log` | log file path |
| `--key` | *(required)* | pre-shared transfer/encryption key -- must match sender's `--key` |
| `--allow-ip` | *(required)* | only this single source IP may connect |
| `--service-id` | | status/loop-detection files |

`--key` and `--allow-ip` refusing to default to empty is intentional
(v2.1.0): an unauthenticated or unrestricted listener would let any host
on the LAN attempt a sync.

ACL bootstrap (no CLI flag): on every `run` startup, cs-sync auto-detects
which `acl.csv` (if any) is authoritative -- primary's own copy, else
secondary's (recovery case), else none (fresh live scan) -- and restores/
propagates it once before the normal reconcile loop starts. Over a
remote leg, `acl.csv` is always pushed to the backup target; native
folder ACLs are additionally applied when both ends auto-detect the same
OS and ACL type (no flag -- negotiated in the connection handshake). See
`cs-sync-2.0-design.info` sections 8 and 14.

See `cs-sync-2.0-design.info` for ACL preconditions (ZFS acltype must be
posix or nfs4, not off), the wire protocol, and the site-replication
topology this is designed to slot into.

## License

BSD 2-Clause License -- Copyright (c) 2026 Guenther Alka / napp-it.org.
See [LICENSE](LICENSE) for full terms.
