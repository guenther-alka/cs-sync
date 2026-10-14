# Changelog

All notable changes to cs-sync are documented here. Versions follow
`v<major>.<minor>.<patch>`; see the git tags for the full history.

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
