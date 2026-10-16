//go:build !darwin

package tray

// RunMain is main()'s entrypoint on every platform except macOS, where
// no main-thread inversion is needed: Windows' systray backend has no
// such restriction (tray startup happens via the existing
// PersistentPreRunE hook in tray.go, same as before entrypoint_darwin.go
// existed), and Linux never starts a tray at all. Just run mountFn
// directly.
func RunMain(mountFn func()) {
	mountFn()
}
