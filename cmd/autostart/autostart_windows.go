//go:build windows

package autostart

import (
	"fmt"
	"strings"
	"syscall"

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
// each argument individually escaped via syscall.EscapeArg (same MSDN
// algorithm CreateProcess itself expects), not just joined with spaces,
// so e.g. a mountpoint path containing a space still round-trips as one
// argument.
func quoteWindowsCommandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	return strings.Join(quoted, " ")
}
