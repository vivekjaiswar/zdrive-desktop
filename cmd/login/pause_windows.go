//go:build windows

package login

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
)

// shouldPauseOnExit reports whether this process is the only one attached
// to its console - true when Windows auto-allocated a fresh console
// window just for us (double-clicked, or launched via ShellExecute with
// no parent terminal - exactly what the installer's finish page's "Run"
// step does), false when we're already running inside a shell the user
// had open before (where pausing, or swallowing a non-zero exit code a
// script might check, would be an unwanted surprise).
func shouldPauseOnExit() bool {
	var pids [1]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), 1)
	return n == 1
}

// pauseForEnter blocks until the user presses Enter - call only when
// shouldPauseOnExit is true, and only after printing whatever the user
// needs to read first. Without this, a console-subsystem exe launched by
// double-click or the installer's finish page (both allocate a brand new
// console with nothing else attached) closes that window the instant the
// process exits - if sign-in failed quickly (a network/firewall issue
// reaching the API, for example), the error flashes and is gone before
// anyone can read it, indistinguishable from "nothing happened at all".
// That's exactly what a real customer's first-ever Windows install
// report described (2026-10-17): installed, then nothing - no sign-in,
// no visible error either.
func pauseForEnter() {
	fmt.Print("\nPress Enter to close this window...")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}
