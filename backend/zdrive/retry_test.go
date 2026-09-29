package zdrive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config/configmap"
)

func TestIsRetriable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"429 rate limited", &apiError{Status: http.StatusTooManyRequests}, true},
		{"500 server error", &apiError{Status: 500}, true},
		{"503 unavailable", &apiError{Status: 503}, true},
		{"400 bad request", &apiError{Status: 400}, false},
		{"401 unauthorized", &apiError{Status: 401}, false},
		{"403 forbidden", &apiError{Status: 403}, false},
		{"404 not found", &apiError{Status: 404}, false},
		{"409 conflict", &apiError{Status: 409}, false},
		{"non-apiError", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetriable(c.err); got != c.want {
				t.Errorf("isRetriable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestWithRetrySucceedsAfterTransientFailures(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	attempts := 0
	err := withRetry(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return &apiError{Status: 503}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withRetry = %v, want nil (should have succeeded on attempt 3)", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWithRetryDoesNotRetryNonRetriableErrors(t *testing.T) {
	attempts := 0
	wantErr := &apiError{Status: 404}
	err := withRetry(context.Background(), func() error {
		attempts++
		return wantErr
	})
	if err != error(wantErr) {
		t.Fatalf("withRetry = %v, want the original 404 unwrapped", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (a 404 must never be retried)", attempts)
	}
}

func TestWithRetryGivesUpAfterMaxRetries(t *testing.T) {
	origDelay, origMax := retryBaseDelay, maxRetries
	retryBaseDelay = time.Millisecond
	maxRetries = 3
	defer func() { retryBaseDelay, maxRetries = origDelay, origMax }()

	attempts := 0
	err := withRetry(context.Background(), func() error {
		attempts++
		return &apiError{Status: 500}
	})
	if err == nil {
		t.Fatal("withRetry should still fail after exhausting all retries")
	}
	if attempts != maxRetries+1 {
		t.Fatalf("attempts = %d, want %d (the initial try plus %d retries)", attempts, maxRetries+1, maxRetries)
	}
}

// TestPutPartRetriesOnTransientFailure: the highest-value retry path -
// a real 503 on the first PUT, succeeding on the second, without the
// caller ever seeing an error or having to restart the whole upload.
func TestPutPartRetriesOnTransientFailure(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if string(body) != "part data" {
			t.Fatalf("retry sent different data = %q", body)
		}
		w.Header().Set("ETag", `"real-etag"`)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	etag, err := putPart(context.Background(), srv.URL, []byte("part data"))
	if err != nil {
		t.Fatalf("putPart = %v", err)
	}
	if etag != `"real-etag"` {
		t.Fatalf("etag = %q", etag)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (one failure, one success)", attempts)
	}
}

// TestListRetriesOnTransientFailure confirms a read (List, via listDir)
// transparently recovers from one transient failure - the mount shouldn't
// surface a spurious error for something the client can just retry.
func TestListRetriesOnTransientFailure(t *testing.T) {
	origDelay := retryBaseDelay
	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = origDelay }()

	attempts := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/drive/list", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"folders":[],"files":[],"nextCursor":null}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	f, err := NewFs(ctx, "zdrive", "", configmap.Simple{"url": srv.URL, "token": "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatalf("List = %v, want it to transparently retry past the 503", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
