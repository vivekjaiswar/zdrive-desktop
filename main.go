// zdrive is rclone with the ZDrive backend compiled in and only the
// commands the desktop drive needs.
//
//	zdrive mount zdrive: ~/ZDrive --vfs-cache-mode full        (Linux)
//	zdrive nfsmount zdrive: ~/ZDrive --vfs-cache-mode full     (macOS, no macFUSE)
//	zdrive mount zdrive: Z: --vfs-cache-mode full              (Windows, WinFsp)
//
// Configure with RCLONE_CONFIG_ZDRIVE_TYPE=zdrive and RCLONE_CONFIG_ZDRIVE_TOKEN=<jwt>.
package main

import (
	"os"

	_ "github.com/rclone/rclone/backend/local" // VFS cache storage
	_ "github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"

	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/cat"
	_ "github.com/rclone/rclone/cmd/cmount"
	_ "github.com/rclone/rclone/cmd/lsl"
	_ "github.com/rclone/rclone/cmd/mount"
	_ "github.com/rclone/rclone/cmd/nfsmount"
	_ "github.com/rclone/rclone/cmd/version"
)

func main() {
	// ponytail: M1 backend can't upload, so without this the VFS cache would
	// accept writes locally and lose them at upload time. Drop with M2 writes.
	os.Setenv("RCLONE_READ_ONLY", "true")
	if os.Getenv("RCLONE_VOLNAME") == "" {
		os.Setenv("RCLONE_VOLNAME", "ZDrive") // drive label in Explorer/Finder
	}
	cmd.Main()
}
