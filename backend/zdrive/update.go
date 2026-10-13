package zdrive

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/coreos/go-semver/semver"
)

// latestKnownVersion is set from the X-ZDrive-Latest-Version response
// header on the regular change-feed poll (pollChangesOnce) - piggybacking
// on an already-running, already-authenticated call rather than a
// separate periodic request. atomic.Value since it's written from the
// poll goroutine and read from a tray goroutine.
var latestKnownVersion atomic.Value

func init() {
	latestKnownVersion.Store("")
}

// SetLatestKnownVersion records the newest version the backend has told
// this client about. Empty/unparseable values are handled by
// IsUpdateAvailable, not here - storing the raw string keeps this a pure
// setter with no validation logic to duplicate.
func SetLatestKnownVersion(v string) {
	latestKnownVersion.Store(v)
}

// LatestKnownVersion returns the most recent value SetLatestKnownVersion
// was given, or "" if the backend has never sent one (e.g.
// LATEST_DESKTOP_CLIENT_VERSION isn't configured server-side yet).
func LatestKnownVersion() string {
	return latestKnownVersion.Load().(string)
}

// IsUpdateAvailable reports whether latest is a real, parseable version
// strictly newer than ClientVersion. An empty or unparseable latest (e.g.
// the backend hasn't been configured with LATEST_DESKTOP_CLIENT_VERSION
// yet) is treated as "no update", not an error - a config gap
// server-side shouldn't surface as a client bug.
func IsUpdateAvailable(latest string) bool {
	if latest == "" {
		return false
	}
	latestVer, err := semver.NewVersion(latest)
	if err != nil {
		return false
	}
	currentVer, err := semver.NewVersion(ClientVersion)
	if err != nil {
		return false // can't happen for our own compiled-in constant, but fail safe rather than panic
	}
	return currentVer.LessThan(*latestVer)
}

const releaseRepo = "vivekjaiswar/zdrive-desktop"

// releaseAssetName returns this platform's exact binary filename as
// produced by .github/workflows/build.yml - must stay in sync with that
// file's output names.
func releaseAssetName() (string, error) {
	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		return "zdrive-windows-amd64.exe", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "zdrive-macos-arm64", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return "zdrive-macos-amd64", nil
	default:
		return "", fmt.Errorf("auto-update isn't supported on %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

// releaseBaseURL is a var, not a const, so tests can point it at a fake
// httptest.Server instead of real GitHub.
var releaseBaseURL = func(version string) string {
	return fmt.Sprintf("https://github.com/%s/releases/download/v%s", releaseRepo, version)
}

// DownloadRelease fetches version's release asset for this platform plus
// its checksums.txt, verifies the asset's sha256 against the matching
// line, and writes it (executable, 0o755) to destDir under its original
// asset name. Returns the full path to the verified, staged file.
//
// This only proves the download matches what the release published -
// integrity, not authenticity. It doesn't catch a compromised release
// publishing a malicious binary alongside a matching bad checksum; real
// protection there needs code signing (a detached signature verified
// against a key pinned in the *old*, already-trusted client), which
// doesn't exist yet - deferred along with the rest of code signing per
// the M4 plan. Documented here, not silently assumed solved.
func DownloadRelease(version, destDir string) (string, error) {
	assetName, err := releaseAssetName()
	if err != nil {
		return "", err
	}
	return downloadAndVerify(assetName, version, destDir)
}

// downloadAndVerify is DownloadRelease's actual mechanism, split out so
// it's testable on any platform (releaseAssetName's result is the only
// OS-specific part - this function itself has nothing in it that only
// makes sense on Windows/macOS).
func downloadAndVerify(assetName, version, destDir string) (string, error) {
	base := releaseBaseURL(version)

	checksums, err := httpGetBody(base + "/checksums.txt")
	if err != nil {
		return "", fmt.Errorf("fetch checksums.txt: %w", err)
	}
	wantHash, err := findChecksum(checksums, assetName)
	if err != nil {
		return "", err
	}

	data, err := httpGetBody(base + "/" + assetName)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", assetName, err)
	}

	gotSum := sha256.Sum256(data)
	gotHash := hex.EncodeToString(gotSum[:])
	if gotHash != wantHash {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", assetName, gotHash, wantHash)
	}

	// .new suffix deliberately avoids ever matching assetName exactly -
	// destDir is typically wherever the currently-running binary lives,
	// and if the user kept the release's own default filename, this
	// would otherwise collide with the exact file the platform-specific
	// swap step (applyUpdate) needs to carefully replace while it may
	// still be running (Windows in particular can't just be overwritten
	// in place).
	destPath := filepath.Join(destDir, assetName+".new")
	if err := os.WriteFile(destPath, data, 0o755); err != nil {
		return "", err
	}
	return destPath, nil
}

func httpGetBody(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec // destination is always releaseBaseURL (GitHub or a test server), never user input
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// findChecksum looks up name's hash in a standard `sha256sum` output file
// (lines of "<hash>  <filename>").
func findChecksum(checksumsFile []byte, name string) (string, error) {
	for _, line := range strings.Split(string(checksumsFile), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry found for %s", name)
}
