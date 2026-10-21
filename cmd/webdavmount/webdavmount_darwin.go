//go:build darwin

package webdavmount

import (
	"fmt"
	"os"
	"os/exec"
)

// nativeMount uses macOS's own mount_webdav - the same binary Finder's
// "Connect to Server" uses for a WebDAV URL, so this needs no extra
// software (unlike macFUSE). mount_webdav requires the mountpoint
// directory to already exist.
func nativeMount(url, mountpoint string) error {
	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		return fmt.Errorf("creating mountpoint: %w", err)
	}
	out, err := exec.Command("mount_webdav", "-S", url, mountpoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func nativeUnmount(mountpoint string) error {
	out, err := exec.Command("umount", mountpoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}
