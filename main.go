// zdrive is rclone with the ZDrive backend compiled in and only the
// commands the desktop drive needs.
//
//	zdrive login                                                (once, opens a browser to approve this device)
//	zdrive mount zdrive: ~/ZDrive --vfs-cache-mode full        (Linux)
//	zdrive nfsmount zdrive: ~/ZDrive --vfs-cache-mode full     (macOS, no macFUSE)
//	zdrive mount zdrive: Z: --vfs-cache-mode full              (Windows, WinFsp)
//
// `zdrive login` saves type/url/token into rclone's own config file, so no
// env vars are needed afterward. RCLONE_CONFIG_ZDRIVE_TYPE/_URL/_TOKEN still
// work too (env vars win over the config file) - useful for CI/testing.
package main

import (
	"os"

	_ "github.com/rclone/rclone/backend/local" // VFS cache storage
	_ "github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
	_ "github.com/vivekjaiswar/zdrive-desktop/cmd/login"

	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/cat"
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/lsl"
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/nfsmount"
	_ "github.com/rclone/rclone/cmd/version"
)

func main() {
	if os.Getenv("RCLONE_VOLNAME") == "" {
		os.Setenv("RCLONE_VOLNAME", "ZDrive") // drive label in Explorer/Finder
	}
	cmd.Main()
}
