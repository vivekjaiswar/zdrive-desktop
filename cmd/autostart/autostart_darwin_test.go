//go:build darwin

package autostart

import (
	"fmt"
	"testing"
)

func TestPlistRoundTripsExecPath(t *testing.T) {
	cases := []struct {
		name     string
		execPath string
	}{
		{"plain path", "/usr/local/bin/zdrive"},
		{"path with a space", "/Users/me/My App/zdrive"},
		{"path needing XML escaping", "/Users/me & co/zdrive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var argsXML string
			for _, a := range []string{c.execPath, "mount", "zdrive:"} {
				escaped, err := xmlEscape(a)
				if err != nil {
					t.Fatalf("xmlEscape(%q) = %v", a, err)
				}
				argsXML += "\t\t<string>" + escaped + "</string>\n"
			}
			escapedExecPath, err := xmlEscape(c.execPath)
			if err != nil {
				t.Fatalf("xmlEscape(%q) = %v", c.execPath, err)
			}

			content := []byte(fmt.Sprintf(plistTpl, label, argsXML, escapedExecPath))

			got, ok, err := extractExecPath(content)
			if err != nil {
				t.Fatalf("extractExecPath() = %v", err)
			}
			if !ok {
				t.Fatal("extractExecPath() ok = false, want true")
			}
			if got != c.execPath {
				t.Errorf("extractExecPath() = %q, want %q", got, c.execPath)
			}
		})
	}
}

func TestExtractExecPathMissingKey(t *testing.T) {
	_, ok, err := extractExecPath([]byte(`<?xml version="1.0"?><plist version="1.0"><dict></dict></plist>`))
	if err != nil {
		t.Fatalf("extractExecPath() = %v", err)
	}
	if ok {
		t.Error("extractExecPath() ok = true for a plist with no ZDriveExecPath key, want false")
	}
}
