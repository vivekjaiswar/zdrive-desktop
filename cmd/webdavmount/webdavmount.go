// Package webdavmount mounts a remote via WebDAV using each OS's own
// built-in WebDAV client (mount_webdav on macOS, `net use` on Windows) -
// no third-party driver needed, unlike WinFsp (Windows) or even a system
// NFS client that isn't present on Windows Home. Added 2026-10-21 per a
// standing-decision request: mac and windows should use the identical
// underlying mechanism. ADDITIVE, not a replacement: cmd/nfsmount and
// cmd/cmount (WinFsp) still exist and are what's actually shipped and
// verified on real hardware today - this command exists so the WebDAV
// path can be tested on real Mac/Windows machines before anything gets
// removed. See the 2026-10-21 design doc for the full tradeoff
// (identical mechanism vs. a known 50MB default file-size cap on
// Windows' built-in WebDAV client, vs. re-verifying atomic-save/pin
// behavior against WebDAV's different locking semantics).
//
// rclone's own `serve webdav` has no exported constructor (unlike
// `serve nfs`, which cmd/nfsmount already uses in-process via
// nfs.NewServer) - newWebDAV is unexported. So this runs the WebDAV
// server as this same binary re-executed as a child process
// (`zdrive serve webdav ...`, already wired into main.go), rather than
// nfsmount's cleaner in-process pattern. A real, acknowledged tradeoff,
// not an oversight.
package webdavmount

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfsflags"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func init() {
	vfsflags.AddFlags(command.Flags())
	cmd.Root.AddCommand(command)
}

var command = &cobra.Command{
	Use:   "webdavmount remote:path /path/to/mountpoint",
	Short: "Mount a remote via WebDAV (macOS/Windows only, no third-party driver).",
	Long: `Mounts remote:path at the given mountpoint using the operating system's
own built-in WebDAV client - no WinFsp, no macFUSE, no extra driver install.

Experimental and additive: cmd/nfsmount (macOS) and cmd/cmount (Windows,
WinFsp) remain this project's verified, shipped mount commands. Use this
one to test the WebDAV path on real hardware before anything is removed.

Accepts the same --vfs-cache-mode / --vfs-cache-max-size flags as
"zdrive mount".`,
	Args: cobra.ExactArgs(2),
	RunE: run,
}

func run(cmd *cobra.Command, args []string) error {
	remote, mountpoint := args[0], args[1]

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding own executable: %w", err)
	}

	addr, err := freeLocalAddr()
	if err != nil {
		return fmt.Errorf("picking a local port for the WebDAV server: %w", err)
	}

	childArgs := []string{"serve", "webdav", remote, "--addr", addr}
	// Forward every flag the user actually passed (vfs-cache-mode, etc.)
	// verbatim to the child - this command shares vfsflags.AddFlags with
	// `serve webdav` itself, so every flag name it understands is valid
	// here too, without hand-listing each one.
	cmd.Flags().Visit(func(f *pflag.Flag) {
		childArgs = append(childArgs, "--"+f.Name, f.Value.String())
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	child := exec.CommandContext(ctx, execPath, childArgs...)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return fmt.Errorf("starting WebDAV server: %w", err)
	}
	// Watched (not just reaped by a bare defer Wait()) so that if the
	// child dies on its own - crash, OOM, anything - this process
	// notices and unmounts instead of leaving the OS's WebDAV mount
	// pointing at a server that no longer exists. Confirmed as a real
	// failure mode on real hardware (2026-10-21): a closed terminal tab
	// sends SIGHUP, which this process didn't handle before, so it died
	// without ever reaching the unmount step below - macOS's own
	// webdavfs_agent then kept the mount "up" but non-functional
	// ("Failed to stat node" on every access) until manually unmounted.
	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()

	if err := waitForServer(addr, 10*time.Second); err != nil {
		cancel()
		return fmt.Errorf("WebDAV server never became reachable: %w", err)
	}

	url := "http://" + addr + "/"
	if err := nativeMount(url, mountpoint); err != nil {
		cancel()
		return fmt.Errorf("mounting via the OS's native WebDAV client: %w", err)
	}
	fs.Logf(nil, "Mounted %s at %s via WebDAV (%s)", remote, mountpoint, url)

	// SIGHUP included alongside the usual INT/TERM specifically for the
	// closed-terminal case above - this is the signal a controlling
	// terminal's hangup actually sends, not INT or TERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-sigCh:
	case err := <-childDone:
		fs.Errorf(nil, "WebDAV server exited unexpectedly: %v", err)
	}

	if err := nativeUnmount(mountpoint); err != nil {
		fs.Errorf(nil, "unmount failed: %v", err)
	}
	cancel()
	return nil
}

// freeLocalAddr asks the OS for a free port the same way nfsmount relies
// on the OS picking one, then releases it immediately so the child
// process can bind it - a small, acknowledged race (another process
// could grab the port first), acceptable for this additive/experimental
// command; nfsmount doesn't have this race since its server runs
// in-process rather than as a separate child.
func freeLocalAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr, nil
}

func waitForServer(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s", timeout)
}
