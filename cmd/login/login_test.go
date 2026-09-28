package login

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

// noSleep lets the polling loop run at full speed in tests - production
// uses time.Sleep, this is a no-op honoring the same signature.
func noSleep(time.Duration) {}

func TestDoLoginApprovedAfterPending(t *testing.T) {
	polls := 0
	var gotVersion string
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.Header.Get("X-ZDrive-Client-Version")
		io.WriteString(w, `{"deviceCode":"dc1","userCode":"AAAA-BBBB","expiresIn":60,"interval":1}`)
	})
	mux.HandleFunc("/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		polls++
		if polls < 2 {
			io.WriteString(w, `{"status":"pending"}`)
			return
		}
		io.WriteString(w, `{"status":"approved","accessToken":"tok123"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	var savedURL, savedToken string
	err := doLogin(context.Background(), srv.URL, noSleep, func(url, token string) {
		savedURL, savedToken = url, token
	})
	if err != nil {
		t.Fatalf("doLogin = %v", err)
	}
	if savedToken != "tok123" || savedURL != srv.URL {
		t.Fatalf("saved = %q, %q", savedURL, savedToken)
	}
	if polls < 2 {
		t.Fatalf("polls = %d, want at least 2 (one pending, one approved)", polls)
	}
	if gotVersion != zdrive.ClientVersion {
		t.Fatalf("X-ZDrive-Client-Version = %q, want %q", gotVersion, zdrive.ClientVersion)
	}
}

func TestDoLoginDenied(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"deviceCode":"dc1","userCode":"AAAA-BBBB","expiresIn":60,"interval":1}`)
	})
	mux.HandleFunc("/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	saved := false
	err := doLogin(context.Background(), srv.URL, noSleep, func(string, string) { saved = true })
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("err = %v, want a denied error", err)
	}
	if saved {
		t.Fatal("token should not be saved on denial")
	}
}

func TestDoLoginExpired(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"deviceCode":"dc1","userCode":"AAAA-BBBB","expiresIn":60,"interval":1}`)
	})
	mux.HandleFunc("/auth/device/poll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := doLogin(context.Background(), srv.URL, noSleep, func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expired error", err)
	}
}

func TestDoLoginStartFails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/device/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	err := doLogin(context.Background(), srv.URL, noSleep, func(string, string) {})
	if err == nil {
		t.Fatal("expected an error when /auth/device/start fails")
	}
}
