package zdrive

// IsMountCommand reports whether name is one of this project's three
// mount-shaped subcommands (cmd/mount, cmd/cmount, cmd/nfsmount) -
// confirmed via rclone's vendored source that cmount registers itself AS
// "mount" on non-Linux platforms (keeping "cmount" only as a cobra alias),
// so checking all three names is correct regardless of platform or which
// alias the user typed. Shared by every PersistentPreRunE hook that only
// makes sense inside a long-lived mount process (the pin daemon, autostart
// registration, the tray icon).
func IsMountCommand(name string) bool {
	switch name {
	case "mount", "cmount", "nfsmount":
		return true
	}
	return false
}
