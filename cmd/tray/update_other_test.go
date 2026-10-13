//go:build !windows

package tray

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSwapBinaryReplacesFile(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "zdrive")
	stagedPath := filepath.Join(dir, "zdrive.new")
	if err := os.WriteFile(execPath, []byte("old content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stagedPath, []byte("new content"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(execPath, stagedPath); err != nil {
		t.Fatalf("swapBinary() = %v", err)
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new content" {
		t.Errorf("execPath content = %q, want %q", got, "new content")
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Error("staged file should no longer exist at its old path - it was renamed into place, not copied")
	}
}

func TestSwapBinaryFailsClearlyWhenStagedFileMissing(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "zdrive")
	if err := os.WriteFile(execPath, []byte("old content"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := swapBinary(execPath, filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("swapBinary() = nil error, want failure since the staged file doesn't exist")
	}

	// execPath must be untouched - a failed update shouldn't leave a
	// gap where there used to be a working binary.
	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old content" {
		t.Errorf("execPath content = %q, want it unchanged at %q", got, "old content")
	}
}
