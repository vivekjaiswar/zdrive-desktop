//go:build windows

package tray

import (
	"fmt"
	"os"
	"os/exec"
)

// applyUpdate replaces the running executable at execPath with the
// already-downloaded-and-checksum-verified stagedPath, then launches the
// new binary with this process's own arguments. The caller is
// responsible for exiting the old process afterward (quitMount).
func applyUpdate(execPath, stagedPath string) error {
	if err := swapBinary(execPath, stagedPath); err != nil {
		return err
	}
	return launchUpdated(execPath)
}

// swapBinary does the actual file replacement - split out from
// applyUpdate so it's testable without spawning a real child process.
//
// Windows won't let you overwrite or delete a running exe's own image
// file, but renaming it IS allowed - the loader doesn't take a lock
// against rename, only against delete/overwrite. This is the same
// technique established Go self-update tools use (e.g.
// rhysd/go-github-selfupdate): rename the running binary aside, move the
// staged one into its place. The old binary is left behind as
// execPath+".old" (deleting it immediately would hit the exact lock this
// dance exists to avoid, since it's still this process's own open image)
// - cleaned up on the next successful run instead, see cleanupOldBinary.
func swapBinary(execPath, stagedPath string) error {
	oldPath := execPath + ".old"
	_ = os.Remove(oldPath) // leftover from a previous update, if any - best-effort

	if err := os.Rename(execPath, oldPath); err != nil {
		return fmt.Errorf("rename running binary aside: %w", err)
	}
	if err := os.Rename(stagedPath, execPath); err != nil {
		// Best-effort: put the original back so the user isn't left with
		// a missing binary. The update still failed either way.
		_ = os.Rename(oldPath, execPath)
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

// cleanupOldBinary removes a ".old" file left behind by a previous
// applyUpdate, if any. Called once at tray startup rather than right
// after launching the new process in applyUpdate, since the OLD process
// (which just renamed itself aside) is the one still running at that
// point - it can't reliably delete its own just-renamed image file
// either, for the same reason it couldn't delete/overwrite it in the
// first place. The NEW process, once it starts up fresh, has no such
// restriction.
func cleanupOldBinary() {
	execPath, err := os.Executable()
	if err != nil {
		return
	}
	cleanupOldBinaryAt(execPath)
}

func cleanupOldBinaryAt(execPath string) {
	_ = os.Remove(execPath + ".old")
}
