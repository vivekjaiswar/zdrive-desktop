//go:build !windows

// Within cmd/tray's own build scope (windows || darwin, see tray.go),
// !windows unambiguously means darwin - tray_linux.go never references
// applyUpdate/cleanupOldBinary at all.
package tray

import (
	"fmt"
	"os"
	"os/exec"
)

// applyUpdate replaces the running executable directly, then launches
// the new binary with this process's own arguments.
func applyUpdate(execPath, stagedPath string) error {
	if err := swapBinary(execPath, stagedPath); err != nil {
		return err
	}
	return launchUpdated(execPath)
}

// swapBinary does the actual file replacement - split out from
// applyUpdate so it's testable without spawning a real child process.
// POSIX allows renaming or removing a file that's currently executing;
// the running process keeps its already-mapped inode alive until it
// exits, so this is safe even while still running - no rename-aside
// dance needed here, unlike Windows.
func swapBinary(execPath, stagedPath string) error {
	if err := os.Rename(stagedPath, execPath); err != nil {
		return fmt.Errorf("move staged binary into place: %w", err)
	}
	return nil
}

func launchUpdated(execPath string) error {
	cmd := exec.Command(execPath, os.Args[1:]...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch updated binary: %w", err)
	}
	return nil
}

// cleanupOldBinary is a no-op here - only Windows's applyUpdate leaves a
// ".old" file behind (see update_windows.go for why).
func cleanupOldBinary() {}
