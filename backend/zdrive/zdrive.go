// Package zdrive is an rclone backend for the ZDrive API. rclone supplies
// the mount, VFS cache and eviction; this file only maps paths to ZDrive
// folder/file IDs, reads content by byte range and writes via the web API.
package zdrive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/rest"
)

const rootID = "root"

// ClientVersion is sent as X-ZDrive-Client-Version on every request (B5).
// The backend's kill switch (MIN_DESKTOP_CLIENT_VERSION) compares against
// this to block versions known to be broken/unsafe - bump on every release
// that changes wire-visible behavior.
const ClientVersion = "0.3.0"

// ZDrive names may contain anything; encode only what can't be a path segment.
const enc = encoder.Standard

// Must match the backend's LARGE_UPLOAD_MIN_SIZE_BYTES default
// (storage.service.ts) - below this, the simple single-POST path is used
// instead. The per-part size is NOT a client-side constant: the server
// scales it with file size (to stay within S3/MinIO's 10,000-part ceiling
// on very large files) and returns the actual partSize it used, which the
// client reads directly rather than assuming a fixed value.
const largeUploadMinBytes = 50 * 1024 * 1024

// syncSkip reports whether leaf is an editor lock/swap file or OS metadata
// that should live only in the local VFS cache and never sync remotely.
// rclone's own --exclude filtering doesn't reach mount's read/write path
// (it's a sync/copy-only mechanism), so this has to live in the backend.
func syncSkip(leaf string) bool {
	switch {
	case strings.HasPrefix(leaf, "~$"): // Office lock file
	case strings.HasPrefix(leaf, ".~lock.") && strings.HasSuffix(leaf, "#"): // LibreOffice lock file
	case strings.HasSuffix(leaf, ".swp"), strings.HasSuffix(leaf, ".swx"): // vim swap
	case leaf == ".DS_Store", leaf == "Thumbs.db", leaf == "desktop.ini":
	default:
		return false
	}
	return true
}

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "zdrive",
		Description: "ZDrive",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:    "url",
			Help:    "API base URL.",
			Default: "https://zhdrive.in/api",
		}, {
			Name:      "token",
			Help:      "Session token (JWT).",
			Required:  true,
			Sensitive: true,
		}},
	})
}

// Options are the backend's config values.
type Options struct {
	URL   string `config:"url"`
	Token string `config:"token"`
}

// Fs is a ZDrive account rooted at some folder.
type Fs struct {
	name     string
	root     string
	features *fs.Features
	srv      *rest.Client
	dirCache *dircache.DirCache
}

// Object is a ZDrive file.
type Object struct {
	fs      *Fs
	remote  string
	id      string
	size    int64
	modTime time.Time
}

type apiFolder struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type apiFile struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Size      json.Number `json:"size"` // string from list, number from upload
	UpdatedAt time.Time   `json:"updatedAt"`
}

type listPage struct {
	Folders    []apiFolder `json:"folders"`
	Files      []apiFile   `json:"files"`
	NextCursor *string     `json:"nextCursor"`
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("zdrive: %d %s", e.Status, e.Message) }

func errorHandler(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		Message any `json:"message"`
	}
	_ = json.Unmarshal(body, &parsed)
	return &apiError{Status: resp.StatusCode, Message: fmt.Sprint(parsed.Message)}
}

func isNotFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func isConflict(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusConflict
}

// isRetriable reports whether err is a transient failure worth retrying:
// 429 (rate limited - the server explicitly rejected the request, nothing
// was processed) or a 5xx. Deliberately excludes every other 4xx (400,
// 401, 403, 404, 409, ...) - retrying those can't fix anything, they're
// telling us the request itself was wrong, not that timing was bad.
func isRetriable(err error) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// var, not const, so tests can shrink retryBaseDelay to exercise the full
// retry-exhaustion path without a real multi-second test.
var (
	maxRetries     = 5
	retryBaseDelay = 500 * time.Millisecond
)

// withRetry calls fn, retrying with exponential backoff (base * 2^attempt)
// on transient errors up to maxRetries times. Deliberately used only at
// call sites that are safe to repeat - a plain read, an S3 multipart part
// PUT (idempotent by design: re-uploading the same part number just
// overwrites it), or a call the server itself protects against duplicate
// effects (the multipart complete steps, guarded by PendingUpload's own
// status tracking). Never wraps a write with no such protection - a lost
// response after a real write must surface as an error, not risk being
// silently repeated into a duplicate.
func withRetry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err = fn()
		if err == nil || !isRetriable(err) {
			return err
		}
		if attempt == maxRetries {
			break
		}
		delay := retryBaseDelay * time.Duration(1<<attempt)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// revisionOf is the If-Match value for a write against a file last seen with
// this modTime - the same value drive/list's "revision" field carries, so a
// write only succeeds if the file hasn't changed since this client's last
// listing or open (B3).
func revisionOf(modTime time.Time) string {
	return strconv.FormatInt(modTime.UnixMilli(), 10)
}

// NewFs builds an Fs from config.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	f := &Fs{
		name: name,
		root: strings.Trim(root, "/"),
		srv: rest.NewClient(fshttp.NewClient(ctx)).
			SetRoot(strings.TrimRight(opt.URL, "/")).
			SetHeader("Authorization", "Bearer "+opt.Token).
			SetHeader("X-ZDrive-Client-Version", ClientVersion).
			SetErrorHandler(errorHandler),
	}
	f.features = (&fs.Features{
		DuplicateFiles:          true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)
	f.dirCache = dircache.New(f.root, rootID, f)

	if err := f.dirCache.FindRoot(ctx, false); err != nil {
		// Root may point at a file: re-root at its parent (rclone convention).
		newRoot, remote := dircache.SplitPath(f.root)
		parent := *f
		parent.root = newRoot
		parent.dirCache = dircache.New(newRoot, rootID, &parent)
		if parent.dirCache.FindRoot(ctx, false) != nil {
			return f, nil
		}
		if _, err := parent.NewObject(ctx, remote); err != nil {
			return f, nil
		}
		f.root, f.dirCache = newRoot, parent.dirCache
		return f, fs.ErrorIsFile
	}
	go f.pollChanges(ctx)
	return f, nil
}

// listDir calls fn for every page of a folder's direct children.
func (f *Fs) listDir(ctx context.Context, dirID string, fn func(*listPage) (stop bool)) error {
	params := url.Values{"limit": {"1000"}}
	if dirID != rootID {
		params.Set("folderId", dirID)
	}
	for {
		var page listPage
		opts := rest.Opts{Method: "GET", Path: "/drive/list", Parameters: params}
		err := withRetry(ctx, func() error {
			_, err := f.srv.CallJSON(ctx, &opts, nil, &page)
			return err
		})
		if err != nil {
			return err
		}
		if fn(&page) || page.NextCursor == nil {
			return nil
		}
		params.Set("cursor", *page.NextCursor)
	}
}

// FindLeaf implements dircache.DirCacher.
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (string, bool, error) {
	var id string
	err := f.listDir(ctx, pathID, func(p *listPage) bool {
		for _, d := range p.Folders {
			if enc.ToStandardName(d.Name) == leaf {
				id = d.ID
				return true
			}
		}
		return false
	})
	if isNotFound(err) {
		return "", false, nil
	}
	return id, id != "", err
}

// CreateDir implements dircache.DirCacher.
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (string, error) {
	body := map[string]string{"name": enc.FromStandardName(leaf)}
	if pathID != rootID {
		body["parentId"] = pathID
	}
	var folder apiFolder
	opts := rest.Opts{Method: "POST", Path: "/folders"}
	_, err := f.srv.CallJSON(ctx, &opts, body, &folder)
	return folder.ID, err
}

// List the entries in dir.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}
	var entries fs.DirEntries
	err = f.listDir(ctx, dirID, func(p *listPage) bool {
		for _, d := range p.Folders {
			remote := path.Join(dir, enc.ToStandardName(d.Name))
			f.dirCache.Put(remote, d.ID)
			entries = append(entries, fs.NewDir(remote, d.UpdatedAt).SetID(d.ID))
		}
		for _, file := range p.Files {
			entries = append(entries, f.newObject(path.Join(dir, enc.ToStandardName(file.Name)), file))
		}
		return false
	})
	if isNotFound(err) {
		return nil, fs.ErrorDirNotFound
	}
	return entries, err
}

func (f *Fs) newObject(remote string, file apiFile) *Object {
	size, _ := file.Size.Int64()
	return &Object{fs: f, remote: remote, id: file.ID, size: size, modTime: file.UpdatedAt}
}

// newSkippedObject represents a syncSkip file: it exists only in the local
// VFS cache (id "") and is never read from or written to the API. Object
// methods check for the empty id and no-op instead of calling the backend.
func (f *Fs) newSkippedObject(remote string) *Object {
	return &Object{fs: f, remote: remote, modTime: time.Now()}
}

// NewObject finds the file at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	leaf, dirID, err := f.dirCache.FindPath(ctx, remote, false)
	if err != nil {
		if errors.Is(err, fs.ErrorDirNotFound) {
			return nil, fs.ErrorObjectNotFound
		}
		return nil, err
	}
	var found *Object
	err = f.listDir(ctx, dirID, func(p *listPage) bool {
		for _, file := range p.Files {
			if enc.ToStandardName(file.Name) == leaf {
				found = f.newObject(remote, file)
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fs.ErrorObjectNotFound
	}
	return found, nil
}

// upload POSTs in as multipart field "file" plus params, decoding the reply
// into result. Streams: nothing is buffered client-side. headers may be nil.
func (f *Fs) upload(ctx context.Context, apiPath string, params url.Values, name string, in io.Reader, src fs.ObjectInfo, result any, headers map[string]string) error {
	pr, pw := io.Pipe()
	defer pr.Close() // unblocks the writer if the request fails early
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(func() error {
			for k := range params {
				if err := mw.WriteField(k, params.Get(k)); err != nil {
					return err
				}
			}
			// FormatMediaType emits RFC 2231 filename* for non-ASCII names;
			// busboy (multer) would read a raw UTF-8 filename as latin1.
			h := textproto.MIMEHeader{}
			h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": name}))
			h.Set("Content-Type", fs.MimeType(ctx, src))
			w, err := mw.CreatePart(h)
			if err != nil {
				return err
			}
			if _, err := io.Copy(w, in); err != nil {
				return err
			}
			return mw.Close()
		}())
	}()
	opts := rest.Opts{Method: "POST", Path: apiPath, Body: pr, ContentType: mw.FormDataContentType(), ExtraHeaders: headers}
	_, err := f.srv.CallJSON(ctx, &opts, nil, result)
	return err
}

// Put uploads a new file via POST /storage/upload, or via the presigned
// multipart flow (B6) once the file crosses largeUploadMinBytes - matches
// the backend's own LARGE_UPLOAD_MIN_SIZE_BYTES default (50MB), below
// which the simple path is both simpler and genuinely cheaper server-side.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	leaf, dirID, err := f.dirCache.FindPath(ctx, src.Remote(), true)
	if err != nil {
		return nil, err
	}
	if syncSkip(leaf) {
		_, _ = io.Copy(io.Discard, in) // drain so the VFS write-back doesn't block on us
		return f.newSkippedObject(src.Remote()), nil
	}
	if src.Size() >= largeUploadMinBytes {
		if obj, ok, err := f.putLarge(ctx, in, src, leaf, dirID); ok {
			return obj, err
		}
		// putLarge returned before touching in (only possible when it
		// isn't seekable, which --vfs-cache-mode full always avoids in
		// practice) - safe to fall through to the simple upload below.
	}
	params := url.Values{}
	if dirID != rootID {
		params.Set("folderId", dirID)
	}
	var file apiFile
	if err := f.upload(ctx, "/storage/upload", params, enc.FromStandardName(leaf), in, src, &file, nil); err != nil {
		return nil, err
	}
	// ponytail: upload reply has no updatedAt; now is within ms of it, and the
	// next listing corrects it (costs at most one cache re-fetch).
	file.UpdatedAt = time.Now()
	return f.newObject(src.Remote(), file), nil
}

// initiateUploadResp is the shared shape of both /storage/upload/initiate
// and /files/:id/version/initiate - partSize is the server's actual chosen
// per-part size (it scales this up for very large files to stay within
// S3/MinIO's 10,000-part ceiling), not a value the client gets to assume.
type initiateUploadResp struct {
	UploadID string `json:"uploadId"`
	PartSize int    `json:"partSize"`
	Parts    []struct {
		PartNumber int    `json:"partNumber"`
		URL        string `json:"url"`
	} `json:"parts"`
}

// hashAndRewind hashes all of in (which must be fully consumed to do so)
// and seeks it back to the start, so the caller can then read it again for
// the actual upload. Returns ok=false, untouched, if in isn't seekable.
func hashAndRewind(in io.Reader) (contentHash string, ok bool, err error) {
	seeker, ok := in.(io.Seeker)
	if !ok {
		return "", false, nil
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, in); err != nil {
		return "", true, err
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return "", true, err
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

// uploadParts reads exactly resp.PartSize bytes per part from in (the last
// part may be shorter) and PUTs each directly to its presigned URL,
// collecting the ETags the complete step needs to identify them.
func uploadParts(ctx context.Context, in io.Reader, resp *initiateUploadResp) ([]map[string]any, error) {
	completedParts := make([]map[string]any, 0, len(resp.Parts))
	buf := make([]byte, resp.PartSize)
	for _, part := range resp.Parts {
		n, err := io.ReadFull(in, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		etag, err := putPart(ctx, part.URL, buf[:n])
		if err != nil {
			return nil, err
		}
		completedParts = append(completedParts, map[string]any{"partNumber": part.PartNumber, "etag": etag})
	}
	return completedParts, nil
}

// putLarge implements B6: uploads via POST /storage/upload/initiate, then
// PUTs each part directly to presigned storage URLs (the file never
// round-trips through the API server's own request body), then finalizes
// via POST /storage/upload/complete. The bool return is whether putLarge
// actually ran (false only when in isn't seekable, so the caller knows not
// to also try the fallback path against an already-drained reader).
func (f *Fs) putLarge(ctx context.Context, in io.Reader, src fs.ObjectInfo, leaf, dirID string) (fs.Object, bool, error) {
	contentHash, ok, err := hashAndRewind(in)
	if !ok {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}

	body := map[string]any{
		"name":        enc.FromStandardName(leaf),
		"size":        src.Size(),
		"mimeType":    fs.MimeType(ctx, src),
		"contentHash": contentHash,
	}
	if dirID != rootID {
		body["folderId"] = dirID
	}
	var init initiateUploadResp
	initOpts := rest.Opts{Method: "POST", Path: "/storage/upload/initiate"}
	if _, err := f.srv.CallJSON(ctx, &initOpts, body, &init); err != nil {
		return nil, true, err
	}

	completedParts, err := uploadParts(ctx, in, &init)
	if err != nil {
		return nil, true, err
	}

	var file apiFile
	completeOpts := rest.Opts{Method: "POST", Path: "/storage/upload/complete"}
	completeBody := map[string]any{"uploadId": init.UploadID, "parts": completedParts}
	// Safe to retry: a lost response after a real completion just means the
	// PendingUpload row is already COMPLETED, so a retry cleanly fails with
	// "no pending upload found" rather than creating a second File row.
	err = withRetry(ctx, func() error {
		_, err := f.srv.CallJSON(ctx, &completeOpts, completeBody, &file)
		return err
	})
	if err != nil {
		return nil, true, err
	}
	file.UpdatedAt = time.Now() // ponytail: complete's reply has no updatedAt either, same as the simple path
	return f.newObject(src.Remote(), file), true, nil
}

// putPart PUTs one part directly to a presigned storage URL (bypassing
// f.srv - these URLs carry their own auth, not our Bearer token) and
// returns the ETag the multipart complete step needs to identify it.
// Retried on transient failures (429/5xx) - safe by S3 multipart's own
// design, re-uploading the same part number just overwrites it. This is
// the single highest-value retry point in the whole client: a large
// upload over a real network is exactly where a transient blip is likely,
// and without this, one bad part means restarting the entire upload.
func putPart(ctx context.Context, url string, data []byte) (string, error) {
	var etag string
	err := withRetry(ctx, func() error {
		req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(data))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(data))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &apiError{Status: resp.StatusCode, Message: "upload part failed"}
		}
		etag = resp.Header.Get("ETag")
		if etag == "" {
			return errors.New("upload part response had no ETag header")
		}
		return nil
	})
	return etag, err
}

// Mkdir creates dir and any missing parents.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// Rmdir deletes dir if empty. The server refuses non-empty folders too, but
// with a 400; checking first gives rclone the error it expects.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}
	empty := true
	err = f.listDir(ctx, dirID, func(p *listPage) bool {
		empty = len(p.Folders)+len(p.Files) == 0
		return true
	})
	if err != nil {
		return err
	}
	if !empty {
		return fs.ErrorDirectoryNotEmpty
	}
	opts := rest.Opts{Method: "DELETE", Path: "/folders/" + dirID}
	if _, err := f.srv.CallJSON(ctx, &opts, nil, nil); err != nil {
		return err
	}
	f.dirCache.FlushDir(dir)
	return nil
}

// Move moves and/or renames a file: PATCH /files/:id/move then PATCH /files/:id.
//
// A same-folder rename onto an existing name is how an atomic editor save
// looks by the time it reaches us: write a temp file, then rename it over
// the original. rclone's own move helper (operations.Move) deletes the
// destination object first, then renames the temp file onto its name - so
// without help here, the original file's identity and version history
// would be silently replaced by the temp upload. Recover: if a file was
// JUST trashed under the destination name, treat this as "the temp content
// is a new version of the original" - restore it, upload as its next
// version, and discard the temp object - instead of letting the rename
// finish creating a fresh file with no history under the old name.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	if srcObj.id == "" { // syncSkip file (lock/swap/OS metadata) - never touches the API
		return f.newSkippedObject(remote), nil
	}
	srcLeaf, srcDirID, err := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	if err != nil {
		return nil, err
	}
	dstLeaf, dstDirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	if srcDirID == dstDirID {
		recovered, rErr := f.recoverFromAtomicSave(ctx, srcObj, dstDirID, dstLeaf, remote)
		if rErr != nil {
			return nil, rErr
		}
		if recovered != nil {
			return recovered, nil
		}
	}
	var file apiFile
	// updatedAt bumps on every write, so the revision guarding the second
	// PATCH (rename) must be the one the first PATCH (move) just returned,
	// not the value we started with - otherwise a real move always makes
	// the following rename look stale and 409 spuriously.
	rev := revisionOf(srcObj.modTime)
	if srcDirID != dstDirID {
		body := map[string]string{} // no folderId = root; the API rejects null
		if dstDirID != rootID {
			body["folderId"] = dstDirID
		}
		opts := rest.Opts{Method: "PATCH", Path: "/files/" + srcObj.id + "/move", ExtraHeaders: map[string]string{"If-Match": rev}}
		if _, err := f.srv.CallJSON(ctx, &opts, body, &file); err != nil {
			return nil, err
		}
		if !file.UpdatedAt.IsZero() {
			rev = revisionOf(file.UpdatedAt)
		}
	}
	if srcLeaf != dstLeaf {
		opts := rest.Opts{Method: "PATCH", Path: "/files/" + srcObj.id, ExtraHeaders: map[string]string{"If-Match": rev}}
		if _, err := f.srv.CallJSON(ctx, &opts, map[string]string{"name": enc.FromStandardName(dstLeaf)}, &file); err != nil {
			return nil, err
		}
	}
	dst := &Object{fs: f, remote: remote, id: srcObj.id, size: srcObj.size, modTime: srcObj.modTime}
	if !file.UpdatedAt.IsZero() {
		dst.modTime = file.UpdatedAt
	}
	return dst, nil
}

// trashRecoveryWindow bounds how recent a trashed file must be to count as
// "rclone just deleted this for us" rather than an unrelated earlier delete.
const trashRecoveryWindow = 30 * time.Second

// recoverFromAtomicSave checks whether a file named dstLeaf in dstDirID was
// trashed within trashRecoveryWindow - not our doing, but rclone's own
// delete-before-overwrite move helper - and if so restores it and uploads
// srcObj's content as its next version. Returns (nil, nil) when there's
// nothing to recover, so the caller falls through to an ordinary rename.
func (f *Fs) recoverFromAtomicSave(ctx context.Context, srcObj *Object, dstDirID, dstLeaf, remote string) (fs.Object, error) {
	var trash []struct {
		ID        string     `json:"id"`
		Name      string     `json:"name"`
		FolderID  *string    `json:"folderId"`
		DeletedAt *time.Time `json:"deletedAt"`
	}
	opts := rest.Opts{Method: "GET", Path: "/files/trash"}
	err := withRetry(ctx, func() error {
		_, err := f.srv.CallJSON(ctx, &opts, nil, &trash)
		return err
	})
	if err != nil {
		return nil, nil // best-effort: a lookup failure just falls back to an ordinary rename
	}
	wantFolder := dstDirID
	if wantFolder == rootID {
		wantFolder = ""
	}
	for _, t := range trash {
		folder := ""
		if t.FolderID != nil {
			folder = *t.FolderID
		}
		if enc.ToStandardName(t.Name) != dstLeaf || folder != wantFolder {
			continue
		}
		if t.DeletedAt == nil || time.Since(*t.DeletedAt) > trashRecoveryWindow {
			continue
		}
		return f.replaceWithVersion(ctx, t.ID, srcObj, remote)
	}
	return nil, nil
}

// replaceWithVersion restores originalID and uploads srcObj's content as its
// next version, then discards srcObj's own (now-superfluous) object.
func (f *Fs) replaceWithVersion(ctx context.Context, originalID string, srcObj *Object, remote string) (fs.Object, error) {
	restoreOpts := rest.Opts{Method: "PATCH", Path: "/files/" + originalID + "/restore"}
	if _, err := f.srv.CallJSON(ctx, &restoreOpts, nil, nil); err != nil {
		return nil, err
	}
	rc, err := srcObj.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// A fs.ObjectInfo with the DESTINATION remote, not srcObj's temp name -
	// content-type detection is by extension, and the temp name's is wrong.
	info := object.NewStaticObjectInfo(remote, srcObj.modTime, srcObj.size, true, nil, nil)
	var resp struct {
		File apiFile `json:"file"`
	}
	name := path.Base(remote)
	if err := f.upload(ctx, "/files/"+originalID+"/version", nil, enc.FromStandardName(name), rc, info, &resp, nil); err != nil {
		return nil, err
	}
	_ = srcObj.Remove(ctx) // best-effort cleanup of the now-orphaned temp object
	return f.newObject(remote, resp.File), nil
}

// DirMove renames a folder in place. The folders API has no re-parenting
// (PATCH /folders/:id takes only name), so cross-folder moves are refused.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}
	srcID, srcDirID, srcLeaf, dstDirID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	if srcDirID != dstDirID {
		// Re-parent via PATCH /folders/:id/move (backend PR #90) - previously
		// this branch just returned ErrorCantDirMove, which made rclone's VFS
		// fall back to moving every file inside the directory individually.
		body := map[string]string{} // no parentId = root; the API rejects null
		if dstDirID != rootID {
			body["parentId"] = dstDirID
		}
		opts := rest.Opts{Method: "PATCH", Path: "/folders/" + srcID + "/move"}
		if _, err := f.srv.CallJSON(ctx, &opts, body, nil); err != nil {
			return err
		}
		if dstLeaf != srcLeaf {
			opts := rest.Opts{Method: "PATCH", Path: "/folders/" + srcID}
			if _, err := f.srv.CallJSON(ctx, &opts, map[string]string{"name": enc.FromStandardName(dstLeaf)}, nil); err != nil {
				return err
			}
		}
	} else {
		opts := rest.Opts{Method: "PATCH", Path: "/folders/" + srcID}
		if _, err := f.srv.CallJSON(ctx, &opts, map[string]string{"name": enc.FromStandardName(dstLeaf)}, nil); err != nil {
			return err
		}
	}
	srcFs.dirCache.FlushDir(srcRemote)
	return nil
}

// About reports the account's real quota (drive free space in Explorer/Finder).
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var me struct {
		StorageUsed  string `json:"storageUsed"`
		StorageLimit string `json:"storageLimit"`
	}
	opts := rest.Opts{Method: "GET", Path: "/auth/me"}
	err := withRetry(ctx, func() error {
		_, err := f.srv.CallJSON(ctx, &opts, nil, &me)
		return err
	})
	if err != nil {
		return nil, err
	}
	used, _ := strconv.ParseInt(me.StorageUsed, 10, 64)
	total, _ := strconv.ParseInt(me.StorageLimit, 10, 64)
	free := max(total-used, 0)
	return &fs.Usage{Total: &total, Used: &used, Free: &free}, nil
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

// String describes the remote.
func (f *Fs) String() string { return "ZDrive root '" + f.root + "'" }

// Features of the remote.
func (f *Fs) Features() *fs.Features { return f.features }

// Precision of mod times (server-set updatedAt, ms).
func (f *Fs) Precision() time.Duration { return time.Millisecond }

// Hashes supported: none exposed by the API.
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.None) }

// DirCacheFlush drops cached path->ID mappings.
func (f *Fs) DirCacheFlush() { f.dirCache.ResetRoot() }

// changesPollInterval is how often pollChanges checks GET /drive/changes -
// replaces waiting out the VFS directory-cache TTL (rclone's own
// --dir-cache-time, default 5 minutes) for noticing a remote change made
// elsewhere (web app, another device, another desktop install).
const changesPollInterval = 30 * time.Second

type changesResponse struct {
	Files            []json.RawMessage `json:"files"`
	Folders          []json.RawMessage `json:"folders"`
	DeletedFileIDs   []string          `json:"deletedFileIds"`
	DeletedFolderIDs []string          `json:"deletedFolderIds"`
	NextCursor       string            `json:"nextCursor"`
}

// pollChanges runs for the lifetime of ctx (the one NewFs was called with -
// there's no explicit unmount hook on this mount path, rclone's own
// Shutdowner interface is only ever invoked by `serve docker`, not plain
// `mount`/`cmount` - so this just runs until the process exits, same as
// the mount itself does).
func (f *Fs) pollChanges(ctx context.Context) {
	ticker := time.NewTicker(changesPollInterval)
	defer ticker.Stop()
	var cursor string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cursor, _ = f.pollChangesOnce(ctx, cursor)
		}
	}
}

// pollChangesOnce does a single GET /drive/changes and, if anything came
// back, drops the whole local directory cache rather than surgically
// invalidating just the affected folders - reverse-engineering rclone's
// internal id->path cache for that isn't worth it for v1; the cost (a few
// extra relist calls right after a real change) is small. Split out from
// pollChanges so a test can call it directly without waiting on a real
// ticker. Returns the cursor to use next time (unchanged on a request
// error - a transient failure shouldn't skip ahead and miss whatever
// happened during it) and whether a flush happened (so a test can assert
// on this method's own branch decision directly, rather than needing to
// prove rclone's own, already-used dircache.ResetRoot() took effect).
func (f *Fs) pollChangesOnce(ctx context.Context, cursor string) (nextCursor string, flushed bool) {
	params := url.Values{}
	if cursor != "" {
		params.Set("cursor", cursor)
	}
	var resp changesResponse
	opts := rest.Opts{Method: "GET", Path: "/drive/changes", Parameters: params}
	if _, err := f.srv.CallJSON(ctx, &opts, nil, &resp); err != nil {
		fs.Debugf(f, "poll changes: %v", err)
		return cursor, false
	}
	if len(resp.Files) > 0 || len(resp.Folders) > 0 ||
		len(resp.DeletedFileIDs) > 0 || len(resp.DeletedFolderIDs) > 0 {
		f.DirCacheFlush()
		flushed = true
	}
	return resp.NextCursor, flushed
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.fs }

// String describes the object.
func (o *Object) String() string { return o.remote }

// Remote path.
func (o *Object) Remote() string { return o.remote }

// ID of the file.
func (o *Object) ID() string { return o.id }

// Size in bytes.
func (o *Object) Size() int64 { return o.size }

// ModTime is the file's last server-side change.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

// Storable reports the object can be stored.
func (o *Object) Storable() bool { return true }

// Hash is unsupported.
func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// SetModTime is unsupported: updatedAt is server-controlled.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error { return fs.ErrorCantSetModTime }

// Open streams the file, honouring Range/Seek options via GET /files/:id/stream.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	if o.id == "" { // syncSkip file - never existed remotely
		return io.NopCloser(strings.NewReader("")), nil
	}
	fs.FixRangeOption(options, o.size)
	opts := rest.Opts{Method: "GET", Path: "/files/" + o.id + "/stream", Options: options}
	var resp *http.Response
	err := withRetry(ctx, func() error {
		var err error
		resp, err = o.fs.srv.Call(ctx, &opts)
		return err
	})
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// conflictedCopyName mirrors the "name (conflicted copy <host> <date>).ext"
// convention other sync clients use for a keep-both resolution.
func conflictedCopyName(name string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	return fmt.Sprintf("%s (conflicted copy %s %s)%s", base, host, time.Now().Format("2006-01-02"), ext)
}

// dirOfRemote normalizes path.Dir's "." (no directory component) to "",
// matching how this package's dircache/API calls represent the root.
func dirOfRemote(remote string) string {
	dir := path.Dir(remote)
	if dir == "." {
		return ""
	}
	return dir
}

// Update uploads new content as the next version - a single POST (small
// files) or the presigned-multipart flow via updateLarge (B6, files
// crossing largeUploadMinBytes, mirrors Put's own split).
//
// If-Match guards against a stale write (B3): a 409 means the remote file
// changed since this object was last read, so the edit is kept as a new
// sibling file ("keep both") instead of silently overwriting a change this
// client never saw.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	if o.id == "" { // syncSkip file - stays local-only
		_, _ = io.Copy(io.Discard, in)
		return nil
	}

	err := o.updateOnce(ctx, in, src)
	if err == nil || !isConflict(err) {
		return err
	}

	seeker, ok := in.(io.Seeker)
	if !ok {
		// ponytail: can't safely retry the write without a re-readable body
		// (already consumed by the failed attempt above); surface the 409
		// so the VFS keeps the local edit queued instead of silently
		// dropping it. Add a buffering fallback if this proves common.
		return err
	}
	if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
		return err
	}
	return o.keepBothCopy(ctx, in, src)
}

// updateOnce makes one attempt to upload new content as the next version -
// large (updateLarge) or the simple single POST, chosen by size like Put.
// If updateLarge declines (in isn't seekable), falls through to the simple
// path, which works fine for a first attempt without seeking.
func (o *Object) updateOnce(ctx context.Context, in io.Reader, src fs.ObjectInfo) error {
	if src.Size() >= largeUploadMinBytes {
		ok, err := o.updateLarge(ctx, in, src)
		if ok {
			return err
		}
	}
	var resp struct {
		File apiFile `json:"file"`
	}
	headers := map[string]string{"If-Match": revisionOf(o.modTime)}
	err := o.fs.upload(ctx, "/files/"+o.id+"/version", nil, enc.FromStandardName(path.Base(o.remote)), in, src, &resp, headers)
	if err != nil {
		return err
	}
	*o = *o.fs.newObject(o.remote, resp.File)
	return nil
}

// updateLarge is the version-upload equivalent of putLarge: presigned
// multipart via POST /files/:id/version/initiate + /complete instead of
// buffering the whole edit through a single request. Same bool-return
// contract as putLarge (false only when in isn't seekable).
func (o *Object) updateLarge(ctx context.Context, in io.Reader, src fs.ObjectInfo) (bool, error) {
	contentHash, ok, err := hashAndRewind(in)
	if !ok {
		return false, nil
	}
	if err != nil {
		return true, err
	}

	body := map[string]any{
		"name":        enc.FromStandardName(path.Base(o.remote)),
		"size":        src.Size(),
		"mimeType":    fs.MimeType(ctx, src),
		"contentHash": contentHash,
	}
	var init initiateUploadResp
	initOpts := rest.Opts{
		Method:       "POST",
		Path:         "/files/" + o.id + "/version/initiate",
		ExtraHeaders: map[string]string{"If-Match": revisionOf(o.modTime)},
	}
	if _, err := o.fs.srv.CallJSON(ctx, &initOpts, body, &init); err != nil {
		return true, err
	}

	completedParts, err := uploadParts(ctx, in, &init)
	if err != nil {
		return true, err
	}

	// Flat apiFile, not {"file": ...} - completeLargeVersionUpload mirrors
	// completeLargeUpload's (new-file) response shape, not the simple
	// POST /files/:id/version endpoint's wrapped one.
	var file apiFile
	completeOpts := rest.Opts{Method: "POST", Path: "/files/" + o.id + "/version/complete"}
	completeBody := map[string]any{"uploadId": init.UploadID, "parts": completedParts}
	// Safe to retry, same reasoning as putLarge's complete step: a lost
	// response after a real completion leaves the PendingUpload row
	// COMPLETED, so a retry cleanly fails instead of double-applying.
	err = withRetry(ctx, func() error {
		_, err := o.fs.srv.CallJSON(ctx, &completeOpts, completeBody, &file)
		return err
	})
	if err != nil {
		return true, err
	}
	*o = *o.fs.newObject(o.remote, file)
	return true, nil
}

// keepBothCopy uploads in (already rewound to the start by the caller) as a
// new sibling file instead of the version Update was trying to write - the
// B3 "keep both" conflict resolution. Small or large upload chosen by size,
// mirroring Put's own split; the new object's own identity is discarded
// (Update's contract is "did the version write succeed", not "here's the
// copy" - a later listing picks it up naturally).
func (o *Object) keepBothCopy(ctx context.Context, in io.Reader, src fs.ObjectInfo) error {
	leaf, dirID, fErr := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if fErr != nil {
		return fErr
	}
	name := conflictedCopyName(leaf)

	if src.Size() >= largeUploadMinBytes {
		copyRemote := path.Join(dirOfRemote(o.remote), name)
		copyInfo := object.NewStaticObjectInfo(copyRemote, src.ModTime(ctx), src.Size(), true, nil, nil)
		if _, ok, err := o.fs.putLarge(ctx, in, copyInfo, name, dirID); ok {
			if err != nil {
				return err
			}
			o.fs.dirCache.FlushDir(dirOfRemote(o.remote))
			return nil
		}
		// not seekable - but the caller already proved in is seekable
		// (that's why we're here); defensive fallback only.
	}

	params := url.Values{}
	if dirID != rootID {
		params.Set("folderId", dirID)
	}
	var copyResp apiFile
	if uErr := o.fs.upload(ctx, "/storage/upload", params, enc.FromStandardName(name), in, src, &copyResp, nil); uErr != nil {
		return uErr
	}
	o.fs.dirCache.FlushDir(dirOfRemote(o.remote))
	return nil
}

// Remove moves the file to trash (DELETE /files/:id is a soft delete).
func (o *Object) Remove(ctx context.Context) error {
	if o.id == "" { // syncSkip file - nothing remote to remove
		return nil
	}
	opts := rest.Opts{Method: "DELETE", Path: "/files/" + o.id}
	_, err := o.fs.srv.CallJSON(ctx, &opts, nil, nil)
	return err
}

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.DirMover        = (*Fs)(nil)
	_ dircache.DirCacher = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
)
