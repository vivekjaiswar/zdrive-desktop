//go:build windows || darwin

// Package tray shows a minimal system-tray icon (status/quit/open web
// app) while a mount is running - the no-Qt-GUI decision in docs/PLAN.md
// draws the line here deliberately: no account-management UI, just
// enough to know it's running and reach for Quit. Windows and macOS
// only; tray_linux.go is an empty stub so main.go's unconditional blank
// import still compiles on Linux (dev-only for this project, and
// systray's Linux backend would need a D-Bus session that isn't worth
// pulling in for that audience).
package tray

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

// updateCheckInterval controls how often the tray looks at the version
// the backend's most recently mentioned (see LatestKnownVersion,
// refreshed by the already-running 30s change-feed poll) and, if it's
// newer, downloads+verifies it. An hour is deliberately not aggressive -
// this is a background check, not a user-initiated action.
const updateCheckInterval = time.Hour

func init() {
	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		// macOS's systray backend drives real Cocoa/AppKit APIs via cgo,
		// which (like all AppKit calls) must run on the actual main OS
		// thread - calling Run() from a spawned goroutine, as this used
		// to do unconditionally, crashes with a bare SIGTRAP inside
		// nativeLoop on first real Apple Silicon hardware (confirmed
		// 2026-10-14; fyne.io/systray's own README example only ever
		// calls Run() from main() directly, never go Run()). Properly
		// fixing this means inverting this program's control flow on
		// macOS specifically - systray.Run() owning the real main
		// goroutine, with cobra's command execution moved into its
		// onReady callback - too large a change to land alongside a
		// one-line crash fix. Disabling tray on macOS until that's done
		// is a real regression (no tray icon at all on Mac for now), but
		// a missing icon beats a crash on every single mount attempt.
		// Windows' backend has no such thread restriction and is
		// unaffected.
		if zdrive.IsMountCommand(command.Name()) && runtime.GOOS != "darwin" {
			go runTray()
		}
		return nil
	}
}

func runTray() {
	defer zdrive.RecoverAndLog("tray")
	cleanupOldBinary() // no-op on macOS - only Windows's swap leaves one behind, see update_windows.go
	systray.Run(onReady, func() {})
}

func onReady() {
	systray.SetIcon(iconBytes())
	systray.SetTooltip("ZDrive")

	status := systray.AddMenuItem("Mounted", "")
	status.Disable() // informational only - optimistic "running" for v1, not a live health check
	systray.AddSeparator()
	openItem := systray.AddMenuItem("Open web app", "Open the ZDrive web app in your browser")
	updateItem := systray.AddMenuItem("Restart to finish updating", "A new version has been downloaded and verified")
	updateItem.Hide() // only shown once an update is actually staged
	quitItem := systray.AddMenuItem("Quit", "Unmount and quit ZDrive")

	var stagedPath string
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-openItem.ClickedCh:
			openWebApp()
		case <-updateItem.ClickedCh:
			if stagedPath == "" {
				continue // menu item is hidden until staged, but guard anyway
			}
			if applyStagedUpdate(stagedPath) {
				return
			}
			// Apply failed (already logged inside) - un-stage so the next
			// tick can try again from scratch rather than repeatedly
			// retrying a swap that just failed.
			stagedPath = ""
			updateItem.Hide()
		case <-quitItem.ClickedCh:
			quitMount()
			return
		case <-ticker.C:
			if stagedPath == "" {
				if path := checkAndStageUpdate(); path != "" {
					stagedPath = path
					updateItem.Show()
				}
			}
		}
	}
}

// checkAndStageUpdate compares the most recently backend-reported
// version (LatestKnownVersion, refreshed by the existing change-feed
// poll - see backend/zdrive/update.go) against this binary's own
// compiled-in version, and if newer, downloads+verifies it into the same
// directory the running binary lives in. Does NOT apply or restart
// anything - that only happens once the user clicks the menu item,
// deliberately: silently swapping and restarting while a drive is
// actively mounted risks the exact Finder/Explorer-hang failure mode
// kernel-adjacent software is prone to.
func checkAndStageUpdate() string {
	latest := zdrive.LatestKnownVersion()
	if !zdrive.IsUpdateAvailable(latest) {
		return ""
	}
	execPath, err := os.Executable()
	if err != nil {
		fs.Logf(nil, "auto-update: couldn't resolve this binary's own path: %v", err)
		return ""
	}
	stagedPath, err := zdrive.DownloadRelease(latest, filepath.Dir(execPath))
	if err != nil {
		fs.Logf(nil, "auto-update: failed to download/verify v%s: %v", latest, err)
		return ""
	}
	return stagedPath
}

// applyStagedUpdate swaps in the already-verified binary at stagedPath
// and launches it, then quits this (old) process - returns false (and
// logs) on failure, leaving the old process running untouched.
func applyStagedUpdate(stagedPath string) bool {
	execPath, err := os.Executable()
	if err != nil {
		fs.Logf(nil, "auto-update: couldn't resolve this binary's own path: %v", err)
		return false
	}
	if err := applyUpdate(execPath, stagedPath); err != nil {
		fs.Logf(nil, "auto-update: failed to apply staged update: %v", err)
		return false
	}
	quitMount()
	return true
}

// openWebApp derives the web app's URL from the same saved login config
// cmd/login itself writes (config.FileSetValue("zdrive", "url", ...)),
// stripping the API suffix the same way cmd/login's own verifyURL
// derivation does - reusing that config value instead of hardcoding a
// domain means this still works against staging or any other URL
// someone logged in against.
func openWebApp() {
	apiURL, ok := config.FileGetValue("zdrive", "url")
	if !ok || apiURL == "" {
		return
	}
	webURL := strings.TrimSuffix(strings.TrimSuffix(apiURL, "/api"), "/")
	_ = oauthutil.OpenURL(webURL)
}

// quitMount sends this process its own interrupt, reusing rclone's
// existing signal-handled clean-unmount path instead of building a
// second shutdown mechanism (os.Exit would skip that cleanup entirely).
func quitMount() {
	systray.Quit()
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return
	}
	_ = p.Signal(os.Interrupt)
}
