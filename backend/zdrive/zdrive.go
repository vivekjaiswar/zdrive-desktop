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

// Must match the backend's LARGE_UPLOAD_MIN_SIZE_BYTES / MULTIPART_PART_SIZE_BYTES
// defaults (storage.service.ts) - the server computes exactly
// ceil(size/multipartPartBytes) presigned part URLs, so the client's part
// size has to match, not just be "close enough".
const (
	largeUploadMinBytes = 50 * 1024 * 1024
	multipartPartBytes  = 16 * 1024 * 1024
)

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
		if _, err := f.srv.CallJSON(ctx, &opts, nil, &page); err != nil {
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

// putLarge implements B6: uploads via POST /storage/upload/initiate, then
// PUTs each part directly to presigned storage URLs (the file never
// round-trips through the API server's own request body), then finalizes
// via POST /storage/upload/complete. The bool return is whether putLarge
// actually ran (false only when in isn't seekable, so the caller knows not
// to also try the fallback path against an already-drained reader).
func (f *Fs) putLarge(ctx context.Context, in io.Reader, src fs.ObjectInfo, leaf, dirID string) (fs.Object, bool, error) {
	seeker, ok := in.(io.Seeker)
	if !ok {
		return nil, false, nil
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, in); err != nil {
		return nil, true, err
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return nil, true, err
	}
	contentHash := hex.EncodeToString(hasher.Sum(nil))

	body := map[string]any{
		"name":        enc.FromStandardName(leaf),
		"size":        src.Size(),
		"mimeType":    fs.MimeType(ctx, src),
		"contentHash": contentHash,
	}
	if dirID != rootID {
		body["folderId"] = dirID
	}
	var init struct {
		UploadID string `json:"uploadId"`
		Parts    []struct {
			PartNumber int    `json:"partNumber"`
			URL        string `json:"url"`
		} `json:"parts"`
	}
	initOpts := rest.Opts{Method: "POST", Path: "/storage/upload/initiate"}
	if _, err := f.srv.CallJSON(ctx, &initOpts, body, &init); err != nil {
		return nil, true, err
	}

	completedParts := make([]map[string]any, 0, len(init.Parts))
	buf := make([]byte, multipartPartBytes)
	for _, part := range init.Parts {
		n, err := io.ReadFull(in, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, true, err
		}
		etag, err := putPart(ctx, part.URL, buf[:n])
		if err != nil {
			return nil, true, err
		}
		completedParts = append(completedParts, map[string]any{"partNumber": part.PartNumber, "etag": etag})
	}

	var file apiFile
	completeOpts := rest.Opts{Method: "POST", Path: "/storage/upload/complete"}
	completeBody := map[string]any{"uploadId": init.UploadID, "parts": completedParts}
	if _, err := f.srv.CallJSON(ctx, &completeOpts, completeBody, &file); err != nil {
		return nil, true, err
	}
	file.UpdatedAt = time.Now() // ponytail: complete's reply has no updatedAt either, same as the simple path
	return f.newObject(src.Remote(), file), true, nil
}

// putPart PUTs one part directly to a presigned storage URL (bypassing
// f.srv - these URLs carry their own auth, not our Bearer token) and
// returns the ETag the multipart complete step needs to identify it.
func putPart(ctx context.Context, url string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.ContentLength = int64(len(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("upload part failed: status %d", resp.StatusCode)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		return "", errors.New("upload part response had no ETag header")
	}
	return etag, nil
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
	if _, err := f.srv.CallJSON(ctx, &opts, nil, &trash); err != nil {
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
	srcID, srcDirID, _, dstDirID, dstLeaf, err := f.dirCache.DirMove(ctx, srcFs.dirCache, srcFs.root, srcRemote, f.root, dstRemote)
	if err != nil {
		return err
	}
	if srcDirID != dstDirID {
		return fs.ErrorCantDirMove
	}
	opts := rest.Opts{Method: "PATCH", Path: "/folders/" + srcID}
	if _, err := f.srv.CallJSON(ctx, &opts, map[string]string{"name": enc.FromStandardName(dstLeaf)}, nil); err != nil {
		return err
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
	if _, err := f.srv.CallJSON(ctx, &opts, nil, &me); err != nil {
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
	resp, err := o.fs.srv.Call(ctx, &opts)
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

// Update uploads new content as a version via POST /files/:id/version. The
// server renames the file to the upload's filename, so send the current leaf.
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
	var resp struct {
		File apiFile `json:"file"`
	}
	headers := map[string]string{"If-Match": revisionOf(o.modTime)}
	err := o.fs.upload(ctx, "/files/"+o.id+"/version", nil, enc.FromStandardName(path.Base(o.remote)), in, src, &resp, headers)
	if err == nil {
		*o = *o.fs.newObject(o.remote, resp.File)
		return nil
	}
	if !isConflict(err) {
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
	leaf, dirID, fErr := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if fErr != nil {
		return err
	}
	params := url.Values{}
	if dirID != rootID {
		params.Set("folderId", dirID)
	}
	var copyResp apiFile
	name := conflictedCopyName(leaf)
	if uErr := o.fs.upload(ctx, "/storage/upload", params, enc.FromStandardName(name), in, src, &copyResp, nil); uErr != nil {
		return uErr
	}
	dir := path.Dir(o.remote)
	if dir == "." {
		dir = ""
	}
	o.fs.dirCache.FlushDir(dir)
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
