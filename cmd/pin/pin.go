// Package pin implements `zdrive pin`/`zdrive unpin` (M3 pin-to-keep-local):
// mark specific files on an already-mounted ZDrive as exempt from the
// local VFS cache's normal LRU eviction. See backend/zdrive/pin.go for the
// actual mechanism (no rclone fork needed - a daemon inside the running
// mount process holds an open handle on each pinned path).
//
// This file also wires the pin daemon's startup: it only makes sense
// running inside the long-lived mount/cmount/nfsmount process, not a
// short-lived `zdrive pin` invocation (which just edits the pin list and
// exits) - gated via cmd.Root's PersistentPreRunE, the same hook this file
// uses to wire --vfs-cache-max-size's own stated 10GB default for real
// (previously only a suggestion in README).
package pin

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

const defaultCacheMaxSize = "10G"

var listFlag bool

func init() {
	cmd.Root.AddCommand(pinCommand, unpinCommand)
	pinCommand.Flags().BoolVar(&listFlag, "list", false, "List currently pinned paths instead of pinning a new one.")

	prevPreRun := cmd.Root.PersistentPreRunE
	cmd.Root.PersistentPreRunE = func(command *cobra.Command, args []string) error {
		if prevPreRun != nil {
			if err := prevPreRun(command, args); err != nil {
				return err
			}
		}
		if !zdrive.IsMountCommand(command.Name()) {
			return nil
		}
		setDefaultCacheQuota(command)
		maxBytes := currentCacheMaxSizeBytes(command)
		daemon := zdrive.NewPinDaemon(statePath(), maxBytes)
		go daemon.Run(context.Background())
		return nil
	}
}

// setDefaultCacheQuota wires --vfs-cache-max-size's own already-stated
// 10GB default (docs/PLAN.md) for real, rather than leaving it as a
// suggestion in README - needed so the pin daemon's capacity guard means
// something. Only applies when the user hasn't explicitly passed their
// own value for this invocation.
func setDefaultCacheQuota(command *cobra.Command) {
	flag := command.Flags().Lookup("vfs-cache-max-size")
	if flag != nil && !flag.Changed {
		_ = flag.Value.Set(defaultCacheMaxSize)
	}
}

// currentCacheMaxSizeBytes reads back whatever --vfs-cache-max-size
// actually ends up as (the default just set above, or the user's own
// explicit value) - this is the one place that can see it: `zdrive pin`
// and `zdrive mount` are normally two separate OS processes, so a later
// `zdrive pin` invocation has no way to read the mount's in-memory flag
// value, which is why the capacity guard lives in the daemon (started
// here, with this real value) rather than in the pin/unpin commands below.
func currentCacheMaxSizeBytes(command *cobra.Command) int64 {
	flag := command.Flags().Lookup("vfs-cache-max-size")
	if flag == nil {
		return 0
	}
	var size fs.SizeSuffix
	if err := size.Set(flag.Value.String()); err != nil {
		return 0
	}
	return int64(size)
}

func statePath() string {
	return zdrive.PinStatePath(config.GetConfigPath())
}

var pinCommand = &cobra.Command{
	Use:   "pin <path>",
	Short: "Keep a file always cached locally, exempt from normal cache eviction.",
	Long: `Marks a file (its real path under the mounted drive, e.g.
"/home/you/ZDrive/Reports/Q3.xlsx") so the running mount never evicts it
from the local cache, regardless of how full the cache gets. Run
"zdrive pin --list" to see what's currently pinned. Only takes effect
while a mount is actually running - the mount process itself is what
holds pinned files open.`,
	RunE: func(command *cobra.Command, args []string) error {
		if listFlag {
			pins, err := zdrive.ListPins(statePath())
			if err != nil {
				return err
			}
			if len(pins) == 0 {
				fmt.Println("Nothing pinned.")
				return nil
			}
			for _, p := range pins {
				fmt.Println(p)
			}
			return nil
		}
		if len(args) != 1 {
			return fmt.Errorf("usage: zdrive pin <path>, or zdrive pin --list")
		}
		absPath, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		if err := zdrive.AddPin(statePath(), absPath); err != nil {
			return err
		}
		fmt.Printf("Pinned %s - it'll be fully cached within a few seconds if the mount is running.\n", absPath)
		return nil
	},
}

var unpinCommand = &cobra.Command{
	Use:   "unpin <path>",
	Short: "Remove a file from the pinned set.",
	RunE: func(command *cobra.Command, args []string) error {
		if len(args) != 1 {
			return fmt.Errorf("usage: zdrive unpin <path>")
		}
		absPath, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		return zdrive.RemovePin(statePath(), absPath)
	},
}
