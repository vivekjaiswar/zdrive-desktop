# ZDrive Desktop — build plan

Status: in progress (started 2026-09-26)
Source design: `~/.gstack/projects/zennial-drive/ubuntu-multi-tenant-vision-design-20260924-213535.md` (APPROVED 2026-09-24)

## Decisions (2026-09-26)

| Question | Decision |
|---|---|
| Approach | **D — rclone as a library + a ZDrive backend** (`backend/zdrive`). rclone supplies mount (WinFsp on Windows, built-in NFS client on macOS via `nfsmount`, FUSE on Linux), the VFS block cache and LRU eviction. Native CfApi / File Provider can come later. |
| Write path (design OQ2) | **Full read-write**, delivered in milestones: read-only first, then writes. |
| Test machines | Windows PC + Mac (owner's). Linux (this box) for dev/CI, always against **staging.zhdrive.in**, never production. |
| Eviction (design OQ3) | Size-capped LRU block cache, default 10 GB, user-configurable; pinned files exempt (pinning lands in M3). |
| Background auth (design OQ4) | Client sends `Authorization: Bearer` to a new Range-capable stream endpoint — no 60 s ticket involved. Long-lived login via device-code flow (B4, before beta; M1 testing uses the `zd_session` cookie). |
| Demand evidence (design OQ1) | Owner chose to proceed with the build; attach the ticket when available. |
| Client shell (2026-09-26, revised) | **No Qt tray GUI app.** The downloaded exe auto-mounts the drive on run (background process, minimal or no tray icon — just enough for quit/status). All account management (usage, devices, sharing, billing) stays in the existing web app and mobile app — one codebase to maintain instead of Qt C++ + web + mobile. Same principle on iOS: the mobile app is the management surface; a Files-app mount still needs a native File Provider Extension (unavoidable on iOS), but it carries no separate account-management UI. **Kills the "hire a Qt 6 C++ developer" line item entirely.** |
| WinFsp licensing (2026-09-28) | This repo (and `zdrive-mobile`) are now **open-sourced under MIT** (owner retains copyright/trademark - only the license grant to others changed, backend/API stays closed) specifically so WinFsp's GPLv3 FLOSS exception applies: bundling + silently auto-installing WinFsp in a real installer no longer needs WinFsp's $6,000/3yr commercial license. Verified via the actual license text, not assumed - see README. No architecture change: still rclone + WinFsp (Approach D), same as before. |

## Hard scale rules (from the design)

1. Must work for a 1 TB `BUSINESS_TEAM` allocation without that much local disk → content is fetched **on demand, by byte range**, never whole-drive.
2. Directory listings are fetched **lazily per folder** as the user opens them, never an eager tree walk. Listings must be **paginated** (today's 1000-row cap silently truncates).

## Backend gaps found (2026-09-26)

- `GET /files/:id/content` has no Range support: every read streams the whole file from byte 0.
- Files are AES-256-GCM encrypted as a single stream (two layouts, see `File.streamEncrypted`). Range reads are still possible: GCM is CTR mode underneath, so plaintext offset `o` maps to counter block `2 + o/16`. Trade-off: a partial read cannot verify the GCM auth tag (only a full read can). Transport is TLS; at-rest tamper detection still applies to full downloads.
- Folder children / files-in-folder capped at `MAX_LIST_RESULTS = 1000`, no cursor.
- No optimistic-concurrency check on rename/move/new-version → two writers silently overwrite each other.
- No change feed → remote changes are only discoverable by re-listing.
- JWT lifetime 24 h, Google-idToken login only → unsuitable for an always-on desktop service.

## Milestones

### M0 — Backend foundations — B1 + B2 LIVE in production; B3 + B4 both opened as PRs (backend repo, normal PR → staging → merge flow), pending review
- **B1** `GET /files/:id/stream` — Bearer-authenticated, honours `Range` (206 / `Content-Range` / `Accept-Ranges` / `ETag`), both encryption layouts + unencrypted. ← **first task**
- **B2** Paginated listing: `GET /drive/list?folderId=&cursor=` → `{ folders, files, nextCursor }` with `id, name, size, updatedAt, revision`.
- **B3** `If-Match` revision check on `POST /files/:id/version`, `PATCH /files/:id`, `PATCH /files/:id/move` → `409` on mismatch. **Backend + client both done** (PR #79, `feat/desktop-conflict-check`; client side in `zdrive.go`). Awaiting merge.
- **B4** (2026-09-28, done) Device-code login: `POST /auth/device/start` + `/approve`/`/deny` + `/poll`, `/device` approval page (frontend PR #68), issuing a Session-backed, revocable, 90-day-default token (`DEVICE_SESSION_EXPIRES_IN`). Backend PR #80. Client: new `zdrive login` command (`cmd/login`) runs the flow and writes `type`/`url`/`token` straight into rclone's config file - `zdrive mount zdrive: ...` needs no env vars afterward. All three PRs (#79, #80, frontend #68) awaiting review/merge.

### M1 — Read-only mount — BUILT 2026-09-26, verified on Linux vs staging; awaiting Windows/Mac test
- Go module: API client, token in OS keychain, config.
- cgofuse filesystem: `getattr`, `readdir` (lazy, TTL-cached), `open`/`read` via Range into an on-disk block cache (4 MB blocks) with LRU eviction.
- Writes return `EROFS` in this milestone.
- Test: Linux against staging here; cross-compiled Windows (WinFsp) and Mac (macFUSE) builds handed to owner.

### M2 — Writes — BUILT 2026-09-26 (fake-API tests only; not yet run against staging)
- Done: mkdir, create (`POST /storage/upload`), edit (`POST /files/:id/version`), file rename/move, unlink → trash, rmdir (empty only), folder rename in place. Write-back/staging is rclone's VFS cache (`--vfs-cache-mode full`).
- Not yet: If-Match/409 conflict copies (needs B3), editor temp-file ignore list, multipart large uploads (single request, ≤2 GB, buffered server-side), `ENOSPC` mapping. Folder moves across parents fall back to per-file moves (API can't re-parent folders).
- `mkdir`, `create`, `rename`/move, `unlink` (→ trash, recoverable), `rmdir`.
- Write-back: edits go to a local staging file; on close, upload (new file → upload, existing → new version with `If-Match`).
- Conflict: server `409` → keep both, local copy saved as `name (conflicted copy <host> <date>).ext`.
- Editor save patterns (2026-09-28, done): lock/OS-metadata files (`~$*`, `.~lock.*#`, `*.swp`/`.swx`, `.DS_Store`, `Thumbs.db`, `desktop.ini`) are local-only, never uploaded (`syncSkip`) - confirmed rclone's own `--exclude` filtering does NOT reach mount's read/write path (sync/copy-only), so this has to live in the backend, not as a documented mount flag. Write-temp-then-rename atomic saves: rclone's own move helper deletes the destination before renaming the temp file onto its name (traced in vendored rclone source, `fs/operations/operations.go` `move()`); recovered in `Fs.Move` by checking `GET /files/trash` for a same-name, same-folder file trashed in the last 30s and, if found, restoring it and uploading the temp content as its next version instead of letting the temp object take over the name.
- Large files via existing multipart `upload/initiate` + `upload/complete`.
- Quota exceeded → `ENOSPC`.

### M3 — Sync correctness
- Change feed (`GET /sync/changes?cursor=`) replacing listing-TTL polling.
- Offline upload queue with retry; pin-to-keep-local.
- Crash reporting + mount success/failure telemetry (design OQ14).

### M4 — Ship
- No Qt shell (revised 2026-09-26, see Decisions table): a background auto-mount process, launched on login, with at most a minimal tray icon (status/quit/open-web-app) — no account-management UI, no C++/Qt toolchain. Installers (bundling WinFsp / macFUSE), code signing + notarization (OQ7), auto-update + server-side kill switch (OQ15), opt-in beta + uninstall runbook (OQ10, OQ16) still apply.

## Known risks
- **macFUSE on Apple Silicon**: the classic kext backend requires lowering system security; newer macFUSE releases add an FSKit (user-space) backend on recent macOS — verify on the owner's Mac during M1.
- **Explorer/Finder polish** is below OneDrive's (no native placeholder badges) — accepted trade-off for Approach D.
- `POST /files/:id/version` and `/storage/upload` buffer uploads in memory (Multer) — large desktop writes must use the multipart path.

## To-do (as of 2026-09-28, updated)

**Waiting on owner**
- [x] Backend PR #78 (missing stored object → 404) — landed on main, verified live in production.
- [x] Test release **v0.2.0-m2** on Windows (Z: drive): create folder, drag files in, edit in Notepad, rename, delete — done, confirmed against staging.zhdrive.in (real DB rows verified, not just self-report).
- [ ] Test on Windows/Mac with the real, current build (installer + B3/B4/editor-save/B5/B6 all included) - the earlier "v0.2.0-m2" test above predates all of that; still open.
- ~~[ ] Ask Navimatics about a commercial WinFsp licence~~ — **moot as of 2026-09-28**: this repo is now MIT-licensed, which qualifies for WinFsp's GPLv3 FLOSS exception (free) instead of the $6,000/3yr commercial license.
- ~~[ ] Hire / assign a Qt 6 C++ Windows developer~~ — **dropped 2026-09-26**: no Qt shell, see Decisions table above.
- [ ] Attach the customer request for local-drive access (design doc Open Question 1).
- [ ] Rotate the test-account password (it was shared in chat).
- [ ] Get real legal counsel to confirm the WinFsp FLOSS-exception licensing read before leaning on it at scale - verified via the actual license text, not legal advice.
- [ ] Code-sign the Windows installer - it's unsigned, SmartScreen warns on first run.
- [x] ~~Backend PR #79 (B3)~~ / ~~PR #80 (B4)~~ / ~~frontend PR #68 (`/device`)~~ — all merged and confirmed live in production 2026-09-28.
- [x] ~~Backend PR #81 (B5 min client version)~~ — merged 2026-09-29, confirmed live in production.
- [ ] **Backend PR #82 needs your review** (no upload-size cap + multipart for versions, `feat/desktop-unlimited-upload-size`) — staging-verified, tests green, landing it into main is a human call this agent can't make on its own.

**Next for Claude, in order**
1. [x] **B3 conflict check, backend half**: `If-Match: <revision>` on `POST /files/:id/version`, `PATCH /files/:id`, `PATCH /files/:id/move` → 409. Shipped as PR #79.
2. [x] **B3 conflict check, client half**: rclone backend (`backend/zdrive/zdrive.go`) sends `If-Match` on Update/Move; a stale Update gets a 409 and the edit is kept as a new file (`name (conflicted copy <host> <date>).ext`) instead of failing outright. A stale Move/rename just surfaces the 409 (no auto-copy - rare case, no data-loss risk since nothing was overwritten). Only takes effect against staging once PR #79 is on main - coded against what B3's backend contract will be, not yet tested against a live 409.
3. [x] **Office/editor saves** (2026-09-28): lock/OS-metadata files (`~$*`, `.~lock.*#`, `*.swp`/`.swx`, `.DS_Store`, `Thumbs.db`, `desktop.ini`) never leave the local VFS cache (`syncSkip`). Write-temp-then-rename atomic saves recover as a new version of the original instead of trashing it (`Fs.Move`'s `recoverFromAtomicSave`, checks `GET /files/trash` for a same-name/-folder file trashed in the last 30s). Both covered by unit tests (`TestSyncSkip`, `TestMoveRecoversAtomicSave`); not yet exercised against a real editor on staging.
4. [x] **B4 device-code sign-in** (2026-09-28): backend PR #80 + frontend PR #68 + new `zdrive login` CLI command (`cmd/login`, cross-compiles clean on all 4 targets) that runs the flow and writes the resulting token into rclone's own config file - no more manually copying a `zd_session` cookie. Covered by 4 Go tests against a fake server (approved-after-pending, denied, expired, start-fails), all mocking out real sleeps and the real config file. Not yet tested end-to-end against a live staging deploy (needs #79/#80/#68 merged first).
5. [x] **Windows installer bundling WinFsp** (2026-09-28, pulled forward from M4 - user explicitly asked "why does the user need to install WinFsp separately"): `installer/windows/zdrive-installer.nsi` (NSIS, compiles on Linux CI - no Windows runner needed), silently runs WinFsp's own MSI installer (`msiexec /quiet`) only if `HKLM\SOFTWARE\WOW6432Node\WinFsp` isn't already set, offers to launch `zdrive login` on the finish page. CI (`build.yml`) downloads a pinned WinFsp release (`WINFSP_VERSION` env var - bump there when a newer WinFsp is wanted, script itself never changes) and produces `zdrive-windows-setup.exe` as a build artifact alongside the existing raw binaries. Verified for real, not just written: installed `nsis` and compiled the actual script against the actual built exe and a real downloaded WinFsp MSI locally, produced a genuine 16MB PE installer - not yet run through an actual Windows install (no Windows machine in this environment), that's the next physical-hardware step alongside Mac testing. This is what made "download one exe, everything works" literally true for Windows, without changing the mount architecture (still rclone + WinFsp) - the WinFsp bundling was blocked purely on licensing, resolved by open-sourcing the client (see LICENSE / Decisions table), not by new mount code.
6. [x] **B5 minimum client version (kill switch), 2026-09-28**: backend PR #81 - global `DesktopClientVersionGuard` checks `X-ZDrive-Client-Version` on every request (426 if below `MIN_DESKTOP_CLIENT_VERSION`, untouched if the header's absent - never affects the web app). Client sends it on every request via `ClientVersion = "0.3.0"` (`backend/zdrive/zdrive.go`), including `zdrive login`'s own calls (`cmd/login`, needs the header too since a killed version shouldn't even complete sign-in). Covered by tests both sides (12 backend, 2 new client) - client-side confirms the header genuinely reaches a fake server, not just that the code compiles.
   **B6 large uploads (multipart), 2026-09-28**: client-only change (backend `upload/initiate`+`upload/complete` already existed) - `Fs.Put` routes files ≥50MB (matches the backend's own `LARGE_UPLOAD_MIN_SIZE_BYTES` default) through the presigned-multipart flow instead of the single-POST path: hashes the file (needs a seekable reader - `--vfs-cache-mode full` always provides one in practice), initiates, PUTs each 16MB part directly to storage (not through the API server - matches `MULTIPART_PART_SIZE_BYTES`), completes. Falls back to the simple upload path if the reader isn't seekable. Covered by `TestPutLarge`.
7. [x] **No app-level upload size cap; multipart extended to versions (2026-09-29, user explicitly asked "no threshold limitation to upload or download, only their plan should limit it")**: backend PR #82 removes `LARGE_UPLOAD_MAX_SIZE_BYTES` (10GB) entirely - `reserveStorage` (the real, atomic plan-quota check) was already sufficient and this was a redundant ceiling on top of it, given the product bills per-GB and large plans are real. Part size now scales dynamically with file size (`computePartSize`) so a very large file doesn't hit S3/MinIO's real 10,000-part ceiling with a fixed 16MB part size; the initiate response now carries the actual `partSize` used. New `POST /files/:id/version/initiate` + `/complete` close the gap B6 left open - large *version* uploads (editing an existing large file) now use the same presigned-multipart flow as creates instead of the memory-buffered simple path, sharing the relay/hash/scan logic via a new `finalizeMultipartRelay` helper. Client (`backend/zdrive/zdrive.go`): `Object.Update` now mirrors `Put`'s small/large split via `updateLarge`; reads the server's dynamic `partSize` instead of a hardcoded 16MB constant (both `Put` and `Update`); B3's "keep both" conflict resolution is also size-aware (`keepBothCopy`), so a stale large edit resolves via a large conflicted-copy upload, not a doomed attempt to buffer it through the simple path. Caught and fixed a real client/server contract bug while writing `TestUpdateLarge`: the client initially expected the large-version-complete response wrapped in `{"file": ...}` (copying the wrong pattern from the simple version-upload path) when the actual endpoint returns a flat object matching `completeLargeUpload`'s shape - the new test caught this before it ever shipped. 9 client tests total for the multipart paths (up from 2), all passing; 19 backend tests for the large-upload paths.
8. [x] **Retries/pacer for 429/5xx (2026-09-29, client-only)**: `withRetry` (exponential backoff, 5 attempts) applied selectively, not blanket - only at call sites where a retry genuinely can't cause harm: reads (`List`/`listDir`, `About`, `Open`/stream, the trash lookup), presigned-part `PUT`s (idempotent by S3 multipart's own design - re-uploading the same part number just overwrites it), and the multipart-complete steps (protected by the backend's own `PendingUpload` status tracking, so a lost-response retry cleanly fails with "no pending upload found" instead of double-applying). Deliberately NOT applied to non-idempotent writes with no server-side dedup (create/rename/move/simple-version-upload, and the multipart *initiate* calls - retrying those risks double-reserving quota or duplicate resources). 8 new tests (`retry_test.go`), including one proving a real 503-then-success round-trip through `putPart` without the caller ever seeing an error.
   **`ENOSPC` on quota-full - investigated, not simply skipped: this rclone version (v1.75.1, latest available) has no path to it.** Traced `cmd/cmount/fs.go`'s `translateError` (Go error → FUSE errno) - its switch only recognizes a small fixed set of `vfs.*` sentinels (`ENOTEMPTY`, `ESPIPE`, `EBADF`, `EROFS`, `ENOSYS`, `ELOOP`, plus a few `os.Err*` aliases), and `vfs/errors.go` has **no `ENOSPC` constant at all** - anything else falls through to generic `EIO`. Confirmed no newer rclone release exists to pick this up (`go list -m -versions` tops out at the pinned v1.75.1). The only way to get a real "disk full" OS error would be forking/patching this dependency - real, ongoing maintenance cost for what's a UX polish item, not a correctness one (the write still fails cleanly with a clear message today, nothing is lost or corrupted). Left as `EIO` with a clear underlying message rather than silently building something that looks like it fixes this but doesn't change the OS-level code. Worth a real decision from the user if this UX gap matters enough to justify a fork - not decided unilaterally here.
9. [x] **Folder re-parenting + orphaned-trash fix (2026-10-02)**: backend PR [zennial-drive#90](https://github.com/vivekjaiswar/zennial-drive/pull/90) (awaiting merge) adds `PATCH /folders/:id/move { parentId }` (cycle-guarded - walks up the destination's own ancestor chain and rejects if the folder being moved would become its own descendant) and fixes a real bug: `delete()`'s guard only ever counted *active* files before allowing a hard-delete, so an already-trashed file left pointing at a folder that no longer existed once it was gone (no FK/cascade on `File.folderId`) - `delete()` now finalizes those via `FilesService.permanentlyDelete` first. Client PR [zdrive-desktop#1](https://github.com/vivekjaiswar/zdrive-desktop/pull/1) (awaiting merge, depends on the backend PR): `DirMove`'s cross-parent branch now calls the new endpoint directly instead of returning `ErrorCantDirMove` (which made rclone's VFS fall back to moving every file individually). 3 new client tests, `go test ./...`/`go vet`/`gofmt` clean, all 4 CI cross-compile targets build.
10. [ ] M3: change feed, offline queue, pin files, telemetry. M4 remainder: background auto-mount process launched on login + minimal tray icon (no Qt), code signing (the installer itself is unsigned - Windows SmartScreen will warn on first run until that's resolved), auto-update, beta. Mac equivalent installer (bundle nothing - macOS's built-in NFS client needs no driver - but still worth a proper `.pkg` for the same one-download experience).

**Parked (owner decision)**: infra storage split (MinIO off EC2).

**Gotchas**: staging runs one PR branch at a time (last pushed wins); pre-2026-09-18 files exist in staging DB but not staging storage; never build in the shared production checkout; Go is at `~/.local/go/bin`.
