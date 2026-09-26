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
