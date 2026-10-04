// zdrive is rclone with the ZDrive backend compiled in and only the
// commands the desktop drive needs.
//
//	zdrive login                                                (once, opens a browser to approve this device)
//	zdrive mount zdrive: ~/ZDrive --vfs-cache-mode full        (Linux)
//	zdrive nfsmount zdrive: ~/ZDrive --vfs-cache-mode full     (macOS, no macFUSE)
//	zdrive mount zdrive: Z: --vfs-cache-mode full              (Windows, WinFsp)
//	zdrive pin ~/ZDrive/Reports/Q3.xlsx                         (keep a file always cached, while mounted)
//
// `zdrive login` saves type/url/token into rclone's own config file, so no
// env vars are needed afterward. RCLONE_CONFIG_ZDRIVE_TYPE/_URL/_TOKEN still
// work too (env vars win over the config file) - useful for CI/testing.
package main

import (
	"os"

	_ "github.com/rclone/rclone/backend/local" // VFS cache storage
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/login"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/pin"

	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/cat"
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/lsl"
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/nfsmount"
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
	cmd.Main()
}
