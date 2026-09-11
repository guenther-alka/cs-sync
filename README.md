# cs-sync

Realtime bidirectional/unidirectional folder sync service with folder
ACL sync, for ZFS hosts. Part of [napp-it CS](https://napp-it.org)
(csweb-gui), also usable completely standalone.

**Tri-endpoint.** Every relationship connects two of three endpoint
kinds — a local filesystem folder, another cs-sync service (wire
protocol, natively encrypted), or a RustFS/S3 bucket — through one
unified syntax used identically for every role:

```
local path      D:\data, /tank/data, ./data, ../data
cs-sync host    host[:port]           (default port 9010)
RustFS bucket   host[:port]/bucket    (default port 9000)
```

One string tells cs-sync everything it needs: whether to watch a
folder, dial out to another cs-sync process, or drive an S3 bucket via
`rclone` — no separate flags to say which.

> **v3.0 is a breaking rewrite.** There is no upgrade path from 2.x —
> every flag changed meaning. Recreate any existing service rather
> than trying to migrate its configuration.

---

## The three modes

Always left to right: **Primary → Secondary → Backup**.

| Mode | Primary | Secondary | Backup |
|---|---|---|---|
| `sync` | local path (required) | local path, or a **local** RustFS bucket (bidirectional) | optional, any endpoint (extra one-way leg) |
| `push` | local path (required, the source) | — | required, any endpoint (the target) |
| `pull` | required, any endpoint (the source) | — | required, local path (the destination) |

```
sync:  Primary(local)  <====>  Secondary(local)   [====> Backup(any)]
push:  Primary(local)  ======================================> Backup(any)
pull:  Primary(any)    ======================================> Backup(local)
```

### `sync` — bidirectional

```
cs-sync run --mode sync --primary /tank/share --secondary /tank/mirror
```

Real-time bidirectional folder sync with folder ACL propagation
(NFSv4 ACLs on illumos/FreeBSD, POSIX ACLs on Linux). Both sides must
be local paths on the same host — bidirectional sync needs real-time
change detection on both sides, which only a local filesystem watch
gives you.

**Secondary can also be a local RustFS/S3 bucket** — the essential
SMB↔S3 case:

```
cs-sync run --mode sync \
  --primary /tank/smbshare \
  --secondary localhost:9000/s3share \
  --key s3admin:yourSecretKey
```

`localhost`/`127.0.0.1` only — a remote bucket can't participate in
true bidirectional sync (see **Endpoints and auth** below for why).
Under the hood cs-sync drives `rclone` against the bucket
(`bisync --resync` for the initial baseline, then per-object
copy/delete for ongoing changes) — it never writes into the bucket's
storage path directly.

Add an optional unidirectional **Backup** leg on top — a third copy,
pushed one-way from Primary, to any endpoint kind:

```
cs-sync run --mode sync --primary /tank/share --secondary /tank/mirror \
  --backup 192.168.1.50:9010 --key mySharedKey
```

### `push` — one-way, you are the source

```
cs-sync run --mode push --primary /tank/share --backup 192.168.1.50:9010 --key mySharedKey
cs-sync run --mode push --primary /tank/share --backup 192.168.1.50/bucket --key access:secret
cs-sync run --mode push --primary /tank/share --backup /mnt/otherdisk
```

Backup can be a local path (a plain one-way mirror on the same host),
another cs-sync host (needs a matching `serve` listener there — see
below), or a RustFS/S3 bucket, local or remote. You never need to know
anything about the receiving side's own folder layout — only its
address and key.

### `pull` — one-way, you are the destination

```
cs-sync run --mode pull --primary 192.168.1.10/bucket --backup /tank/restore --key access:secret
cs-sync run --mode pull --primary /tank/source --backup /tank/mirror
```

Primary is the source (a RustFS bucket, local or remote, or a second
local path), Backup is always local — the destination. Pulling
directly from a *live cs-sync host* isn't supported (see **Known
limitations**); pull a RustFS bucket or another local folder instead.

---

## `serve` — the receiving end of a push

Anything on the *other* side of a `push`, or a `pull` sourced from a
live cs-sync host, needs its own listener:

```
cs-sync serve --dest /tank/incoming --listen 0.0.0.0:9010 \
  --key mySharedKey --allow_ip 192.168.1.20
```

`--key` and `--allow_ip` are both mandatory — an empty key or a
missing single-IP allowlist would let an arbitrary host on the network
attempt a sync. `serve` only ever *accepts* pushed data; the wire
protocol has no reverse "pull" frame (see **Known limitations**).

---

## Endpoints and auth

**`--key` / `--primary_key` / `--secondary_key` / `--backup_key`.**
One shared default (`--key`), with per-leg overrides. Interpreted per
that leg's endpoint kind:

- **cs-sync endpoint** → a pre-shared transfer/encryption key. The
  wire protocol is natively encrypted (ChaCha20-Poly1305) — this key
  both authenticates and encrypts, matching `serve`'s own `--key`.
- **RustFS endpoint** → `access:secret` (split on the first `:`).
  cs-sync auto-creates/updates the underlying named `rclone` remote
  itself, non-interactively, from these credentials — **no manual
  `rclone config` step needed**, ever.

**`--allow_ip`.** Restricts *every* incoming listener a process opens
— `serve`'s receiver, and `run`'s optional `--webhook-listen` — to a
single source IP.

**Why a remote RustFS bucket can't be `sync`'s Secondary.**
Bidirectional sync needs a real-time, symmetric way to detect changes
on *both* sides to resolve conflicts correctly. A local bucket gets
that indirectly (primary's own filesystem watch triggers a fresh scan
of both sides on every pass); a remote bucket has no such local
watch — only a webhook or periodic rescan, both far slower and
asymmetric. `pull`/`push` exist precisely for the one-way,
remote-source-of-truth case a remote bucket actually fits.

---

## Install — pre-built binaries

Prebuilt binaries for every platform are attached to each
[GitHub release](https://github.com/guenther-alka/cs-sync/releases)
(`cs-sync-<os>.<arch>.tar.gz`, built by `.github/workflows/release.yml`
on every `v*` tag: `mswin.amd64`, `linux.amd64`, `linux.arm64`,
`illumos.amd64`, `solaris.amd64`, `freebsd.amd64`, `darwin.amd64`,
`darwin.arm64`).

### Using cs-sync from napp-it CS (csweb-gui)
csweb-gui deploys and updates this manually per member menu About > Download cs-tools

napp-it CS's own Realtime Sync menu (`03_System > 02_Services > 25_Realtime_Sync`)
expects the binary at a fixed path per platform:

```
data/cs_server/tools/cs-sync/<osdir>.<arch>/cs-sync[.exe]
```

where `<osdir>` is `mswin`, `linux`, `illumos`, `solaris`, `freebsd`,
or `darwin` (matching the release archive names above, minus the
`.tar.gz`/`.exe` distinction).

**On the frontend host (Windows, `C:\opt\csweb-gui`):**

1. Download `cs-sync-mswin.amd64.tar.gz` from the latest release.
2. Extract `cs-sync.exe` from it.
3. Place it at:
   ```
   C:\opt\csweb-gui\data\cs_server\tools\cs-sync\mswin.amd64\cs-sync.exe
   ```
4. The Realtime Sync menu picks it up automatically — no restart
   needed for new services created after this point.

For a member running a different OS, extract the matching archive and
place `cs-sync` (no `.exe`) at the equivalent
`data/cs_server/tools/cs-sync/<osdir>.<arch>/cs-sync` path on that
member instead.

### Standalone (no napp-it CS)

Extract the archive for your platform, put `cs-sync`/`cs-sync.exe`
wherever you like, and run it directly per the examples above.
`rclone` must additionally be installed and on `PATH` for any RustFS
endpoint (`sync`'s local-bucket Secondary, or any `push`/`pull` leg
targeting a bucket) — cs-sync shells out to it, it doesn't vendor an
S3 client itself.

---

## Build from source

Requires Go 1.25+ (the toolchain auto-upgrades itself if an older Go
is present and `go.mod`'s `toolchain` directive requests a newer one).

```
git clone https://github.com/guenther-alka/cs-sync.git
cd cs-sync
go build -o cs-sync .                     # current platform
go build -o cs-sync.exe -ldflags "-X main.version=dev" .   # embed a version string
```

Cross-compile for another platform by setting `GOOS`/`GOARCH`:

```
GOOS=illumos GOARCH=amd64 go build -o cs-sync .
GOOS=windows GOARCH=amd64 go build -o cs-sync.exe .
```

(On Windows/PowerShell, set them as separate `$env:GOOS`/`$env:GOARCH`
assignments before the build line, not inline — PowerShell doesn't
support the `VAR=value command` shell syntax.)

---

## Tests

```
go build ./...      # compiles cleanly, all packages
go vet ./...         # static analysis, no findings
go test ./...         # unit tests (internal/endpoint, internal/rustfs, internal/remote)
```

Unit tests cover the endpoint parser (`internal/endpoint`) — every
grammar case including the documented edge cases (Windows drive paths
vs. `host:port`, bare-relative-path handling) — the RustFS package
(`internal/rustfs`) — webhook event parsing, `--allow_ip` filtering,
xl.meta parsing — and the remote receiver (`internal/remote`) — that
incoming wire paths are refused when they would escape `--dest`.

`-race` requires cgo (a C toolchain); skip it on a host without one —
`go build`/`go vet`/`go test` alone already catch the vast majority of
real issues, and the concurrency-sensitive code paths (the pass
lock, the debounced watcher, the retry/quarantine queue) are covered
by ordinary tests too, just without the race detector attached.

There is no bundled integration-test suite against a real RustFS
instance or a second cs-sync host — those require live infrastructure
and were exercised manually during development (see commit history and
`sync-3.0-design.info` in a napp-it CS checkout for what was verified
and how).

---

## Known limitations

- **Pulling from a live cs-sync host isn't supported directly.** The
  wire protocol only has a "push into a receiver" frame, not a "send
  me your tree" one — `run --mode pull` refuses a cs-sync-kind Primary
  with a clear error rather than doing nothing silently. Set up a
  `serve` listener on the receiving side and a `push` on the sending
  side instead (napp-it CS's own Realtime Sync menu does exactly this
  automatically when it detects the combination).
- **`sync` mode's Backup leg can't be a local path.** A third local
  mirror alongside a bidirectional pair isn't implemented — run a
  second, independent `push` relationship for that instead.
- **No ACL propagation to/from a RustFS-typed endpoint.** S3 has no
  filesystem-ACL concept. Folders created on the filesystem side still
  inherit ACLs normally from their parent (`aclinherit=passthrough`);
  nothing analogous applies to the bucket side.
- **No recursive delete for non-empty "folders" on a RustFS side** — a
  documented no-op. RustFS folders are a naming convention over flat
  object keys, not real directories.
- **RustFS with a self-signed TLS certificate:** cs-sync always passes
  `--no-check-certificate` to `rclone` for RustFS endpoints (not
  configurable) — without it, `rclone` hangs indefinitely on the TLS
  handshake rather than failing cleanly, confirmed against a real
  self-signed-cert RustFS instance. This means cs-sync will also
  accept a *genuinely* invalid/expired certificate without complaint;
  it is written for a trusted-LAN deployment model, not a
  hostile-network one.

---

## License

BSD 2-Clause — see [LICENSE](LICENSE).

## Warranty

None. This is infrastructure software that deletes and overwrites
files as part of its normal operation. Read the delete-budget guard
flags (`--max-delete-count`, `--max-delete-percent`), test against
non-production data first, and keep independent backups regardless of
what cs-sync itself reports as successful.
