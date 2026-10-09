//go:build linux

package autostart

import "fmt"

// Linux is dev-only for this project (per docs/PLAN.md) - not worth
// building a desktop-autostart mechanism (e.g. an XDG autostart .desktop
// entry) for an audience that already runs `zdrive mount` by hand.
// ensureRegistered (autostart.go) skips Linux entirely via its own
// runtime.GOOS check before any of this is reached from the silent
// per-mount hook; these three only run if a user explicitly invokes
// `zdrive autostart install/uninstall` themselves, where a clear error is
// the right answer.

func install(execPath string, mountArgs []string) error {
	return fmt.Errorf("autostart isn't supported on this platform build (Linux is dev-only)")
}

func uninstall() error {
	return fmt.Errorf("autostart isn't supported on this platform build (Linux is dev-only)")
}

func isInstalled() (registeredPath string, ok bool, err error) {
	return "", false, fmt.Errorf("autostart isn't supported on this platform build (Linux is dev-only)")
}
