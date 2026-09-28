package zdrive

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
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
		if err := o.Update(ctx, strings.NewReader("more junk"), object.NewStaticObjectInfo(name, time.Now(), 9, true, nil, nil)); err != nil {
			t.Fatalf("Update(%q) = %v", name, err)
		}
		if err := o.Remove(ctx); err != nil {
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
		io.WriteString(w, `{"uploadId":"up1","key":"k1","parts":[{"partNumber":1,"url":"`+srvURL+`/fake-s3-part"}]}`)
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
