//go:build windows

package autostart

import "testing"

// Only the composition (join with space) is this project's own logic -
// per-argument escaping is syscall.EscapeArg, already tested upstream.
func TestQuoteWindowsCommandLine(t *testing.T) {
	got := quoteWindowsCommandLine([]string{`C:\ZDrive\zdrive.exe`, "mount", "zdrive:", `C:\Users\me\My Drive`, "--vfs-cache-mode", "full"})
	want := `C:\ZDrive\zdrive.exe mount zdrive: "C:\Users\me\My Drive" --vfs-cache-mode full`
	if got != want {
		t.Errorf("quoteWindowsCommandLine(...) = %q, want %q", got, want)
	}
}
