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
	"strings"

	"fyne.io/systray"
	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

func init() {
	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		if isMountCommand(command.Name()) {
			go runTray()
		}
		return nil
	}
}

// isMountCommand matches the three mount-shaped subcommands this project
// registers (cmd/mount, cmd/cmount, cmd/nfsmount). Duplicated from
// cmd/pin's own original copy rather than depending on the in-flight
// autostart PR's zdrive.IsMountCommand promotion - these two PRs are
// independent and may merge in either order; switch this to the shared
// helper once autostart lands first.
func isMountCommand(name string) bool {
	switch name {
	case "mount", "cmount", "nfsmount":
		return true
	}
	return false
}

func runTray() {
	defer zdrive.RecoverAndLog("tray")
	systray.Run(onReady, func() {})
}

func onReady() {
	systray.SetIcon(iconBytes())
	systray.SetTooltip("ZDrive")

	status := systray.AddMenuItem("Mounted", "")
	status.Disable() // informational only - optimistic "running" for v1, not a live health check
	systray.AddSeparator()
	openItem := systray.AddMenuItem("Open web app", "Open the ZDrive web app in your browser")
	quitItem := systray.AddMenuItem("Quit", "Unmount and quit ZDrive")

	for {
		select {
		case <-openItem.ClickedCh:
			openWebApp()
		case <-quitItem.ClickedCh:
			quitMount()
			return
		}
	}
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
