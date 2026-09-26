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

### M0 — Backend foundations — B1 + B2 LIVE in production 2026-09-26; B3 moved to M2, B4 before beta (backend repo, normal PR → staging → merge flow)
- **B1** `GET /files/:id/stream` — Bearer-authenticated, honours `Range` (206 / `Content-Range` / `Accept-Ranges` / `ETag`), both encryption layouts + unencrypted. ← **first task**
- **B2** Paginated listing: `GET /drive/list?folderId=&cursor=` → `{ folders, files, nextCursor }` with `id, name, size, updatedAt, revision`.
- **B3** `If-Match` revision check on `POST /files/:id/version`, rename, move → `409` on mismatch.
- **B4** Device-code login (`/auth/device/start`, `/auth/device/poll`, web approval page at `/device`) issuing a Session-backed, revocable, longer-lived token.

### M1 — Read-only mount — BUILT 2026-09-26, verified on Linux vs staging; awaiting Windows/Mac test
- Go module: API client, token in OS keychain, config.
- cgofuse filesystem: `getattr`, `readdir` (lazy, TTL-cached), `open`/`read` via Range into an on-disk block cache (4 MB blocks) with LRU eviction.
- Writes return `EROFS` in this milestone.
- Test: Linux against staging here; cross-compiled Windows (WinFsp) and Mac (macFUSE) builds handed to owner.

### M2 — Writes
- `mkdir`, `create`, `rename`/move, `unlink` (→ trash, recoverable), `rmdir`.
- Write-back: edits go to a local staging file; on close, upload (new file → upload, existing → new version with `If-Match`).
- Conflict: server `409` → keep both, local copy saved as `name (conflicted copy <host> <date>).ext`.
- Editor save patterns: ignore lock/temp files (`~$*`, `.~lock*`, `*.swp`, `.DS_Store`, `Thumbs.db`); handle write-temp-then-rename atomic saves.
- Large files via existing multipart `upload/initiate` + `upload/complete`.
- Quota exceeded → `ENOSPC`.

### M3 — Sync correctness
- Change feed (`GET /sync/changes?cursor=`) replacing listing-TTL polling.
- Offline upload queue with retry; pin-to-keep-local.
- Crash reporting + mount success/failure telemetry (design OQ14).

### M4 — Ship
- Tray app shell, installers (bundling WinFsp / macFUSE), code signing + notarization (OQ7), auto-update + server-side kill switch (OQ15), opt-in beta + uninstall runbook (OQ10, OQ16).

## Known risks
- **macFUSE on Apple Silicon**: the classic kext backend requires lowering system security; newer macFUSE releases add an FSKit (user-space) backend on recent macOS — verify on the owner's Mac during M1.
- **Explorer/Finder polish** is below OneDrive's (no native placeholder badges) — accepted trade-off for Approach D.
- `POST /files/:id/version` and `/storage/upload` buffer uploads in memory (Multer) — large desktop writes must use the multipart path.
