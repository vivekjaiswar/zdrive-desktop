//go:build linux

package autostart

import "testing"

func TestLinuxStubsReturnClearErrors(t *testing.T) {
	if err := install("/usr/local/bin/zdrive", []string{"mount"}); err == nil {
		t.Error("install() = nil error, want a clear not-supported error")
	}
	if err := uninstall(); err == nil {
		t.Error("uninstall() = nil error, want a clear not-supported error")
	}
	if _, ok, err := isInstalled(); err == nil || ok {
		t.Errorf("isInstalled() = (_, %v, %v), want (_, false, non-nil error)", ok, err)
	}
}
