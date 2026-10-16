package zdrive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchAccountInfo(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan":"FREE","storageUsed":1234,"storageLimit":5368709120}`))
	}))
	defer srv.Close()

	info, err := fetchAccountInfo(context.Background(), srv.URL, "test-token")
	if err != nil {
		t.Fatalf("fetchAccountInfo returned an error: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-token")
	}
	if gotPath != "/auth/me" {
		t.Errorf("request path = %q, want %q", gotPath, "/auth/me")
	}
	if info.Plan != "FREE" {
		t.Errorf("Plan = %q, want %q", info.Plan, "FREE")
	}
	if info.StorageUsed != 1234 {
		t.Errorf("StorageUsed = %d, want 1234", info.StorageUsed)
	}
	if info.StorageLimit != 5368709120 {
		t.Errorf("StorageLimit = %d, want 5368709120", info.StorageLimit)
	}
}

func TestFetchAccountInfoRequestError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := fetchAccountInfo(context.Background(), srv.URL, "bad-token"); err == nil {
		t.Fatal("expected an error for a 401 response, got nil")
	}
}
