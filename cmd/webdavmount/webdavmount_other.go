//go:build !darwin && !windows

package webdavmount

import "fmt"

// Linux already has a working native mount (cmd/mount, cmd/cmount) with
// no WinFsp-equivalent driver problem - this command exists specifically
// for the Mac/Windows identical-mechanism goal, so it's a clear error
// here rather than a half-working stub.
func nativeMount(url, mountpoint string) error {
	return fmt.Errorf("webdavmount is only supported on macOS and Windows - use `zdrive mount` on this platform")
}

func nativeUnmount(mountpoint string) error {
	return fmt.Errorf("webdavmount is only supported on macOS and Windows")
}
