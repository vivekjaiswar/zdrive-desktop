package zdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/rest"
)

// Fake API: root has folder "Docs" (on page 1) and file "a.txt" (on page 2,
// behind a cursor); Docs holds "b.txt". Stream honours Range.
func TestListAndRangeRead(t *testing.T) {
	const content = "0123456789abcdefghij"
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Query().Get("folderId") + "|" + r.URL.Query().Get("cursor") {
		case "|":
			io.WriteString(w, `{"folders":[{"id":"d1","name":"Docs","updatedAt":"2026-09-26T10:00:00Z"}],"files":[],"nextCursor":"p2"}`)
		case "|p2":
			io.WriteString(w, `{"folders":[],"files":[{"id":"f1","name":"a.txt","size":"20","updatedAt":"2026-09-26T10:00:00Z"}],"nextCursor":null}`)
		case "d1|":
			io.WriteString(w, `{"folders":[],"files":[{"id":"f2","name":"b.txt","size":"20","updatedAt":"2026-09-26T10:00:00Z"}],"nextCursor":null}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Folder not found"}`)
		}
	})
	mux.HandleFunc("/auth/me", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"storageUsed":"300","storageLimit":"1000"}`)
	})
	mux.HandleFunc("/files/f2/stream", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "b.txt", time.Time{}, strings.NewReader(content))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}

	entries, err := f.List(ctx, "")
	if err != nil || len(entries) != 2 || entries[0].Remote() != "Docs" || entries[1].Remote() != "a.txt" {
		t.Fatalf("root listing across pages = %v, %v", entries, err)
	}

	o, err := f.NewObject(ctx, "Docs/b.txt")
	if err != nil || o.Size() != 20 {
		t.Fatalf("NewObject = %v, %v", o, err)
	}
	if _, err := f.NewObject(ctx, "Nope/x"); err != fs.ErrorObjectNotFound {
		t.Fatalf("missing path err = %v", err)
	}

	u, err := f.Features().About(ctx)
	if err != nil || *u.Total != 1000 || *u.Used != 300 || *u.Free != 700 {
		t.Fatalf("About = %+v, %v", u, err)
	}

	rc, err := o.Open(ctx, &fs.RangeOption{Start: 5, End: 9})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "56789" {
		t.Fatalf("range read = %q", got)
	}
}

// Fake API that logs every write call and keeps just enough listing state.
func TestWrites(t *testing.T) {
	var calls []string
	rootFolders, docsFiles := `[]`, `[]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
			fh := r.MultipartForm.File["file"][0]
			part, _ := fh.Open()
			body, _ := io.ReadAll(part)
			ct, _, _ := strings.Cut(fh.Header.Get("Content-Type"), ";")
			call += fmt.Sprintf(" folderId=%q %s:%s:%s", r.FormValue("folderId"), fh.Filename, body, ct)
		} else if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			call += " " + string(body)
		}
		if r.Method != "GET" {
			calls = append(calls, call)
		}
		switch r.URL.Path {
		case "/drive/list":
			folders, files := rootFolders, `[]`
			if r.URL.Query().Get("folderId") == "d1" {
				folders, files = `[]`, docsFiles
			}
			io.WriteString(w, `{"folders":`+folders+`,"files":`+files+`,"nextCursor":null}`)
		case "/folders":
			io.WriteString(w, `{"id":"d1","name":"Docs"}`)
		case "/folders/d1":
			rootFolders = `[{"id":"d1","name":"Papers"}]`
			io.WriteString(w, `{"id":"d1","name":"Papers"}`)
		case "/storage/upload":
			docsFiles = `[{"id":"f1","name":"résumé.txt","size":"5","updatedAt":"2026-09-26T10:00:00Z"}]`
			io.WriteString(w, `{"id":"f1","name":"résumé.txt","size":5,"folderId":"d1"}`)
		case "/files/f1/version":
			io.WriteString(w, `{"success":true,"file":{"id":"f1","name":"résumé.txt","size":"7","updatedAt":"2026-09-26T11:00:00Z"}}`)
		case "/files/f1/move":
			docsFiles = `[]`
			io.WriteString(w, `{"id":"f1","size":"7","updatedAt":"2026-09-26T12:00:00Z"}`)
		default:
			io.WriteString(w, `{"id":"f1","size":"7","updatedAt":"2026-09-26T12:00:01Z"}`)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Mkdir(ctx, "Docs"); err != nil {
		t.Fatal(err)
	}
	src := object.NewStaticObjectInfo("Docs/résumé.txt", time.Now(), 5, true, nil, nil)
	o, err := f.Put(ctx, strings.NewReader("hello"), src)
	if err != nil || o.Size() != 5 {
		t.Fatalf("Put = %v, %v", o, err)
	}
	if err := o.Update(ctx, strings.NewReader("hello 2"), object.NewStaticObjectInfo("Docs/résumé.txt", time.Now(), 7, true, nil, nil)); err != nil || o.Size() != 7 {
		t.Fatalf("Update = %v, size %d", err, o.Size())
	}
	if err := f.Rmdir(ctx, "Docs"); err != fs.ErrorDirectoryNotEmpty {
		t.Fatalf("Rmdir non-empty = %v", err)
	}
	o, err = f.Features().Move(ctx, o, "b.txt")
	if err != nil || o.Remote() != "b.txt" || o.ModTime(ctx).Hour() != 12 {
		t.Fatalf("Move = %v, %v", o, err)
	}
	if err := o.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.Features().DirMove(ctx, f, "Docs", "Papers"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(ctx, "Papers"); err != nil {
		t.Fatal(err)
	}

	want := []string{
		`POST /folders {"name":"Docs"}`,
		`POST /storage/upload folderId="d1" résumé.txt:hello:text/plain`,
		`POST /files/f1/version folderId="" résumé.txt:hello 2:text/plain`,
		`PATCH /files/f1/move {}`,
		`PATCH /files/f1 {"name":"b.txt"}`,
		`DELETE /files/f1 `,
		`PATCH /folders/d1 {"name":"Papers"}`,
		`DELETE /folders/d1 `,
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// dirMoveFixture starts a fake server with two root folders - "Docs" (d1,
// the one being moved) and "Archive" (d2, the destination parent) - and a
// fresh Fs, for a single DirMove call. Each test gets its own instance
// rather than chaining several moves against one long-lived Fs: dircache
// negatively caches a directory's listing the moment DirMove's own
// pre-check confirms the destination doesn't exist yet, and never
// invalidates that entry after the real move happens server-side - a
// rclone-library quirk every backend using dircache.DirMove shares (Box's
// own DirMove, for one, only ever flushes the source path too), not
// something to work around here. One clean move per Fs sidesteps it
// entirely while still proving this code sends the right API calls.
func dirMoveFixture(t *testing.T) (fs.Fs, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := r.Method + " " + r.URL.Path
		if r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			if len(body) > 0 {
				call += " " + string(body)
			}
		}
		if r.Method != "GET" {
			calls = append(calls, call)
		}
		switch {
		case r.URL.Path == "/drive/list":
			var folders string
			switch r.URL.Query().Get("folderId") {
			case "":
				folders = `[{"id":"d1","name":"Docs"},{"id":"d2","name":"Archive"}]`
			case "d2":
				folders = `[{"id":"d3","name":"Nested"}]`
			default:
				folders = `[]`
			}
			io.WriteString(w, `{"folders":`+folders+`,"files":[],"nextCursor":null}`)
		case strings.HasPrefix(r.URL.Path, "/folders/d1") || strings.HasPrefix(r.URL.Path, "/folders/d3"):
			io.WriteString(w, `{"id":"d1"}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"not found"}`)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	return f, &calls
}

// TestDirMoveCrossParent: a plain re-parent (same name) now goes through
// PATCH /folders/:id/move (backend PR #90) in one call, instead of the old
// ErrorCantDirMove that made rclone's VFS fall back to moving every file
// inside the directory individually.
func TestDirMoveCrossParent(t *testing.T) {
	f, calls := dirMoveFixture(t)
	ctx := context.Background()

	if err := f.Features().DirMove(ctx, f, "Docs", "Archive/Docs"); err != nil {
		t.Fatalf("DirMove = %v", err)
	}

	want := []string{`PATCH /folders/d1/move {"parentId":"d2"}`}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestDirMoveCrossParentAndRename: a re-parent that also changes the name
// fires both the move and the rename PATCH.
func TestDirMoveCrossParentAndRename(t *testing.T) {
	f, calls := dirMoveFixture(t)
	ctx := context.Background()

	if err := f.Features().DirMove(ctx, f, "Docs", "Archive/Renamed"); err != nil {
		t.Fatalf("DirMove = %v", err)
	}

	want := []string{
		`PATCH /folders/d1/move {"parentId":"d2"}`,
		`PATCH /folders/d1 {"name":"Renamed"}`,
	}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestDirMoveToRoot: moving a nested folder back to root sends an empty
// body (no parentId key) - the API rejects a literal null, same convention
// files' own Move() already follows.
func TestDirMoveToRoot(t *testing.T) {
	f, calls := dirMoveFixture(t)
	ctx := context.Background()

	if err := f.Features().DirMove(ctx, f, "Archive/Nested", "Final"); err != nil {
		t.Fatalf("DirMove = %v", err)
	}

	want := []string{
		`PATCH /folders/d3/move {}`,
		`PATCH /folders/d3 {"name":"Final"}`,
	}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(*calls, "\n"), strings.Join(want, "\n"))
	}
}

// TestUpdateConflict: a stale If-Match on a version upload gets a 409, and
// the edit is kept as a new sibling file (B3's "keep both") instead of
// being dropped or overwriting a remote change this client never saw.
func TestUpdateConflict(t *testing.T) {
	var ifMatches []string
	var copyUploaded bool
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("folderId") == "" {
			io.WriteString(w, `{"folders":[],"files":[{"id":"f1","name":"a.txt","size":"5","updatedAt":"2026-09-26T10:00:00Z"}],"nextCursor":null}`)
			return
		}
		io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`)
	})
	mux.HandleFunc("/files/f1/version", func(w http.ResponseWriter, r *http.Request) {
		ifMatches = append(ifMatches, r.Header.Get("If-Match"))
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"message":"File has changed since it was last read"}`)
	})
	mux.HandleFunc("/storage/upload", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		fh := r.MultipartForm.File["file"][0]
		part, _ := fh.Open()
		body, _ := io.ReadAll(part)
		if string(body) != "new content" {
			t.Fatalf("conflicted copy body = %q", body)
		}
		if !strings.Contains(fh.Filename, "(conflicted copy ") || !strings.HasSuffix(fh.Filename, ".txt") {
			t.Fatalf("conflicted copy name = %q", fh.Filename)
		}
		copyUploaded = true
		io.WriteString(w, `{"id":"f2","name":"`+fh.Filename+`","size":11,"folderId":null}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "a.txt")
	if err != nil {
		t.Fatal(err)
	}

	err = o.Update(ctx, strings.NewReader("new content"), object.NewStaticObjectInfo("a.txt", time.Now(), 11, true, nil, nil))
	if err != nil {
		t.Fatalf("Update should resolve the conflict, not fail: %v", err)
	}
	if len(ifMatches) != 1 || ifMatches[0] != "1790416800000" {
		t.Fatalf("If-Match sent = %v, want the object's revision", ifMatches)
	}
	if !copyUploaded {
		t.Fatal("conflicted copy was never uploaded")
	}
}

// TestSyncSkip: editor lock files and OS metadata never reach the API - the
// object is a purely local placeholder, and Update/Remove on it are no-ops.
func TestSyncSkip(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"~$resume.docx", ".~lock.notes.odt#", "vim.txt.swp", ".DS_Store", "Thumbs.db"} {
		o, err := f.Put(ctx, strings.NewReader("junk"), object.NewStaticObjectInfo(name, time.Now(), 4, true, nil, nil))
		if err != nil {
			t.Fatalf("Put(%q) = %v", name, err)
		}
		// Regression: newSkippedObject used to default to size 0 regardless
		// of the real local file, which rclone's own vfscache writeback
		// then flagged as "corrupted on transfer" and retried forever.
		if o.Size() != 4 {
			t.Fatalf("Put(%q).Size() = %d, want 4 (matching the real local file)", name, o.Size())
		}

		moved, err := f.Features().Move(ctx, o, "moved-"+name)
		if err != nil {
			t.Fatalf("Move(%q) = %v", name, err)
		}
		if moved.Size() != 4 {
			t.Fatalf("Move(%q).Size() = %d, want 4 (carried over from the source)", name, moved.Size())
		}
		mo := moved.(*Object)

		if err := mo.Update(ctx, strings.NewReader("more junk"), object.NewStaticObjectInfo(name, time.Now(), 9, true, nil, nil)); err != nil {
			t.Fatalf("Update(%q) = %v", name, err)
		}
		if mo.Size() != 9 {
			t.Fatalf("Update(%q).Size() = %d, want 9 (matching the rewritten local file)", name, mo.Size())
		}
		if err := mo.Remove(ctx); err != nil {
			t.Fatalf("Remove(%q) = %v", name, err)
		}
	}
	if hit {
		t.Fatal("a syncSkip file reached the API")
	}
}

// TestMoveRecoversAtomicSave: an editor's atomic save (write temp, rename
// over the original) must become a new version of the original file, not a
// trashed original plus a fresh file wearing its name. This replays exactly
// what rclone's own move helper does: delete the destination, then call our
// Move - Remove() here stands in for that delete.
func TestMoveRecoversAtomicSave(t *testing.T) {
	type rec struct {
		Name      string
		FolderID  *string
		DeletedAt *time.Time
		Content   string
	}
	files := map[string]*rec{}
	nextID := 0
	var calls []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/drive/list":
			io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`)
		case r.URL.Path == "/storage/upload":
			r.ParseMultipartForm(1 << 20)
			fh := r.MultipartForm.File["file"][0]
			part, _ := fh.Open()
			body, _ := io.ReadAll(part)
			nextID++
			id := fmt.Sprintf("f%d", nextID)
			files[id] = &rec{Name: fh.Filename, Content: string(body)}
			fmt.Fprintf(w, `{"id":%q,"name":%q,"size":%d}`, id, fh.Filename, len(body))
		case strings.HasSuffix(r.URL.Path, "/stream"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/stream")
			io.WriteString(w, files[id].Content)
		case strings.HasSuffix(r.URL.Path, "/version"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/version")
			r.ParseMultipartForm(1 << 20)
			fh := r.MultipartForm.File["file"][0]
			part, _ := fh.Open()
			body, _ := io.ReadAll(part)
			files[id].Content = string(body)
			files[id].Name = fh.Filename
			fmt.Fprintf(w, `{"file":{"id":%q,"name":%q,"size":%d,"updatedAt":"2026-09-28T12:00:00Z"}}`, id, fh.Filename, len(body))
		case strings.HasSuffix(r.URL.Path, "/restore"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/restore")
			files[id].DeletedAt = nil
		case r.URL.Path == "/files/trash":
			io.WriteString(w, `[`)
			first := true
			for id, rc := range files {
				if rc.DeletedAt == nil {
					continue
				}
				if !first {
					io.WriteString(w, ",")
				}
				first = false
				fmt.Fprintf(w, `{"id":%q,"name":%q,"folderId":null,"deletedAt":%q}`, id, rc.Name, rc.DeletedAt.Format(time.RFC3339))
			}
			io.WriteString(w, `]`)
		case r.Method == "DELETE":
			id := strings.TrimPrefix(r.URL.Path, "/files/")
			now := time.Now()
			files[id].DeletedAt = &now
			io.WriteString(w, `{}`)
		default:
			io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}

	orig, err := f.Put(ctx, strings.NewReader("v1"), object.NewStaticObjectInfo("resume.txt", time.Now(), 2, true, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	origID := orig.(*Object).id

	if err := orig.Remove(ctx); err != nil { // stands in for operations.Move's delete-before-overwrite
		t.Fatal(err)
	}

	tmp, err := f.Put(ctx, strings.NewReader("v2 edited"), object.NewStaticObjectInfo("resume.tmp", time.Now(), 9, true, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	tmpID := tmp.(*Object).id

	result, err := f.Features().Move(ctx, tmp, "resume.txt")
	if err != nil {
		t.Fatalf("Move = %v", err)
	}
	if result.(*Object).id != origID {
		t.Fatalf("Move result id = %q, want the original's id %q (identity/version history must survive)", result.(*Object).id, origID)
	}
	if result.Remote() != "resume.txt" {
		t.Fatalf("Move result remote = %q", result.Remote())
	}
	if files[origID].Content != "v2 edited" {
		t.Fatalf("original's content = %q, want the edited content", files[origID].Content)
	}
	if files[origID].DeletedAt != nil {
		t.Fatal("original should have been restored, not left trashed")
	}
	if files[tmpID].DeletedAt == nil {
		t.Fatal("the leftover temp object should have been discarded")
	}
}

// TestPutLarge: the presigned-multipart flow (B6) - initiate, PUT the part
// directly to the presigned URL (not through the API server), complete.
// Calls putLarge directly rather than through Put() so the test payload
// doesn't need to cross the real 50MB/16MB thresholds to exercise it - the
// server's response shape is what's under test, not the byte counts.
func TestPutLarge(t *testing.T) {
	const content = "large file content"
	var calls []string
	var partBody []byte

	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/storage/upload/initiate", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "initiate")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"contentHash"`) {
			t.Fatalf("initiate body missing contentHash: %s", body)
		}
		io.WriteString(w, `{"uploadId":"up1","key":"k1","partSize":16777216,"parts":[{"partNumber":1,"url":"`+srvURL+`/fake-s3-part"}]}`)
	})
	mux.HandleFunc("/fake-s3-part", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "part-put")
		partBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"part1etag"`)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/storage/upload/complete", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "complete")
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `part1etag`) {
			t.Fatalf("complete body missing the part's etag: %s", body)
		}
		io.WriteString(w, `{"id":"f-large","name":"video.mp4","size":19,"folderId":null}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}

	obj, ok, err := f.(*Fs).putLarge(ctx, strings.NewReader(content), object.NewStaticObjectInfo("video.mp4", time.Now(), int64(len(content)), true, nil, nil), "video.mp4", rootID)
	if !ok {
		t.Fatal("putLarge returned ok=false for a seekable reader")
	}
	if err != nil {
		t.Fatalf("putLarge = %v", err)
	}
	if obj.(*Object).id != "f-large" {
		t.Fatalf("result id = %q", obj.(*Object).id)
	}
	if string(partBody) != content {
		t.Fatalf("uploaded part body = %q, want %q", partBody, content)
	}
	want := []string{"initiate", "part-put", "complete"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// TestClientVersionHeader: B5's kill switch depends entirely on this header
// actually reaching the server - confirm it's sent, not just wired up.
func TestClientVersionHeader(t *testing.T) {
	var gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-ZDrive-Client-Version")
		io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if gotVersion != ClientVersion {
		t.Fatalf("X-ZDrive-Client-Version = %q, want %q", gotVersion, ClientVersion)
	}
}

// TestUpdateLarge: a version upload declared >= largeUploadMinBytes goes
// through the presigned-multipart flow (initiate/PUT-part/complete against
// /files/:id/version/...) instead of the single-POST path, and still sends
// If-Match (B3).
func TestUpdateLarge(t *testing.T) {
	const content = "large new version content"
	var srvURL string
	var ifMatchSent string
	var partBody []byte

	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"folders":[],"files":[{"id":"f1","name":"video.mp4","size":"5","updatedAt":"2026-09-26T10:00:00Z"}],"nextCursor":null}`)
	})
	mux.HandleFunc("/files/f1/version/initiate", func(w http.ResponseWriter, r *http.Request) {
		ifMatchSent = r.Header.Get("If-Match")
		io.WriteString(w, `{"uploadId":"up1","partSize":16777216,"parts":[{"partNumber":1,"url":"`+srvURL+`/fake-s3-part"}]}`)
	})
	mux.HandleFunc("/fake-s3-part", func(w http.ResponseWriter, r *http.Request) {
		partBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"etag1"`)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/files/f1/version/complete", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"f1","name":"video.mp4","size":%d,"updatedAt":"2026-09-27T10:00:00Z"}`, len(content))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "video.mp4")
	if err != nil {
		t.Fatal(err)
	}

	err = o.Update(ctx, strings.NewReader(content), object.NewStaticObjectInfo("video.mp4", time.Now(), largeUploadMinBytes, true, nil, nil))
	if err != nil {
		t.Fatalf("Update (large) = %v", err)
	}
	if ifMatchSent == "" {
		t.Fatal("If-Match was not sent on the large-version initiate")
	}
	if string(partBody) != content {
		t.Fatalf("uploaded part body = %q, want %q", partBody, content)
	}
	if o.(*Object).id != "f1" {
		t.Fatalf("object id after update = %q, want f1", o.(*Object).id)
	}
}

// TestUpdateLargeConflict: a stale If-Match on a LARGE version upload also
// gets the B3 keep-both treatment, and the conflicted copy itself goes
// through the large (presigned-multipart) create path, not the simple one -
// the copy is exactly as big as the edit that triggered it.
func TestUpdateLargeConflict(t *testing.T) {
	const content = "large conflicted content"
	var srvURL string
	var copyPartBody []byte
	var copyName string
	var calls []string

	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"folders":[],"files":[{"id":"f1","name":"video.mp4","size":"5","updatedAt":"2026-09-26T10:00:00Z"}],"nextCursor":null}`)
	})
	mux.HandleFunc("/files/f1/version/initiate", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "version-initiate")
		w.WriteHeader(http.StatusConflict)
		io.WriteString(w, `{"message":"stale"}`)
	})
	mux.HandleFunc("/storage/upload/initiate", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "copy-initiate")
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		copyName, _ = body["name"].(string)
		io.WriteString(w, `{"uploadId":"up2","partSize":16777216,"parts":[{"partNumber":1,"url":"`+srvURL+`/fake-s3-part"}]}`)
	})
	mux.HandleFunc("/fake-s3-part", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "part-put")
		copyPartBody, _ = io.ReadAll(r.Body)
		w.Header().Set("ETag", `"etag2"`)
		w.WriteHeader(200)
	})
	mux.HandleFunc("/storage/upload/complete", func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, "copy-complete")
		fmt.Fprintf(w, `{"id":"f2","name":%q,"size":%d}`, copyName, len(content))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.NewObject(ctx, "video.mp4")
	if err != nil {
		t.Fatal(err)
	}

	err = o.Update(ctx, strings.NewReader(content), object.NewStaticObjectInfo("video.mp4", time.Now(), largeUploadMinBytes, true, nil, nil))
	if err != nil {
		t.Fatalf("Update should resolve the conflict via a large keep-both copy, not fail: %v", err)
	}
	if string(copyPartBody) != content {
		t.Fatalf("conflicted copy part body = %q, want %q", copyPartBody, content)
	}
	if !strings.Contains(copyName, "(conflicted copy ") || !strings.HasSuffix(copyName, ".mp4") {
		t.Fatalf("conflicted copy name = %q", copyName)
	}
	want := []string{"version-initiate", "copy-initiate", "part-put", "copy-complete"}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// TestAbout confirms the mount reports real plan-quota numbers, not
// placeholders - this is what makes Windows Explorer / Finder show an
// accurate used/free bar for the mounted drive, and what lets a native
// copy dialog refuse an over-quota write before it ever starts.
func TestAbout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/me", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"storageUsed":"60000000000","storageLimit":"100000000000"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := f.Features().About(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if *usage.Total != 100000000000 {
		t.Fatalf("Total = %d, want 100000000000 (the plan's 100GB limit)", *usage.Total)
	}
	if *usage.Used != 60000000000 {
		t.Fatalf("Used = %d, want 60000000000", *usage.Used)
	}
	if *usage.Free != 40000000000 {
		t.Fatalf("Free = %d, want 40000000000 (100GB limit - 60GB used)", *usage.Free)
	}
}

// TestPollChangesOnce: a non-empty GET /drive/changes response flushes the
// local directory cache and advances the cursor; an empty one advances the
// cursor without flushing; a request error leaves the cursor unchanged
// (so a transient failure can't skip past whatever happened during it).
func TestPollChangesOnce(t *testing.T) {
	var cursorsSeen []string
	respond := `{"files":[{"id":"f1"}],"folders":[],"deletedFileIds":[],"deletedFolderIds":[],"nextCursor":"2026-10-02T00:00:00.000Z"}`
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/changes", func(w http.ResponseWriter, r *http.Request) {
		cursorsSeen = append(cursorsSeen, r.URL.Query().Get("cursor"))
		io.WriteString(w, respond)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	fsIface, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	f := fsIface.(*Fs)

	// Non-empty response: flushes, advances the cursor.
	cursor, flushed := f.pollChangesOnce(ctx, "")
	if !flushed {
		t.Fatal("flushed = false, want true for a non-empty changes response")
	}
	if cursor != "2026-10-02T00:00:00.000Z" {
		t.Fatalf("cursor = %q", cursor)
	}

	// Empty response: no flush, cursor still advances to whatever the
	// server returns.
	respond = `{"files":[],"folders":[],"deletedFileIds":[],"deletedFolderIds":[],"nextCursor":"2026-10-02T00:00:01.000Z"}`
	cursor, flushed = f.pollChangesOnce(ctx, cursor)
	if flushed {
		t.Fatal("flushed = true, want false for an empty changes response")
	}
	if cursor != "2026-10-02T00:00:01.000Z" {
		t.Fatalf("cursor = %q", cursor)
	}

	if len(cursorsSeen) != 2 || cursorsSeen[0] != "" || cursorsSeen[1] != "2026-10-02T00:00:00.000Z" {
		t.Fatalf("cursorsSeen = %v", cursorsSeen)
	}
}

// TestPollChangesOnceRequestError: a failed request leaves the cursor
// unchanged and never flushes - a transient outage shouldn't lose track
// of what the client has already seen.
func TestPollChangesOnceRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx := context.Background()
	fsIface, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	f := fsIface.(*Fs)

	cursor, flushed := f.pollChangesOnce(ctx, "some-cursor")
	if flushed {
		t.Fatal("flushed = true, want false on a request error")
	}
	if cursor != "some-cursor" {
		t.Fatalf("cursor = %q, want unchanged %q", cursor, "some-cursor")
	}
}

// TestRemoveTreatsRetriedNotFoundAsSuccess: a DELETE that comes back 404
// means "already gone" (IDs are UUIDs, never reused) - most plausibly our
// own prior attempt actually succeeded server-side and only the response
// was lost, but even a concurrent delete from elsewhere should resolve the
// same way. Either way, Remove must not surface this as an error.
func TestRemoveTreatsRetriedNotFoundAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"not found"}`)
	}))
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	o := &Object{fs: f.(*Fs), remote: "gone.txt", id: "f1"}
	if err := o.Remove(ctx); err != nil {
		t.Fatalf("Remove = %v, want nil (a 404 on delete means already gone)", err)
	}
}

// TestRmdirTreatsRetriedNotFoundAsSuccess: same reasoning as Remove, for
// folders - a 404 on the DELETE itself (not the earlier FindDir/listDir
// pre-checks, which still surface their own real errors normally).
func TestRmdirTreatsRetriedNotFoundAsSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("folderId") == "d1" {
			io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`) // Docs is empty
			return
		}
		io.WriteString(w, `{"folders":[{"id":"d1","name":"Docs"}],"files":[],"nextCursor":null}`)
	})
	mux.HandleFunc("/folders/d1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"message":"not found"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Rmdir(ctx, "Docs"); err != nil {
		t.Fatalf("Rmdir = %v, want nil (a 404 on delete means already gone)", err)
	}
}

// fakeFsWithTransport builds an *Fs exactly like NewFs does, but with a
// caller-supplied http.RoundTripper instead of a real network client - lets
// a test simulate a genuine connection-level failure (not just an HTTP
// status code from a server that's actually reachable), which httptest's
// normal fake server can't produce on its own.
func fakeFsWithTransport(t *testing.T, rt http.RoundTripper, baseURL string) *Fs {
	t.Helper()
	f := &Fs{
		name: "zdrive",
		root: "",
		srv: rest.NewClient(&http.Client{Transport: rt}).
			SetRoot(baseURL).
			SetHeader("Authorization", "Bearer tok").
			SetHeader("X-ZDrive-Client-Version", ClientVersion).
			SetErrorHandler(errorHandler),
	}
	f.features = (&fs.Features{DuplicateFiles: true, CanHaveEmptyDirectories: true}).Fill(context.Background(), f)
	f.dirCache = dircache.New(f.root, rootID, f)
	if err := f.dirCache.FindRoot(context.Background(), false); err != nil {
		t.Fatalf("FindRoot = %v", err)
	}
	return f
}

// flakyOnceTransport fails the first N requests with a genuine connection-
// level error (no response at all), then delegates to a real transport.
type flakyOnceTransport struct {
	remaining int
	real      http.RoundTripper
}

func (t *flakyOnceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.remaining > 0 {
		t.remaining--
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	return t.real.RoundTrip(req)
}

// TestRemoveRetriesPastRealNetworkBlip proves Remove is actually wired
// through withRetry for a genuine connection failure, not just an HTTP
// status code - isRetriable/withRetry's own unit tests already prove the
// retry mechanics in isolation, this proves Remove really uses them.
func TestRemoveRetriesPastRealNetworkBlip(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			deleted = true
		}
		io.WriteString(w, `{}`)
	}))
	defer srv.Close()

	transport := &flakyOnceTransport{remaining: 2, real: http.DefaultTransport}
	f := fakeFsWithTransport(t, transport, srv.URL)
	o := &Object{fs: f, remote: "x.txt", id: "f1"}

	if err := o.Remove(context.Background()); err != nil {
		t.Fatalf("Remove = %v, want it to transparently retry past 2 connection failures", err)
	}
	if !deleted {
		t.Fatal("DELETE never reached the server")
	}
}

// TestMoveRetriesPastRealNetworkBlip: same proof for Move's straight-line
// (non-recovery) path.
func TestMoveRetriesPastRealNetworkBlip(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`)
	})
	mux.HandleFunc("/files/f1", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"f1","size":"5","updatedAt":"2026-10-04T12:00:00Z"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	transport := &flakyOnceTransport{remaining: 2, real: http.DefaultTransport}
	f := fakeFsWithTransport(t, transport, srv.URL)
	src := &Object{fs: f, remote: "a.txt", id: "f1", size: 5, modTime: time.Now()}

	dst, err := f.Move(context.Background(), src, "b.txt")
	if err != nil {
		t.Fatalf("Move = %v, want it to transparently retry past 2 connection failures", err)
	}
	if dst.Remote() != "b.txt" {
		t.Fatalf("dst.Remote() = %q, want b.txt", dst.Remote())
	}
}
