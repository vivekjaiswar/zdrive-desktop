//go:build linux

package login

// autoMountAfterLogin is a no-op on Linux - dev-only for this project
// (per docs/PLAN.md), where "zdrive mount" by hand is the expected
// workflow. doLogin's own printed "Run: zdrive mount ..." instruction
// already covers this audience.
func autoMountAfterLogin() {}
