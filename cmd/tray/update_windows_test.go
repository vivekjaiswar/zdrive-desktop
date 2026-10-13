//go:build windows

package tray

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSwapBinaryReplacesFileAndLeavesOldBackup(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "zdrive.exe")
	stagedPath := filepath.Join(dir, "zdrive.exe.new")
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

	oldContent, err := os.ReadFile(execPath + ".old")
	if err != nil {
		t.Fatalf("expected a .old backup of the original binary: %v", err)
	}
	if string(oldContent) != "old content" {
		t.Errorf(".old backup content = %q, want %q", oldContent, "old content")
	}
}

func TestSwapBinaryRollsBackOnFailure(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "zdrive.exe")
	if err := os.WriteFile(execPath, []byte("old content"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Deliberately doesn't exist - the second rename (staged -> execPath)
	// will fail, exercising the rollback path.
	if err := swapBinary(execPath, filepath.Join(dir, "does-not-exist")); err == nil {
		t.Fatal("swapBinary() = nil error, want failure since the staged file doesn't exist")
	}

	got, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatalf("expected execPath to be restored after rollback, but it's gone: %v", err)
	}
	if string(got) != "old content" {
		t.Errorf("execPath content after rollback = %q, want original %q", got, "old content")
	}
	if _, err := os.Stat(execPath + ".old"); !os.IsNotExist(err) {
		t.Error(".old backup should have been renamed back into place, not left behind, after a rollback")
	}
}

func TestCleanupOldBinaryAtRemovesLeftoverFile(t *testing.T) {
	dir := t.TempDir()
	execPath := filepath.Join(dir, "zdrive.exe")
	oldPath := execPath + ".old"
	if err := os.WriteFile(oldPath, []byte("leftover"), 0o755); err != nil {
		t.Fatal(err)
	}

	cleanupOldBinaryAt(execPath)

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("expected the leftover .old file to be removed")
	}
}

func TestCleanupOldBinaryAtNoopWhenNothingToClean(t *testing.T) {
	dir := t.TempDir()
	// Should not panic or error when there's no .old file at all.
	cleanupOldBinaryAt(filepath.Join(dir, "zdrive.exe"))
}
