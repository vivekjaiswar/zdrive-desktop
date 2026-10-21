//go:build darwin

package autostart

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const label = "com.zennialhub.zdrive"

// Deliberately not launchd-meaningful - ZDriveExecPath is read back by
// isInstalled to detect a binary that moved since this was written (e.g.
// after a reinstall to a different folder). launchd itself ignores plist
// keys it doesn't recognize, so this is safe to include.
const plistTpl = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
%s	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ZDriveExecPath</key>
	<string>%s</string>
</dict>
</plist>
`

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

func launchdTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func xmlEscape(s string) (string, error) {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

func install(execPath string, mountArgs []string) error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	var argsXML bytes.Buffer
	for _, a := range append([]string{execPath}, mountArgs...) {
		escaped, err := xmlEscape(a)
		if err != nil {
			return err
		}
		fmt.Fprintf(&argsXML, "\t\t<string>%s</string>\n", escaped)
	}

	escapedExecPath, err := xmlEscape(execPath)
	if err != nil {
		return err
	}

	content := fmt.Sprintf(plistTpl, label, argsXML.String(), escapedExecPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}

	// Unload first (ignore error - fine if it wasn't loaded yet) so a
	// re-install with changed args/path actually takes effect immediately,
	// not just on next login.
	_ = exec.Command("launchctl", "bootout", launchdTarget()+"/"+label).Run()
	return exec.Command("launchctl", "bootstrap", launchdTarget(), path).Run()
}

// relauncherAppName is the name of a now-removed separate "relauncher"
// .app bundle this package used to write to ~/Applications on every
// mount run, purely so Spotlight/Finder had something to double-click
// after quitting ZDrive from the tray (no KeepAlive, by design). Removed
// 2026-10-21: the real ZDrive.app bundle installed to /Applications (see
// installer/macos) made it redundant, and having BOTH visible in
// Spotlight/Launchpad at once was confusing - confirmed as a real,
// repeated complaint across several rounds of hardware testing, not a
// one-off. relaunchIfRegistered below replaces it: the real app bundle's
// own binary now relaunches the mount itself when double-clicked, no
// second bundle needed. Kept only as the uninstall cleanup target below,
// for anyone who still has the old one from a prior version.
const relauncherAppName = "ZDrive.app"

// relaunchIfRegistered re-kicks the registered LaunchAgent
// (`launchctl kickstart`, which starts it whether currently stopped or
// already running) if one is registered for this exact binary, and
// reports whether it did. Called from main.go when zdrive is launched
// with no arguments at all - exactly what happens on a Finder/Launchpad
// double-click - so double-clicking the real installed app bundle
// relaunches the mount instead of just printing cobra's usage text.
func relaunchIfRegistered() bool {
	execPath, err := os.Executable()
	if err != nil {
		return false
	}
	registeredPath, ok, err := isInstalled()
	if err != nil || !ok || registeredPath != execPath {
		return false
	}
	return exec.Command("launchctl", "kickstart", launchdTarget()+"/"+label).Run() == nil
}

func uninstall() error {
	path, err := plistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchdTarget()+"/"+label).Run()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if home, err := os.UserHomeDir(); err == nil {
		// Best-effort, not fatal: cleans up the old relauncher bundle
		// (see its own comment above) for anyone upgrading from a prior
		// version that still created one.
		_ = os.RemoveAll(filepath.Join(home, "Applications", relauncherAppName))
	}
	return nil
}

func isInstalled() (registeredPath string, ok bool, err error) {
	path, err := plistPath()
	if err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return extractExecPath(data)
}

// extractExecPath pulls just the ZDriveExecPath value back out of a
// plist this package itself wrote - using encoding/xml's token decoder
// rather than hand string-scanning, so entity-escaped characters (a path
// containing & or <) still round-trip correctly.
func extractExecPath(data []byte) (string, bool, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	afterKey := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			var key string
			if err := dec.DecodeElement(&key, &start); err != nil {
				return "", false, err
			}
			afterKey = key == "ZDriveExecPath"
		case "string":
			var val string
			if err := dec.DecodeElement(&val, &start); err != nil {
				return "", false, err
			}
			if afterKey {
				return val, true, nil
			}
		}
	}
}
