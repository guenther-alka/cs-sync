# Changelog

All notable changes to cs-sync are documented here. Versions follow
`v<major>.<minor>.<patch>`; see the git tags for the full history.

## v3.3.0 (2026-10-14)

### Changed

- On a Proxmox source the first snapshot of every day automatically goes through `cs-freeze4snap` (no `--freeze` needed;
  `--freeze hh:mm` remains for other sources such as ESXi). It is kept as `kf<MMDD>` on both sides.
- `cs-freeze4snap` is found next to cs-sync (standalone), in `../../cs-freeze4snap/<os>.<arch>/` (napp-it tools layout) or in PATH;
  if it is missing the freeze is skipped with a warning and the snapshot stays crash-consistent.
- `cs:consistent` is set from the tool result: `app` only if every guest was really frozen (guest agent, container freeze, memory,
  ESXi quiesce), `paused` if a VM had to be paused (crash-consistent); plain snapshots get no mark.

## v3.2.1 (2026-10-14)

### Changed

- `--pve-config` removed: fixed behaviour. When the Primary is a Proxmox host (`/etc/pve` exists) the VM/CT config of the synced
  disks is always stored as `cs:pveconf` (logged at start). Other sources are unaffected.

## v3.2.0 (2026-10-14)

### Added

- **Proxmox VM/CT config as ZFS property** (`--pve-config auto|y|n`, default auto = on when `/etc/pve` exists): the config of every
  VM/CT whose disks (`vm-<id>-disk-N`, `subvol-<id>-disk-N`) are synced is stored as `cs:pveconf` (gzip+base64, chunked in
  `cs:pveconf.N` for the ~8 KB property limit) on the disk datasets and on every sync snapshot, on both sides, independent of the
  backup OS. A config-only change is sent as a small frame without `zfs send`.
- `cs-sync pveconf list|show|restore` (`--snapshot`, `--to`, `--map old=new`, `--newid`, `--force`): shows and restores the configs;
  never writes to `/etc/pve` on its own.
- Manual (de/en) and README: disaster recovery of a VM step by step (live-tested: OmniOS backup -> Proxmox node, new id, VM started).

## v3.1.1 (2026-10-14)

### Changed

- The ZFS receiver command `zfspull` is now called `zfsreceive` (receiver / slave mode): nothing is pulled, the sender
  pushes. `zfspull` still works as an alias, so existing services and the GUI keep running.

## v3.1.0 (2026-10-13)

### Fixed (after the live test)

- Idle detection: `written@n0` is read with `-p` (Linux printed `0B`, so an idle dataset counted as changed
  and was sent every cycle).
- The adaptive wait (2x the duration of the last send) no longer applies after the initial full send, so the
  first write afterwards is synced at the next cycle.

### Added

- **ZFS replication mode** (`cs-sync run --mode zfs`, receiver `cs-sync zfspull`): event-based realtime
  ZFS replication Primary â†’ Backup, local (one service) or LAN (service pair). Incremental
  `zfs send | zfs recv` after write events, snapshot slots `n0`/`n1`, `kh`/`kd` retention per side,
  GUID handshake (the Primary never queries the Backup's snapshots), resume tokens, rollback approval
  via `<id>.blocked` / `<id>.approve`, `--recursive y` (`send -R`), daily application-consistent snapshot
  through cs-freeze4snap (`--freeze hh:mm`, slot `kf`).
- New wire frames 15â€“20 (`internal/wire/zfs.go`); `ProtoVersion` unchanged.
- Live-tested Proxmox â†’ OmniOS over the LAN; Windows/OpenZFS tested locally.
## v3.0.1 (2026-08-18)

### Security

- **Harden the `serve` receiver against path traversal outside
  `--dest`.** Incoming wire paths are now validated to stay within the
  sync root before any filesystem operation. Previously a peer that
  holds the transfer key (i.e. a compromised or rogue sender) could
  escape `--dest` with crafted `..` or absolute paths and read, write
  or delete arbitrary files reachable by the service user. The fix is
  defense-in-depth on top of the existing pre-shared-key
  authentication: absolute paths and `..` traversal are refused, and
  the resolved path is confirmed to remain under `--dest`.

### Added

- Unit tests for receiver path validation (`internal/remote`).

## v3.0.0 (2026-08-11)

- Unified local/cs-sync/RustFS endpoint model (breaking rewrite vs 2.x).
