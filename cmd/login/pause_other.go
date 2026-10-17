//go:build !windows

package login

// shouldPauseOnExit/pauseForEnter are no-ops outside Windows - macOS/
// Linux don't launch this binary via ShellExecute from an installer's
// finish page (the one scenario this guards against: a freshly-allocated
// console window that closes before a quick failure can be read), and on
// Linux this is expected to always run from an existing terminal session
// anyway.
func shouldPauseOnExit() bool { return false }

func pauseForEnter() {}
