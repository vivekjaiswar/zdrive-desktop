//go:build !windows && !plan9

package shutdownwait

import (
	"os"
	"os/signal"
	"syscall"
)

// notifySignals mirrors lib/atexit's own unix signal list exactly (that
// package keeps it unexported, so there's no way to reuse it directly)
// - macOS/Linux shutdown delivers SIGTERM to a background process, not
// SIGINT, so both need to be covered the same way atexit itself does.
func notifySignals(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
}
