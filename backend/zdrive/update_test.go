package zdrive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestLatestKnownVersionRoundTrips(t *testing.T) {
	SetLatestKnownVersion("0.9.9")
	defer SetLatestKnownVersion("")

	if got := LatestKnownVersion(); got != "0.9.9" {
		t.Errorf("LatestKnownVersion() = %q, want 0.9.9", got)
	}
}

func TestIsUpdateAvailable(t *testing.T) {
	cases := []struct {
		name   string
		latest string
		want   bool
	}{
		{"empty - never configured server-side", "", false},
		{"unparseable", "not-a-version", false},
		{"older than current", "0.0.1", false},
		{"equal to current", ClientVersion, false},
		{"newer than current", "99.0.0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsUpdateAvailable(c.latest); got != c.want {
				t.Errorf("IsUpdateAvailable(%q) = %v, want %v", c.latest, got, c.want)
			}
		})
	}
}

// downloadAndVerify holds all of DownloadRelease's real logic and takes
// an explicit asset name, so these tests exercise it directly on any
// platform - releaseAssetName (DownloadRelease's only OS-specific part)
// isn't involved here at all.
const testAssetName = "zdrive-test-asset"

func TestDownloadAndVerifyWritesExecutableUnderNewSuffix(t *testing.T) {
	content := []byte("fake binary content")
	sum := sha256.Sum256(content)
	checksums := hex.EncodeToString(sum[:]) + "  " + testAssetName + "\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.2.3/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		case "/v1.2.3/" + testAssetName:
			_, _ = w.Write(content)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = func(version string) string { return srv.URL + "/v" + version }
	defer func() { releaseBaseURL = origBase }()

	destDir := t.TempDir()
	path, err := downloadAndVerify(testAssetName, "1.2.3", destDir)
	if err != nil {
		t.Fatalf("downloadAndVerify() = %v", err)
	}

	// .new suffix matters: destDir is typically wherever the running
	// binary already lives, and this must never collide with a file of
	// the exact release-asset name that might already be running.
	wantPath := filepath.Join(destDir, testAssetName+".new")
	if path != wantPath {
		t.Errorf("downloadAndVerify() path = %q, want %s", path, wantPath)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading staged file: %v", err)
	}
	if string(got) != string(content) {
		t.Error("staged file content doesn't match the downloaded asset")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat staged file: %v", err)
	}
	if info.Mode()&0o100 == 0 {
		t.Error("staged file isn't executable")
	}
}

func TestDownloadAndVerifyRejectsBadChecksum(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.2.3/checksums.txt":
			_, _ = w.Write([]byte(fmt.Sprintf("%064x  %s\n", 0, testAssetName))) // wrong hash on purpose
		case "/v1.2.3/" + testAssetName:
			_, _ = w.Write([]byte("real content"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = func(version string) string { return srv.URL + "/v" + version }
	defer func() { releaseBaseURL = origBase }()

	if _, err := downloadAndVerify(testAssetName, "1.2.3", t.TempDir()); err == nil {
		t.Fatal("downloadAndVerify() = nil error, want a checksum mismatch error")
	}
}

func TestDownloadAndVerifyMissingChecksumEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.2.3/checksums.txt":
			_, _ = w.Write([]byte("abc123  some-other-file\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = func(version string) string { return srv.URL + "/v" + version }
	defer func() { releaseBaseURL = origBase }()

	if _, err := downloadAndVerify(testAssetName, "1.2.3", t.TempDir()); err == nil {
		t.Fatal("downloadAndVerify() = nil error, want a missing-checksum-entry error")
	}
}

func TestDownloadAndVerifyPropagatesHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	origBase := releaseBaseURL
	releaseBaseURL = func(version string) string { return srv.URL + "/v" + version }
	defer func() { releaseBaseURL = origBase }()

	if _, err := downloadAndVerify(testAssetName, "1.2.3", t.TempDir()); err == nil {
		t.Fatal("downloadAndVerify() = nil error, want an HTTP-error propagated")
	}
}

// DownloadRelease itself only adds the OS-specific asset-name lookup on
// top of downloadAndVerify - on an unsupported platform (this test
// runner's), it should fail clearly rather than silently doing nothing.
func TestDownloadReleaseFailsOnUnsupportedPlatform(t *testing.T) {
	if _, err := releaseAssetName(); err == nil {
		t.Skip("this test platform is a supported auto-update target - nothing to assert here")
	}

	if _, err := DownloadRelease("1.2.3", t.TempDir()); err == nil {
		t.Fatal("DownloadRelease() = nil error, want the unsupported-platform error from releaseAssetName")
	}
}
