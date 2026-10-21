//go:build windows

package webdavmount

import (
	"fmt"
	"os/exec"
)

// nativeMount uses Windows' own built-in WebDAV client (the WebClient
// service, same thing Explorer's "Map Network Drive" uses for a WebDAV
// URL) via `net use` - no WinFsp, no driver install. mountpoint here is
// a drive letter like "Z:", matching the existing `zdrive mount` UX.
//
// Known limitation, not yet worked around: Windows' built-in WebDAV
// client caps file size at 50MB by default (registry value
// FileSizeLimitInBytes under
// HKLM\SYSTEM\CurrentControlSet\Services\WebClient\Parameters) - real
// files larger than that will fail to upload/download until that's
// raised, which this command does not do automatically yet.
func nativeMount(url, mountpoint string) error {
	out, err := exec.Command("net", "use", mountpoint, url, "/persistent:no").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}

func nativeUnmount(mountpoint string) error {
	out, err := exec.Command("net", "use", mountpoint, "/delete", "/y").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	return nil
}
