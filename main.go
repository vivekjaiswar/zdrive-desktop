// zdrive is rclone with the ZDrive backend compiled in and only the
// commands the desktop drive needs.
//
//	zdrive login                                                (once, opens a browser to approve this device)
//	zdrive mount zdrive: ~/ZDrive --vfs-cache-mode full        (Linux)
//	zdrive nfsmount zdrive: ~/ZDrive --vfs-cache-mode full     (macOS, no macFUSE)
//	zdrive mount zdrive: Z: --vfs-cache-mode full              (Windows, WinFsp)
//	zdrive pin ~/ZDrive/Reports/Q3.xlsx                         (keep a file always cached, while mounted)
//
// The first time any mount command above actually runs, it's also
// registered to launch automatically at login (Windows/Mac only - see
// cmd/autostart). Manage this with:
//
//	zdrive autostart uninstall                                  (stop launching automatically)
//
// `zdrive login` saves type/url/token into rclone's own config file, so no
// env vars are needed afterward. RCLONE_CONFIG_ZDRIVE_TYPE/_URL/_TOKEN still
// work too (env vars win over the config file) - useful for CI/testing.
package main

import (
	"os"

	_ "github.com/rclone/rclone/backend/local" // VFS cache storage
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
	"github.com/vivekjaiswar/zdrive-desktop/cmd/autostart"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/login"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/pin"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/shutdownwait"
	"github.com/vivekjaiswar/zdrive-desktop/cmd/tray"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/webdavmount"

	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/cat"
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/lsl"
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/nfsmount"
	_ "github.com/rclone/rclone/cmd/serve"
	_ "github.com/rclone/rclone/cmd/serve/webdav"
	_ "github.com/rclone/rclone/cmd/version"
	"github.com/rclone/rclone/fs/config"
)

func main() {
	if os.Getenv("RCLONE_VOLNAME") == "" {
		os.Setenv("RCLONE_VOLNAME", "ZDrive") // drive label in Explorer/Finder
	}
	// As early as possible, before any command runs - so a failure in
	// *any* command (not just mount) still lands in the local event log.
	zdrive.SetupEventLogging(zdrive.EventLogPath(config.GetConfigPath()))
	// Zero arguments at all is exactly what a Finder/Launchpad
	// double-click produces (no way to pass a subcommand that way) -
	// if a mount is already registered for this binary, relaunch it
	// instead of falling through to cobra's "no command, print usage"
	// default. Replaces a separate relauncher .app bundle this project
	// used to install alongside the real one (2026-10-21) - having two
	// ZDrive icons in Spotlight/Launchpad was a real, repeated source of
	// confusion in hardware testing, not a cosmetic nitpick.
	if len(os.Args) == 1 && autostart.RelaunchIfRegistered() {
		return
	}
	// tray.RunMain is the real entrypoint, not cmd.Main() directly: on
	// macOS specifically, a mount command needs systray's own event loop
	// to own this actual main goroutine (see cmd/tray/entrypoint_darwin.go)
	// - everywhere else it's a direct passthrough to cmd.Main().
	tray.RunMain(cmd.Main)
}
