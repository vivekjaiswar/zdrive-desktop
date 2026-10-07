// Package autostart registers/unregisters zdrive to launch automatically
// at login (M4). Per-OS mechanism lives in autostart_<os>.go; this file
// is the shared cobra wiring plus the same PersistentPreRunE-chaining
// trick cmd/pin already established: the first time an actual mount
// command runs (however the user launched it - installer finish page,
// Start Menu shortcut, or just typing the command), silently (re-)
// register autostart for it. This always executes as the real logged-in
// user, so neither the Windows installer nor the macOS .pkg's postinstall
// script needs to do anything with elevated/root permissions to wire
// this up.
package autostart

import (
	"fmt"
	"os"
	"runtime"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

func init() {
	autostartCommand.AddCommand(installCommand, uninstallCommand)
	cmd.Root.AddCommand(autostartCommand)

	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		if zdrive.IsMountCommand(command.Name()) {
			ensureRegistered(os.Args[1:])
		}
		return nil
	}
}

// ensureRegistered registers autostart for this exact invocation (the
// current binary plus the mount args the user launched with) if it isn't
// already registered, or repairs it if it's pointing at a stale binary
// path (e.g. after a reinstall to a different folder). Best-effort: a
// user who can already see their mount working shouldn't have that
// broken by an autostart registration hiccup, so failures are logged,
// not returned/fatal.
func ensureRegistered(mountArgs []string) {
	if runtime.GOOS == "linux" {
		return // dev-only platform for this project - see autostart_linux.go
	}

	execPath, err := os.Executable()
	if err != nil {
		fs.Logf(nil, "autostart: couldn't resolve this binary's own path: %v", err)
		return
	}

	registeredPath, ok, err := isInstalled()
	if err != nil {
		fs.Logf(nil, "autostart: couldn't check existing registration: %v", err)
		return
	}
	if ok && registeredPath == execPath {
		return
	}

	if err := install(execPath, mountArgs); err != nil {
		fs.Logf(nil, "autostart: couldn't register: %v", err)
	}
}

var autostartCommand = &cobra.Command{
	Use:   "autostart",
	Short: "Manage whether ZDrive launches automatically at login.",
}

var installCommand = &cobra.Command{
	Use:   "install -- <mount-args...>",
	Short: "Launch this exact mount automatically at login.",
	Long: `Registers the current binary plus the given mount arguments (e.g.
"zdrive autostart install -- mount zdrive: /home/you/ZDrive --vfs-cache-mode full")
to run automatically at login. You normally don't need to run this
yourself - it happens the first time you actually run a mount command -
this exists for scripting/reinstall scenarios.`,
	RunE: func(command *cobra.Command, args []string) error {
		if len(args) == 0 {
			return fmt.Errorf("usage: zdrive autostart install -- <mount-args...>")
		}
		execPath, err := os.Executable()
		if err != nil {
			return err
		}
		return install(execPath, args)
	},
}

var uninstallCommand = &cobra.Command{
	Use:   "uninstall",
	Short: "Stop launching ZDrive automatically at login.",
	RunE: func(command *cobra.Command, args []string) error {
		return uninstall()
	},
}
