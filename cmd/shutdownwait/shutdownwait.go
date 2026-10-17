// Package shutdownwait makes a real OS shutdown/reboot/logoff wait for
// any upload still sitting in the VFS write-back queue before this
// process is allowed to exit.
//
// Without this, --vfs-cache-mode full's background upload queue gets
// hard-cancelled the instant the OS signals the process to quit: a save
// that completes instantly to the local cache can be silently dropped
// if the machine reboots before the background upload to the real
// cloud storage finishes (confirmed 2026-10-15 - a Mac reboot right
// after an Excel save lost that save with zero error shown anywhere).
// Root cause, read directly from the vendored library: nfsmount's own
// unmount path (cmd/nfsmount/nfsmount.go) runs `diskutil umount force`
// and vfs.VFS.Shutdown() - which only cancels its context, never waits
// for anything - the moment rclone's own lib/atexit fires on
// SIGINT/SIGTERM. Same shape on Windows/Linux's mount backends; none of
// them wait for pending uploads either.
//
// This package takes over that responsibility: it disables lib/atexit's
// own signal-triggered shutdown (registerOnce there contains the only
// scheduler that would otherwise beat us to it), watches for the same
// signals itself, polls the already-built-in vfs/stats rc endpoint
// in-process (no network server needed - the call is registered process-
// wide the moment vfs/rc.go's own init() runs, which happens by the time
// any mount command is live) until the queue drains or a bounded
// timeout passes, then runs the normal (still fully intact) atexit
// handlers itself - so the real unmount/cleanup still happens exactly
// once, just after giving pending uploads a real chance to finish.
package shutdownwait

import (
	"context"
	"os"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"github.com/rclone/rclone/lib/atexit"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

// maxDrainWait bounds how long a shutdown/reboot waits for pending
// uploads - long enough for a normal save to finish, short enough that
// the OS's own "force-kill a slow-to-exit background process" timeout
// (both macOS and Windows apply one) doesn't just kill us mid-wait
// having gained nothing.
const maxDrainWait = 20 * time.Second

// maxUnmountWait bounds the real unmount/cleanup step (atexit.Run(),
// which runs nfsmount's own `diskutil umount force`). Confirmed on real
// hardware (2026-10-17): that call can hang indefinitely - not a Go-level
// deadlock, a macOS kernel-level NFS vnode wedge (the process became
// unkillable even by SIGKILL, "trying to exit" in ps forever). Nothing
// in this package can fix a kernel wedge, but it must never again leave
// our own process waiting on it forever - better to exit having
// abandoned a stuck unmount than to become a permanently stuck process
// that blocks every future mount attempt at this path with "already
// mounted" until a reboot.
const maxUnmountWait = 15 * time.Second

func init() {
	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		if zdrive.IsMountCommand(command.Name()) {
			watchForShutdown()
		}
		return nil
	}
}

// watchForShutdown disables lib/atexit's own signal handling (so it
// never races us to cancel the VFS first) and starts our own goroutine
// that waits for the drain, then runs the real shutdown sequence.
func watchForShutdown() {
	atexit.IgnoreSignals()
	sigCh := make(chan os.Signal, 1)
	notifySignals(sigCh)
	go func() {
		defer zdrive.RecoverAndLog("shutdownwait")
		sig := <-sigCh
		if sig == nil {
			return
		}
		fs.Logf(nil, "shutdown signal received (%s) - waiting up to %s for pending uploads", sig, maxDrainWait)
		drainUploads()
		runCleanupWithTimeout()
		os.Exit(0)
	}()
}

// runCleanupWithTimeout runs atexit.Run() (the real unmount/cleanup) but
// never waits on it past maxUnmountWait - see that constant's comment.
func runCleanupWithTimeout() {
	runWithTimeout(atexit.Run, maxUnmountWait)
}

// runWithTimeout runs fn in the background and returns once it finishes
// or timeout passes, whichever is first - fn is allowed to keep running
// (and even finish) after a timeout, this just stops a stuck fn from
// being able to block the caller forever. Split out from
// runCleanupWithTimeout so a test can drive it with a fake hanging fn,
// without touching rclone's real (global, shared) atexit state.
func runWithTimeout(fn func(), timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		fn()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		fs.Logf(nil, "shutdown: unmount/cleanup didn't finish within %s - exiting anyway", timeout)
	}
}

// drainUploads polls the VFS write-back queue and returns as soon as
// it's empty, or after maxDrainWait, whichever comes first - a bounded
// wait, not a guarantee: a large or genuinely offline upload may still
// not finish in time, but this closes the common case (a small file,
// mid-upload or just-queued, with the network actually up).
func drainUploads() {
	call := rc.Calls.Get("vfs/stats")
	if call == nil {
		return // shouldn't happen - vfs/rc.go always registers this
	}
	deadline := time.Now().Add(maxDrainWait)
	for time.Now().Before(deadline) {
		out, err := call.Fn(context.Background(), rc.Params{})
		if err != nil {
			return // no active VFS left, or a genuine lookup error - nothing more we can do
		}
		diskCache, ok := out["diskCache"].(rc.Params)
		if !ok {
			return // --vfs-cache-mode off, or cache not set up - nothing to wait for
		}
		inProgress, _ := diskCache["uploadsInProgress"].(int)
		queued, _ := diskCache["uploadsQueued"].(int)
		if inProgress == 0 && queued == 0 {
			return
		}
		fs.Logf(nil, "shutdown: waiting on %d in-progress + %d queued upload(s)", inProgress, queued)
		time.Sleep(500 * time.Millisecond)
	}
	fs.Logf(nil, "shutdown: gave up waiting for uploads after %s - some recent changes may not have synced", maxDrainWait)
}
