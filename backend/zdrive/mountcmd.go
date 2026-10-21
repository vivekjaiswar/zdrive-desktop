package zdrive

// IsMountCommand reports whether name is one of this project's mount-shaped
// subcommands (cmd/mount, cmd/cmount, cmd/nfsmount, cmd/webdavmount) -
// confirmed via rclone's vendored source that cmount registers itself AS
// "mount" on non-Linux platforms (keeping "cmount" only as a cobra alias),
// so checking all three rclone names is correct regardless of platform or
// which alias the user typed. webdavmount is this project's own additive
// command (2026-10-21, not yet replacing nfsmount/cmount - see its own
// package doc), included here so it gets the same long-lived-process
// treatment as the others. Shared by every PersistentPreRunE hook that only
// makes sense inside a long-lived mount process (the pin daemon, autostart
// registration, the tray icon).
func IsMountCommand(name string) bool {
	switch name {
	case "mount", "cmount", "nfsmount", "webdavmount":
		return true
	}
	return false
}
