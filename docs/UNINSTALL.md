# Uninstalling ZDrive

## Windows

Use **Settings → Apps → Installed apps** (or the classic Control Panel →
Programs and Features) and remove **ZDrive**, same as any other app.

This runs the bundled uninstaller, which:

- Stops ZDrive from launching at login (`zdrive autostart uninstall`)
- Removes the binary, Start Menu shortcut, and its own registry entries
- Removes a leftover `.old` file if an auto-update was staged but you
  never relaunched afterward

**Left in place on purpose:**

- **WinFsp** (the Windows driver mounting needs) - a shared system
  component other apps may also depend on, same as a VC++ redistributable.
  Uninstall it separately via its own entry in Installed apps if you're
  sure nothing else needs it.
- **Your ZDrive sign-in** (`%APPDATA%\rclone\rclone.conf`) and the local
  event log next to it (`zdrive-events.log`) - so reinstalling doesn't
  make you sign in again. Delete that file yourself if you want a fully
  clean slate.
- Whatever folder you mounted ZDrive to (e.g. `Z:\` or a folder you
  picked) - that's just a regular folder once unmounted, not something
  the uninstaller should touch.

## macOS

There's no Applications-folder app to drag to the Trash - installing
from the `.pkg` just places one file. Three steps:

```sh
zdrive autostart uninstall   # stops it launching at login
sudo rm /usr/local/bin/zdrive
```

Optional, only if you want to remove the local mirror of your files too
(your actual files stay safe in ZDrive's cloud storage either way - this
only removes the local copy the mount kept on disk):

```sh
rm -rf ~/ZDrive   # or wherever you mounted it
```

**Left in place on purpose**, same reasoning as Windows: your sign-in
(`~/.config/rclone/rclone.conf`) and the local event log next to it
(`zdrive-events.log`) - useful for post-mortems, and means a reinstall
doesn't make you sign in again. Remove that file yourself for a fully
clean slate.

## Before testing a beta build

If you're trying an opt-in beta release, run through this full uninstall
first on a disposable VM or throwaway machine - not your daily-driver
computer - before trusting it with real files. Kernel-adjacent software
like a mounted drive fails by hanging or crashing Explorer/Finder
entirely, not just erroring in one app, so confirm a clean mount, write,
rename, delete, pin, and uninstall cycle works before relying on it.
