package zdrive

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
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
