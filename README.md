# ZDrive Desktop

Mounts your ZDrive as a local drive. Files download on demand, by byte range, and are cached locally (LRU, size-capped). Built on [rclone](https://rclone.org)'s mount + VFS cache with a ZDrive backend (`backend/zdrive`). Plan: [docs/PLAN.md](docs/PLAN.md).

**Status: M2, read-write**, plus conflict detection (B3) and editor-save handling - create, edit, rename, move and delete (files go to ZDrive trash); a stale write gets a 409 and is kept as a separate file rather than silently overwriting someone else's change; Office/vim/etc. lock files never sync, and atomic saves (write temp, rename over original) land as a new version of the original instead of trashing it.

## Try it

1. Download `zdrive-binaries` from the latest run in the repo's **Actions** tab.
2. Sign in once: `./zdrive-<platform> login` (add `--url https://staging.zhdrive.in/api` to test against staging instead of production). Prints a code, opens a browser to approve it - approve there, and the resulting session is saved into rclone's own config, so no env vars or manual token-copying are needed afterward.
3. Mount:

**Windows** (install [WinFsp](https://winfsp.dev) first), PowerShell:
```powershell
.\zdrive-windows-amd64.exe login
.\zdrive-windows-amd64.exe mount zdrive: Z: --vfs-cache-mode full
```

**macOS** (no macFUSE needed: uses the built-in NFS client), Terminal:
```sh
xattr -d com.apple.quarantine ./zdrive-macos-arm64; chmod +x ./zdrive-macos-arm64
./zdrive-macos-arm64 login
mkdir -p ~/ZDrive && ./zdrive-macos-arm64 nfsmount zdrive: ~/ZDrive --vfs-cache-mode full
```
(Intel Mac: use `zdrive-macos-amd64`.)

Stop with Ctrl+C. Cache size: add `--vfs-cache-max-size 10G`.

**Manual/CI alternative** (skip `login`, e.g. to test a specific short-lived `zd_session` cookie): set `RCLONE_CONFIG_ZDRIVE_TYPE=zdrive`, `RCLONE_CONFIG_ZDRIVE_URL=<api url>`, `RCLONE_CONFIG_ZDRIVE_TOKEN=<token>` before `mount`/`nfsmount` - env vars still take priority over the config file `login` writes.

Note: on staging, files uploaded before the 2026-09-18 snapshot exist only in the DB, not in staging storage, so opening them fails. Upload fresh files to test content.
