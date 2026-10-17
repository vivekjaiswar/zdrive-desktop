//go:build darwin

package tray

import (
	"os"

	"fyne.io/systray"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

// RunMain is main()'s entrypoint on macOS. fyne.io/systray's macOS
// backend drives real Cocoa/AppKit calls via cgo, which (like all AppKit
// calls) must run on the actual main OS thread - calling it from a
// goroutine crashed with a bare SIGTRAP on real hardware (confirmed
// 2026-10-14, fixed by disabling the tray on macOS entirely until this
// proper fix landed). So for a real mount command, this inverts the
// normal flow: systray.Run() itself occupies this (real main) goroutine,
// and mountFn (what main() would otherwise call directly, i.e.
// cmd.Main()) runs on a spawned goroutine instead, started from
// systray's own onReady callback once the tray is up.
//
// Everything else (login, pin, version, ...) never needs a tray at all,
// so it just calls mountFn directly, same as any other platform.
func RunMain(mountFn func()) {
	if !isMountInvocation() {
		mountFn()
		return
	}
	systray.Run(func() {
		go func() {
			defer zdrive.RecoverAndLog("mount (owned by tray main thread)")
			mountFn()
			// mountFn (cmd.Main()) returning means the mount itself has
			// ended, one way or another. cmd.Main()'s own error path
			// already calls os.Exit() directly, so this line only matters
			// for a *clean* return - which real hardware testing
			// 2026-10-17 showed does happen: ejecting the volume from
			// Finder tears the mount down cleanly without going through
			// our shutdownwait signal handler at all. Before main()
			// called RunMain, that clean return happened on the real
			// main goroutine, so the process exited naturally right
			// after. Now it happens on this spawned goroutine instead -
			// without an explicit exit here, systray.Run() on the real
			// main goroutine has no idea the mount is gone and keeps the
			// whole process alive indefinitely, silently orphaned with a
			// tray and no mount behind it.
			os.Exit(0)
		}()
		onReady()
	}, func() {})
}

// isMountInvocation peeks at os.Args before cobra has parsed anything, to
// decide whether main() needs the tray-owns-main-thread inversion above
// at all. Assumes the subcommand is the first argument (os.Args[1]) with
// no global flag before it - true for every real invocation this project
// documents/generates (docs, cmd/autostart's saved command) - not a full
// cobra-equivalent parse. zdrive.IsMountCommand itself (the PersistentPreRunE
// hook in tray.go, which still gates the equivalent Windows path) is the
// authoritative check once cobra has actually run.
func isMountInvocation() bool {
	return len(os.Args) > 1 && zdrive.IsMountCommand(os.Args[1])
}
