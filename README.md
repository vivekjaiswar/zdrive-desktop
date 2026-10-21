# ZDrive Desktop

Mounts your ZDrive as a local drive. Files download on demand, by byte range, and are cached locally (LRU, size-capped). Built on [rclone](https://rclone.org)'s mount + VFS cache with a ZDrive backend (`backend/zdrive`). Plan: [docs/PLAN.md](docs/PLAN.md).

**Status: M2, read-write**, plus conflict detection (B3) and editor-save handling - create, edit, rename, move and delete (files go to ZDrive trash); a stale write gets a 409 and is kept as a separate file rather than silently overwriting someone else's change; Office/vim/etc. lock files never sync, and atomic saves (write temp, rename over original) land as a new version of the original instead of trashing it.

## Try it

**Windows**: download `zdrive-windows-setup.exe` from the latest run in the repo's **Actions** tab and run it - it installs WinFsp automatically (only if not already present, silently, no separate download from anywhere else) and offers to sign in right on the finish page. Then, PowerShell:
```powershell
zdrive-windows-amd64.exe mount zdrive: Z: --vfs-cache-mode full
```
(Sign in again anytime from the Start Menu shortcut, or `zdrive-windows-amd64.exe login`.)

**macOS** (no macFUSE needed: uses the built-in NFS client): download `zdrive-binaries` from the same Actions run, Terminal:
```sh
xattr -d com.apple.quarantine ./zdrive-macos-arm64; chmod +x ./zdrive-macos-arm64
./zdrive-macos-arm64 login
mkdir -p ~/ZDrive && ./zdrive-macos-arm64 nfsmount zdrive: ~/ZDrive --vfs-cache-mode full
```
(Intel Mac: use `zdrive-macos-amd64`.)

`login` prints a code, opens a browser to approve it - approve there, and the resulting session is saved into rclone's own config, so no env vars or manual token-copying are needed afterward. `--url https://staging.zhdrive.in/api` on `login` tests against staging instead of production.

Stop with Ctrl+C. Cache size: add `--vfs-cache-max-size 10G`.

**Manual/CI alternative** (skip `login`, e.g. to test a specific short-lived `zd_session` cookie): set `RCLONE_CONFIG_ZDRIVE_TYPE=zdrive`, `RCLONE_CONFIG_ZDRIVE_URL=<api url>`, `RCLONE_CONFIG_ZDRIVE_TOKEN=<token>` before `mount`/`nfsmount` - env vars still take priority over the config file `login` writes.

Note: on staging, files uploaded before the 2026-09-18 snapshot exist only in the DB, not in staging storage, so opening them fails. Upload fresh files to test content.

## Security

Found via a `/cso` audit (2026-10-21):

- **The macOS/Linux NFS mount has no authentication at the protocol level** - confirmed directly in rclone's own source (`cmd/serve/nfs`), which says so outright: "There is no authentication available on this server." For as long as a mount is active, any other local process or OS user account able to reach the loopback NFS port (bound to `localhost:<random port>`) gets full, unauthenticated read/write access to the mounted drive - no credential check happens anywhere in the RPC path. This requires the attacker to already have local code execution on the machine (it's not remotely exploitable), so on a single-user Mac/Windows machine the practical risk is limited to another local process/app reaching loopback. **On a shared multi-user Linux host, it's a real unauthenticated cross-account data exposure** - don't run ZDrive this way on a machine with other untrusted local accounts. There's no fix available at this project's level: the vendored NFS server offers no auth option to enable, and no extension point to add one without forking rclone itself.
- **Fixed**: the Windows installer's bundled WinFsp driver MSI is now checksum-verified in CI (`WINFSP_SHA256` in `.github/workflows/build.yml`, checked against WinFsp's own published release hash) before being bundled - previously it was downloaded and silently installed with zero integrity check, meaning a compromised upstream WinFsp release would have shipped straight through into every ZDrive installer unnoticed.

## License

MIT, see [LICENSE](LICENSE). Copyright (c) 2026 ZennialHub Technologies - this open-source license covers only this client; the ZDrive backend/API it talks to is separate and closed-source. "ZDrive" and its logo are ZennialHub Technologies trademarks, not covered by the MIT license.

This client is FLOSS specifically so it can use [WinFsp](https://winfsp.dev) (GPLv3, with a [FLOSS exception](https://github.com/winfsp/winfsp/blob/master/License.txt) for software that is itself under an OSI/FSF-approved license) on Windows without a paid commercial license - the exception requires the linking/distributing software to be genuinely open, which this now is.
