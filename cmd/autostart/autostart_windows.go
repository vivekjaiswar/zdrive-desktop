//go:build windows

package autostart

import (
	"fmt"
	"strings"

	"golang.org/x/sys/windows/registry"
)

const (
	runKeyPath   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValueName = "ZDrive"
	// A second, non-functional value alongside the real Run-key command
	// line, holding just the bare exec path - lets ensureRegistered check
	// "is this still pointing at the right binary" without having to
	// parse the quoted command line back apart.
	execPathValueName = "ZDrive_ExecPath"
)

func install(execPath string, mountArgs []string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Run key: %w", err)
	}
	defer key.Close()

	cmdLine := quoteWindowsCommandLine(append([]string{execPath}, mountArgs...))
	if err := key.SetStringValue(runValueName, cmdLine); err != nil {
		return fmt.Errorf("set Run value: %w", err)
	}
	if err := key.SetStringValue(execPathValueName, execPath); err != nil {
		return fmt.Errorf("set exec-path value: %w", err)
	}
	return nil
}

func uninstall() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return err
	}
	defer key.Close()

	_ = key.DeleteValue(execPathValueName)
	if err := key.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}

func isInstalled() (registeredPath string, ok bool, err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return "", false, nil
		}
		return "", false, err
	}
	defer key.Close()

	path, _, err := key.GetStringValue(execPathValueName)
	if err != nil {
		if err == registry.ErrNotExist {
			return "", false, nil
		}
		return "", false, err
	}
	return path, true, nil
}

// quoteWindowsCommandLine builds a single command-line string the way
// CommandLineToArgvW (and so the Run key launcher) parses it back apart -
// each argument individually quoted/escaped, not just joined with
// spaces, so e.g. a mountpoint path containing a space still round-trips
// as one argument.
func quoteWindowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = quoteWindowsArg(a)
	}
	return strings.Join(quoted, " ")
}

// quoteWindowsArg implements the standard Win32 argv-quoting algorithm
// (same rules as Python's subprocess.list2cmdline and Go's own unexported
// os/exec windows helper): a backslash run is only doubled when it
// directly precedes a literal quote (which itself needs one more
// backslash to escape); backslashes anywhere else are left exactly as
// written.
func quoteWindowsArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		start := i
		for i < len(s) && s[i] == '\\' {
			i++
		}
		n := i - start
		switch {
		case i == len(s):
			b.WriteString(strings.Repeat(`\`, n*2))
		case s[i] == '"':
			b.WriteString(strings.Repeat(`\`, n*2+1))
			b.WriteByte('"')
			i++
		default:
			b.WriteString(strings.Repeat(`\`, n))
			b.WriteByte(s[i])
			i++
		}
	}
	b.WriteByte('"')
	return b.String()
}
