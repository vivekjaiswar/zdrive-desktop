// Package zdrive is an rclone backend for the ZDrive API. rclone supplies
// the mount, VFS cache and eviction; this file only maps paths to ZDrive
// folder/file IDs and reads content by byte range.
//
// ponytail: read-only (M1). Put/Mkdir/Rmdir/Remove land in M2 with If-Match.
package zdrive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

var errReadOnly = errors.New("zdrive: drive is read-only in this build")

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
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      string    `json:"size"` // bigint as string
	UpdatedAt time.Time `json:"updatedAt"`
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
	return "", errReadOnly
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
	size, _ := strconv.ParseInt(file.Size, 10, 64)
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

// Put is not supported yet.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return nil, errReadOnly
}

// Mkdir is not supported yet.
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return errReadOnly }

// Rmdir is not supported yet.
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return errReadOnly }

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

// Update is not supported yet.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return errReadOnly
}

// Remove is not supported yet.
func (o *Object) Remove(ctx context.Context) error { return errReadOnly }

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.DirCacheFlusher = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ dircache.DirCacher = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
)
