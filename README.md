# ZDrive Desktop

Mounts your ZDrive as a local drive. Files download on demand, by byte range, and are cached locally (LRU, size-capped). Built on [rclone](https://rclone.org)'s mount + VFS cache with a ZDrive backend (`backend/zdrive`). Plan: [docs/PLAN.md](docs/PLAN.md).

**Status: M2, read-write.** Create, edit, rename, move and delete (files go to ZDrive trash). No conflict detection yet: last writer wins.

## Try it

1. Download `zdrive-binaries` from the latest run in the repo's **Actions** tab.
2. Get a session token: sign in at https://staging.zhdrive.in (or zhdrive.in) → DevTools → Application → Cookies → copy the `zd_session` value. It lasts 24 h.
3. Set config and mount:

**Windows** (install [WinFsp](https://winfsp.dev) first), PowerShell:
```powershell
$env:RCLONE_CONFIG_ZDRIVE_TYPE="zdrive"
$env:RCLONE_CONFIG_ZDRIVE_URL="https://staging.zhdrive.in/api"
$env:RCLONE_CONFIG_ZDRIVE_TOKEN="<zd_session value>"
.\zdrive-windows-amd64.exe mount zdrive: Z: --vfs-cache-mode full
```

**macOS** (no macFUSE needed: uses the built-in NFS client), Terminal:
```sh
xattr -d com.apple.quarantine ./zdrive-macos-arm64; chmod +x ./zdrive-macos-arm64
export RCLONE_CONFIG_ZDRIVE_TYPE=zdrive RCLONE_CONFIG_ZDRIVE_URL=https://staging.zhdrive.in/api RCLONE_CONFIG_ZDRIVE_TOKEN='<zd_session value>'
mkdir -p ~/ZDrive && ./zdrive-macos-arm64 nfsmount zdrive: ~/ZDrive --vfs-cache-mode full
```
(Intel Mac: use `zdrive-macos-amd64`.)

Stop with Ctrl+C. Cache size: add `--vfs-cache-max-size 10G`.

Note: on staging, files uploaded before the 2026-09-18 snapshot exist only in the DB, not in staging storage, so opening them fails. Upload fresh files to test content.
