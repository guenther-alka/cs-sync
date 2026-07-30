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

**v2.1.2.** Local `primary ↔ secondary` (v1 engine, unchanged) and
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

**v2.1.2** (2026-07-28) -- full source audit: 4 real bugs fixed.

A systematic manual read of all 34 Go files (not triggered by a live-test
failure this time) found and fixed four issues:

1. **zfscheck.go**: the documented "Windows always nfs4, regardless of
   filesystem" policy was only enforced on the no-dataset-found and
   GetProp-error paths. If `zfs get acltype` on a real OpenZFS-on-Windows
   dataset *succeeded* and returned `posixacl` (the same compat-label
   pattern already confirmed live for FreeBSD in this file) or `off`,
   `CheckAndPrepare` would return `"posix"` or a hard error for a Windows
   host -- and Windows has no real posix ACL implementation (hard-error
   stub), so ACL sync would silently and completely break. Fixed: Windows
   now skips the live acltype read entirely, like illumos/solaris/freebsd.
2. **`watch_eventport_illumossolaris.go`**: the illumos/Solaris Event-Ports
   watcher had **no debounce timer at all**, unlike every fsnotify-based
   watcher -- every single fired FEN association triggered an immediate
   full three-way reconcile pass. A burst of filesystem activity on
   illumos (this project's flagship platform) caused far more full-tree
   rescans than intended. Fixed: added a debounce loop identical in
   structure to the fsnotify watcher's. Live-verified on OmniOS: a 30-file
   burst now settles into a single pass ("30 ops") instead of ~30 separate
   passes.
3. **watch package (both watchers)**: `emit()`'s coalescing could silently
   drop a `safety-net`/`sighup` reason in favor of an already-queued
   `event` -- the periodic existing-folder ACL re-sync is gated on that
   exact reason string, so an unlucky timing coincidence could skip it for
   a full `--rescan` cycle (default 24h). Fixed: `emit()` now drains and
   compares before dropping, never letting `event` downgrade a pending
   `safety-net`/`sighup`/`start`. Verified with a concurrency + race-
   detector test (200 concurrent goroutines).
4. **`remote/receiver.go` `writeAclCSV`**: used a fixed temp filename --
   the same bug class fixed for the local state files in v2.1.1, but here
   on the receiver (`cs-sync serve`) side, which has no relationship to
   the sender-side pass lock. PID-suffixed for consistency.

Also clarified (no functional change) a misleading "bootstrap-only"
comment in `apply/executor.go`'s `mkdirWithACL` -- the routine
secondary→primary mkdir case is harmless: `aclinherit=passthrough` (set
on every dataset) makes ZFS itself apply the parent's inheritable ACEs at
`mkdir()` time, at the kernel level, independent of cs-sync's own ACL
apply call.

Verified: `go build`/`go vet`/`staticcheck` all clean; cross-compiled
clean for all 8 release platforms; live regression-tested on all 4
reachable members (see **OS test results** below).

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

### OS test results (v2.1.2)

Live-tested across the full napp-it CS cluster platform matrix. All
platforms confirmed on **v2.1.2** via `cs-sync version`.

| Host | OS | Regression test after v2.1.2 fixes |
|---|---|---|
| my-w11 | Windows | version confirmed; local dev/test only |
| .112 | Linux (Proxmox, posixacl) | clean, no errors |
| .191 | FreeBSD (nfsv4) | clean, no errors |
| .196 | macOS (no ZFS, acltype=none) | clean, no errors |
| **.189** | **OmniOS/illumos (nfs4)** | clean; **debounce fix confirmed**: 30-file burst settled into 1 reconcile pass ("30 ops") instead of ~30 separate passes |

Known gaps (unchanged from v2.0/v2.1.1):

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

## Behavior on slow or unstable connections

Handling disconnects and slow links is a core design goal, not an
afterthought -- local-only 1.x never had to deal with this at all. See
`cs-sync-2.0-design.info` section 3 for the full rationale.

- **Disconnects (flaky WLAN, link flaps) are normal operation, not
  errors.** No timeout, no alarm, just a log line. Every change that
  can't reach the remote right now goes into a **persisted pending
  queue** that survives both process restart and host reboot.
- **Coalesced per path.** A file that changes 100 times while offline is
  transferred once, latest state only -- the queue stores "this path
  needs sync," not an event log.
- **Reconnect uses exponential backoff** (1s -> 2s -> ... capped at
  5min) before a retry counts against a file's failure count, so a
  flapping link can't burn through the retry budget in seconds.
- **Backpressure, not blocking.** The filesystem watcher never blocks on
  the network -- it can produce changes faster than a slow link can
  carry them indefinitely; the sender just drains the queue at whatever
  speed the link allows.
- **Queue depth is unbounded by design.** A deep queue just means "slow
  link + lots of data," which has to keep working. The retry limit
  (10 attempts, see backoff above) applies only to individual files that
  keep failing for their own reasons (permission errors, unreadable
  source, etc.) -- such a file is quarantined and logged so it stops
  blocking the rest of the queue, everything else keeps syncing.
- **Atomic writes + end-to-end hash.** Every transfer goes to a temp
  file, is hash-verified, then renamed into place -- a drop mid-transfer
  never leaves a half-written file that size/mtime could later mistake
  for current.
- **Torn-copy detection.** If the source changes while a slow transfer
  is still reading it, a before/after size+mtime mismatch discards the
  copy and re-queues it -- no locking needed.
- `--bwlimit` throttles a remote leg so an initial full sync over a
  narrow/shared link doesn't saturate it for everything else using the
  same connection.

Not yet implemented: delta transfer for large partially-changed files
(a changed file is always sent in full -- see **Known gaps** above) and
a queue disk-space warning threshold for very long outages.

## Behavior on very large folder/file counts (local -> remote)

- **Initial scan** builds one in-RAM tree (a map, one entry per file/
  dir/symlink -- path, size, mtime, mode, dev+ino). Cost is linear in
  entry count and kept entirely in RAM, no paging/streaming -- fine for
  hundreds of thousands of entries, real RAM to plan for at many
  millions. A single unreadable file/dir does not abort the scan
  (`SkipDir` on that subtree, everything else continues).
- **Watching**: a native OS watch handle is registered per directory
  (inotify/kqueue/FEN/`ReadDirectoryChangesW`). `--max-watched-dirs`
  (default `0` = unlimited) exists specifically for FreeBSD, where
  kqueue costs one fd per watched directory -- past the configured
  count, the watcher **automatically falls back to periodic polling**
  instead of per-directory handles (suggested value there: `50000`).
- **Transfer is sequential, one connection, one file at a time** --
  a deliberate KISS decision (queue-level parallelism across several
  files was considered, not implemented). Throughput on a huge tree
  over the network is per-file overhead x file count, not parallelized
  across multiple streams.
- **Renames stay cheap at any scale**: a renamed folder with millions
  of bytes underneath is a single metadata operation (inode-based
  rename detection), never a re-transfer of its contents -- the
  biggest practical win for large trees that move around.
- **Pending queue** is unbounded and coalesced per path by design -- a
  huge initial backlog (millions of files over a slow/narrow remote
  link) is the expected case, not a failure mode; it drains over time.
- **Delete-budget guard** (`--max-delete-count` default `1000`,
  `--max-delete-percent` default `20%`, OR-combined) can misfire on a
  genuinely huge tree: a routine bulk cleanup well under 20% of the
  tree can still trip the absolute count threshold -- raise
  `--max-delete-count` explicitly for large datasets rather than
  relying on the default.
- **24h safety-net rescan** now also does checksum verification (see
  ACL bootstrap / identity-check section above) -- on a very large tree
  this means reading every file's content once per rescan cycle, I/O
  load that scales with total data size, not just file count.

## Data versioning

cs-sync itself keeps only the current state on the target -- it is a
realtime mirror, not a version history. A file overwritten or deleted on
`primary` is overwritten or deleted on `backup`/`secondary` too, by
design (that's what "realtime" means).

For point-in-time recovery (accidental deletes, ransomware, a bad
overwrite propagated before anyone notices), run a ZFS **autosnap job**
on the backup server's dataset as a complement to realtime sync -- not
a replacement for it, an independent safety net underneath it. A
typical schedule:

- snapshot every hour, keep 12
- snapshot every day, keep 60
- snapshot every week/month, keep as needed

Realtime sync keeps `backup` current to the minute; the autosnap job on
top makes that current state recoverable to any retained point in the
past, at essentially no extra cost (ZFS snapshots are copy-on-write,
near-instant, and only consume space for the blocks that later change).

## Disaster recovery

Restoring `fileserver` from `backup` (e.g. after ransomware on the
primary) is a one-shot **filesync back**, not a reversed cs-sync job --
remote relationships are unidirectional by design (see **Behavior on
slow or unstable connections** / cycle detection above), so running a
second cs-sync instance the other way would itself be the loop pattern
cs-sync's config-time cycle detection exists to reject. Use the
existing filesync job type (cs-stream transport, ACL-aware) for the
one-time restore copy instead.

Typical flow:

1. **Stop the forward sync job** (fileserver -> backup) so nothing more
   propagates from a possibly still-compromised fileserver, and so
   `backup` isn't mutated while you restore from it.
2. **If backup itself may be compromised** (the sync job was still
   running when the incident happened, so the corruption/encryption may
   already be on `backup` too): do **not** sync back from `backup`'s
   live head, and do **not** use `zfs rollback` as the first step --
   rollback is destructive (discards everything after the chosen
   snapshot, including the ability to try an earlier one if you picked
   wrong). Instead:
   - `zfs clone` the last known-good autosnap (see **Data versioning**
     above) into a temporary dataset -- instant, non-destructive.
   - Verify the data in the clone (spot-check files/hashes for absence
     of corruption/encryption markers).
   - If verification fails, clone an earlier snapshot and repeat.
   - Only once a clean point is confirmed, filesync from that verified
     clone to the fileserver. A true `zfs rollback` (or `zfs promote`
     of the clone) is optional cleanup *after* verification, not part
     of the recovery path itself.
3. **Filesync back** (verified backup snapshot/clone -> fileserver) via
   the cs-stream filesync job.
4. **Reset cs-sync's persisted state** on the fileserver side (delete
   `.backupdata/`: baseline + `acl.csv`) before restarting the forward
   job, so the first reconcile pass does a fresh bootstrap against the
   just-restored tree instead of comparing against a stale pre-incident
   baseline.
5. **Restart the forward sync job.** Fileserver and backup should now be
   identical, so the first pass should settle cleanly with no large
   diff and no risk of tripping the mass-delete guard.

## Gewährleistung

napp-it cs-sync/stream ist OpenSource. Sie dürfen es kostenlos nutzen,
analysieren oder verändern. Sie nutzen es "as is" und tragen die
alleinige Verantwortung für die Nutzung. Diese Hinweise ersetzen nicht
die BSD 2-Clause Lizenzbedingungen unten, sondern fassen sie in
verständlicher Form zusammen.

## License

BSD 2-Clause License -- Copyright (c) 2026 Guenther Alka / napp-it.org.
See [LICENSE](LICENSE) for full terms.
