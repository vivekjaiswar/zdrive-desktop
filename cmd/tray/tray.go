//go:build windows || darwin

// Package tray shows a minimal system-tray icon (status, plan/storage,
// quit, open web app) while a mount is running - the no-Qt-GUI decision
// in docs/PLAN.md draws the line here deliberately: no account
// *management* UI (upgrading, inviting users, etc. all stay on the web
// app, reached via the "Manage billing" link), just enough to glance at
// without opening a browser. Windows and macOS only; tray_linux.go is an
// empty stub so main.go's unconditional blank import still compiles on
// Linux (dev-only for this project, and systray's Linux backend would
// need a D-Bus session that isn't worth pulling in for that audience).
package tray

import (
	"context"
	"fmt"
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

// accountRefreshInterval controls how often the tray re-fetches plan/
// storage info. More frequent than the update check (that data actually
// changes as someone uploads files or changes plans) but still a
// background poll, not tied to the 30s change-feed poll which is about
// file sync, not account state.
const accountRefreshInterval = 5 * time.Minute

func init() {
	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		// macOS is deliberately excluded here: fyne.io/systray's macOS
		// backend drives real Cocoa/AppKit APIs via cgo, which must run
		// on the actual main OS thread - calling Run() from a goroutine
		// spawned from this hook (as this used to do unconditionally)
		// crashed with a bare SIGTRAP on real Apple Silicon hardware
		// (confirmed 2026-10-14). The tray there is instead started
		// earlier and differently, from main() itself via
		// cmd/tray/entrypoint_darwin.go's RunMain - owning the real main
		// goroutine before cobra ever dispatches to this hook, with
		// cmd.Main() (and everything below it, including this hook)
		// running on a spawned goroutine instead. Spawning a second tray
		// here for darwin would just double it. Windows' backend has no
		// such thread restriction, so this hook is its only/correct path.
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
	planItem := systray.AddMenuItem("Plan: -", "")
	planItem.Disable()
	storageItem := systray.AddMenuItem("Storage: -", "")
	storageItem.Disable()
	systray.AddSeparator()
	billingItem := systray.AddMenuItem("Manage billing", "Open the ZDrive billing page in your browser")
	openItem := systray.AddMenuItem("Open web app", "Open the ZDrive web app in your browser")
	updateItem := systray.AddMenuItem("Restart to finish updating", "A new version has been downloaded and verified")
	updateItem.Hide() // only shown once an update is actually staged
	quitItem := systray.AddMenuItem("Quit", "Unmount and quit ZDrive")

	var stagedPath string
	updateTicker := time.NewTicker(updateCheckInterval)
	defer updateTicker.Stop()
	accountTicker := time.NewTicker(accountRefreshInterval)
	defer accountTicker.Stop()
	refreshAccountInfo(planItem, storageItem)

	for {
		select {
		case <-openItem.ClickedCh:
			openWebApp("")
		case <-billingItem.ClickedCh:
			openWebApp("/billing")
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
		case <-updateTicker.C:
			if stagedPath == "" {
				if path := checkAndStageUpdate(); path != "" {
					stagedPath = path
					updateItem.Show()
				}
			}
		case <-accountTicker.C:
			refreshAccountInfo(planItem, storageItem)
		}
	}
}

// refreshAccountInfo fetches plan/storage info and updates the given menu
// items - on failure (not logged in, offline, backend error) it leaves
// whatever text was already showing in place rather than blanking out
// previously-good info over a transient blip.
func refreshAccountInfo(planItem, storageItem *systray.MenuItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := zdrive.FetchAccountInfo(ctx)
	if err != nil {
		fs.Debugf(nil, "tray: couldn't refresh account info: %v", err)
		return
	}
	planItem.SetTitle(fmt.Sprintf("Plan: %s", info.Plan))
	storageItem.SetTitle(fmt.Sprintf("Storage: %s of %s used",
		fs.SizeSuffix(info.StorageUsed), fs.SizeSuffix(info.StorageLimit)))
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
// someone logged in against. path is appended as-is (e.g. "/billing"),
// or pass "" for the bare web app root.
func openWebApp(path string) {
	apiURL, ok := config.FileGetValue("zdrive", "url")
	if !ok || apiURL == "" {
		return
	}
	webURL := strings.TrimSuffix(strings.TrimSuffix(apiURL, "/api"), "/")
	_ = oauthutil.OpenURL(webURL + path)
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
