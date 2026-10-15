//go:build windows || plan9

package shutdownwait

import (
	"os"
	"os/signal"
)

// notifySignals mirrors lib/atexit's own windows/plan9 signal list
// exactly - Go's runtime already maps console close/logoff/shutdown
// events to os.Interrupt on Windows, which is what atexit itself relies
// on there too.
func notifySignals(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
