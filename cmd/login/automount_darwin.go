//go:build darwin

package login

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// autoMountAfterLogin launches a background mount of ~/ZDrive immediately
// after a successful sign-in - see automount_windows.go's comment for
// the full rationale (same gap, same fix; different OS mount command and
// detachment mechanism). Setsid detaches this process from the
// controlling session/terminal (login's own process, or the installer
// that ran it) so the mount survives that process's own exit.
func autoMountAfterLogin() {
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("(couldn't auto-mount: %v)\n", err)
		return
	}
	mountPoint := filepath.Join(home, "ZDrive")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		fmt.Printf("(couldn't auto-mount: %v)\n", err)
		return
	}
	hint := fmt.Sprintf("zdrive nfsmount zdrive: %s --vfs-cache-mode full", mountPoint)
	execPath, err := os.Executable()
	if err != nil {
		fmt.Printf("(couldn't auto-mount: %v - run %s yourself)\n", err, hint)
		return
	}
	cmd := exec.Command(execPath, "nfsmount", "zdrive:", mountPoint, "--vfs-cache-mode", "full")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Printf("(couldn't auto-mount: %v - run %s yourself)\n", err, hint)
	}
}
