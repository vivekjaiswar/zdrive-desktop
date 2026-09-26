// Package zdrive is an rclone backend for the ZDrive API. rclone supplies
// the mount, VFS cache and eviction; this file only maps paths to ZDrive
// folder/file IDs, reads content by byte range and writes via the web API.
//
// ponytail: last writer wins. Add If-Match on version/rename/move with B3.
package zdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/rest"
)

const rootID = "root"

// ZDrive names may contain anything; encode only what can't be a path segment.
const enc = encoder.Standard

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
// into result. Streams: nothing is buffered client-side.
func (f *Fs) upload(ctx context.Context, apiPath string, params url.Values, name string, in io.Reader, src fs.ObjectInfo, result any) error {
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
	opts := rest.Opts{Method: "POST", Path: apiPath, Body: pr, ContentType: mw.FormDataContentType()}
	_, err := f.srv.CallJSON(ctx, &opts, nil, result)
	return err
}

// Put uploads a new file via POST /storage/upload.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	leaf, dirID, err := f.dirCache.FindPath(ctx, src.Remote(), true)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if dirID != rootID {
		params.Set("folderId", dirID)
	}
	var file apiFile
	if err := f.upload(ctx, "/storage/upload", params, enc.FromStandardName(leaf), in, src, &file); err != nil {
		return nil, err
	}
	// ponytail: upload reply has no updatedAt; now is within ms of it, and the
	// next listing corrects it (costs at most one cache re-fetch).
	file.UpdatedAt = time.Now()
	return f.newObject(src.Remote(), file), nil
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
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}
	srcLeaf, srcDirID, err := srcObj.fs.dirCache.FindPath(ctx, srcObj.remote, false)
	if err != nil {
		return nil, err
	}
	dstLeaf, dstDirID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}
	var file apiFile
	if srcDirID != dstDirID {
		body := map[string]string{} // no folderId = root; the API rejects null
		if dstDirID != rootID {
			body["folderId"] = dstDirID
		}
		opts := rest.Opts{Method: "PATCH", Path: "/files/" + srcObj.id + "/move"}
		if _, err := f.srv.CallJSON(ctx, &opts, body, &file); err != nil {
			return nil, err
		}
	}
	if srcLeaf != dstLeaf {
		opts := rest.Opts{Method: "PATCH", Path: "/files/" + srcObj.id}
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
	fs.FixRangeOption(options, o.size)
	opts := rest.Opts{Method: "GET", Path: "/files/" + o.id + "/stream", Options: options}
	resp, err := o.fs.srv.Call(ctx, &opts)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Update uploads new content as a version via POST /files/:id/version. The
// server renames the file to the upload's filename, so send the current leaf.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	var resp struct {
		File apiFile `json:"file"`
	}
	if err := o.fs.upload(ctx, "/files/"+o.id+"/version", nil, enc.FromStandardName(path.Base(o.remote)), in, src, &resp); err != nil {
		return err
	}
	*o = *o.fs.newObject(o.remote, resp.File)
	return nil
}

// Remove moves the file to trash (DELETE /files/:id is a soft delete).
func (o *Object) Remove(ctx context.Context) error {
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
