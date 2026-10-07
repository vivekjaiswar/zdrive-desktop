//go:build windows

package autostart

import "testing"

func TestQuoteWindowsArg(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"path with a space gets quoted, no embedded backslash doubling needed", `C:\Program Files\x.exe`, `"C:\Program Files\x.exe"`},
		{"plain, no quoting needed", `simple`, `simple`},
		{"path with a space", `C:\Users\me\My Drive`, `"C:\Users\me\My Drive"`},
		{"trailing backslash before closing quote doubles", `C:\Users\me \`, `"C:\Users\me \\"`},
		{"embedded literal quote", `say "hi"`, `"say \"hi\""`},
		{"backslashes not before a quote stay as-is", `a\b c`, `"a\b c"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quoteWindowsArg(c.in); got != c.want {
				t.Errorf("quoteWindowsArg(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestQuoteWindowsCommandLine(t *testing.T) {
	got := quoteWindowsCommandLine([]string{`C:\ZDrive\zdrive.exe`, "mount", "zdrive:", `C:\Users\me\My Drive`, "--vfs-cache-mode", "full"})
	want := `C:\ZDrive\zdrive.exe mount zdrive: "C:\Users\me\My Drive" --vfs-cache-mode full`
	if got != want {
		t.Errorf("quoteWindowsCommandLine(...) = %q, want %q", got, want)
	}
}
