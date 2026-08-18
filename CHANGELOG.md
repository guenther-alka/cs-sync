# Changelog

All notable changes to cs-sync are documented here. Versions follow
`v<major>.<minor>.<patch>`; see the git tags for the full history.

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
