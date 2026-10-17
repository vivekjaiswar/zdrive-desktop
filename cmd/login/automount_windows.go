//go:build windows

package login

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// autoMountAfterLogin launches a background mount of Z: immediately
// after a successful sign-in, so a non-technical user sees their drive
// appear with zero terminal interaction. The installer's finish page
// already gets them this far with no terminal needed at all (it runs
// "zdrive login" itself) - stopping short of actually mounting left a
// real gap for exactly the audience this needs to work for. "Z:" matches
// every existing doc/example in this project - a known, accepted v1
// limitation: this doesn't check for an already-in-use Z: drive first.
// The resulting mount run is also what triggers autostart registration
// (cmd/autostart) and starts the tray (cmd/tray), via the exact same
// PersistentPreRunE hooks any other mount invocation goes through -
// nothing extra needed here for those to kick in.
func autoMountAfterLogin() {
	args := []string{"mount", "zdrive:", "Z:", "--vfs-cache-mode", "full"}
	execPath, err := os.Executable()
	if err != nil {
		fmt.Printf("(couldn't auto-mount: %v - run zdrive %s yourself)\n", err, strings.Join(args, " "))
		return
	}
	cmd := exec.Command(execPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		fmt.Printf("(couldn't auto-mount: %v - run %s %s yourself)\n", err, execPath, strings.Join(args, " "))
		return
	}
	// Explicit success confirmation, not silence - this prints into the
	// same console pauseBeforeExit (pause_windows.go) keeps open, so a
	// freshly-installed user always sees *something* happened, good or
	// bad, instead of a window that looked empty either way.
	fmt.Println("Mounting Z: now - check File Explorer in a few seconds.")
}
