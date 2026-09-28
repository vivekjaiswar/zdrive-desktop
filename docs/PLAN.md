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
- [ ] Test on Mac with `nfsmount` (no macFUSE).
- [ ] Ask Navimatics about a commercial WinFsp licence (needed to bundle WinFsp in a paid closed-source installer).
- ~~[ ] Hire / assign a Qt 6 C++ Windows developer~~ — **dropped 2026-09-26**: no Qt shell, see Decisions table above.
- [ ] Attach the customer request for local-drive access (design doc Open Question 1).
- [ ] Rotate the test-account password (it was shared in chat).
- [ ] **Backend PR #79 needs your review** (B3 conflict check, `feat/desktop-conflict-check`) — staging-verified, tests green, landing it into main is a human call this agent can't make on its own.
- [ ] **Backend PR #80 needs your review** (B4 device-code sign-in, `feat/desktop-device-auth`) — same reason, staging-verified, tests green.
- [ ] **Frontend PR #68 needs your review** (`/device` approval page, `feat/device-auth-approval`) — depends on PR #80 landing first to be testable end-to-end.

**Next for Claude, in order**
1. [x] **B3 conflict check, backend half**: `If-Match: <revision>` on `POST /files/:id/version`, `PATCH /files/:id`, `PATCH /files/:id/move` → 409. Shipped as PR #79.
2. [x] **B3 conflict check, client half**: rclone backend (`backend/zdrive/zdrive.go`) sends `If-Match` on Update/Move; a stale Update gets a 409 and the edit is kept as a new file (`name (conflicted copy <host> <date>).ext`) instead of failing outright. A stale Move/rename just surfaces the 409 (no auto-copy - rare case, no data-loss risk since nothing was overwritten). Only takes effect against staging once PR #79 is on main - coded against what B3's backend contract will be, not yet tested against a live 409.
3. [x] **Office/editor saves** (2026-09-28): lock/OS-metadata files (`~$*`, `.~lock.*#`, `*.swp`/`.swx`, `.DS_Store`, `Thumbs.db`, `desktop.ini`) never leave the local VFS cache (`syncSkip`). Write-temp-then-rename atomic saves recover as a new version of the original instead of trashing it (`Fs.Move`'s `recoverFromAtomicSave`, checks `GET /files/trash` for a same-name/-folder file trashed in the last 30s). Both covered by unit tests (`TestSyncSkip`, `TestMoveRecoversAtomicSave`); not yet exercised against a real editor on staging.
4. [x] **B4 device-code sign-in** (2026-09-28): backend PR #80 + frontend PR #68 + new `zdrive login` CLI command (`cmd/login`, cross-compiles clean on all 4 targets) that runs the flow and writes the resulting token into rclone's own config file - no more manually copying a `zd_session` cookie. Covered by 4 Go tests against a fake server (approved-after-pending, denied, expired, start-fails), all mocking out real sleeps and the real config file. Not yet tested end-to-end against a live staging deploy (needs #79/#80/#68 merged first).
5. [ ] **B5 minimum client version** (kill switch) and **B6 large uploads** via multipart `upload/initiate` + `upload/complete`.
6. [ ] Quota full → `ENOSPC`; retries/pacer for 429/5xx.
7. [ ] Backend: folder re-parenting (`PATCH /folders/:id {parentId}`) so DirMove is one call; trashed files orphaned when a folder is hard-deleted.
8. [ ] M3: change feed, offline queue, pin files, telemetry. M4: background auto-mount process + minimal tray icon (no Qt), installer, signing, auto-update, beta.

**Parked (owner decision)**: infra storage split (MinIO off EC2).

**Gotchas**: staging runs one PR branch at a time (last pushed wins); pre-2026-09-18 files exist in staging DB but not staging storage; never build in the shared production checkout; Go is at `~/.local/go/bin`.
