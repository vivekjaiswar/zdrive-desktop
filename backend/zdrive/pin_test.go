package zdrive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAddPinRemovePinListPins(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "pins.json")
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := AddPin(statePath, target); err != nil {
		t.Fatalf("AddPin = %v", err)
	}
	pins, err := ListPins(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0] != target {
		t.Fatalf("pins = %v, want [%s]", pins, target)
	}

	// Pinning the same path again is a no-op, not a duplicate entry.
	if err := AddPin(statePath, target); err != nil {
		t.Fatal(err)
	}
	pins, _ = ListPins(statePath)
	if len(pins) != 1 {
		t.Fatalf("pins after re-pin = %v, want exactly 1 entry", pins)
	}

	if err := RemovePin(statePath, target); err != nil {
		t.Fatalf("RemovePin = %v", err)
	}
	pins, _ = ListPins(statePath)
	if len(pins) != 0 {
		t.Fatalf("pins after unpin = %v, want empty", pins)
	}
}

func TestAddPinRefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := AddPin(filepath.Join(dir, "pins.json"), dir); err == nil {
		t.Fatal("AddPin on a directory should fail, pinning a whole directory isn't supported yet")
	}
}

// TestPinDaemonHoldsOpenHandle: the core mechanism - a pinned path gets
// opened and the handle kept, which is what structurally exempts it from
// purgeOverQuota's `!item.inUse()` filter (see the package doc comment).
func TestPinDaemonHoldsOpenHandle(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "pins.json")
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddPin(statePath, target); err != nil {
		t.Fatal(err)
	}

	d := NewPinDaemon(statePath, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	waitFor(t, func() bool { return d.IsHeld(target) })
	if len(d.Broken()) != 0 {
		t.Fatalf("Broken() = %v, want empty", d.Broken())
	}

	cancel()
	<-done
	if d.IsHeld(target) {
		t.Fatal("IsHeld = true after Run returned, want the handle closed")
	}
}

// TestPinDaemonUnpinClosesHandle: removing a pin from the state file closes
// its held handle on the next reconcile, instead of leaking it forever.
func TestPinDaemonUnpinClosesHandle(t *testing.T) {
	origPoll := pinPollInterval
	pinPollInterval = time.Millisecond
	defer func() { pinPollInterval = origPoll }()

	dir := t.TempDir()
	statePath := filepath.Join(dir, "pins.json")
	target := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddPin(statePath, target); err != nil {
		t.Fatal(err)
	}

	d := NewPinDaemon(statePath, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	waitFor(t, func() bool { return d.IsHeld(target) })

	if err := RemovePin(statePath, target); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return !d.IsHeld(target) })
}

// TestPinDaemonCapacityGuard: a pin whose size alone exceeds the active
// mount's own cache quota gets marked broken, not silently held open past
// a quota no code can actually make it fit within.
func TestPinDaemonCapacityGuard(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "pins.json")
	target := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(target, make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddPin(statePath, target); err != nil {
		t.Fatal(err)
	}

	d := NewPinDaemon(statePath, 500) // quota smaller than the file itself
	d.reconcile()

	if d.IsHeld(target) {
		t.Fatal("IsHeld = true, want the daemon to refuse a pin bigger than the quota")
	}
	broken := d.Broken()
	if _, ok := broken[target]; !ok {
		t.Fatalf("Broken() = %v, want %s marked broken", broken, target)
	}
}

// TestPinDaemonMarksMissingPathBroken: a pin whose path can no longer be
// resolved (moved/renamed/deleted) gets marked broken instead of silently
// retried forever with no visibility.
func TestPinDaemonMarksMissingPathBroken(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "pins.json")
	target := filepath.Join(dir, "gone.txt")
	// Deliberately never create target - simulates it having disappeared
	// out from under an already-pinned entry.
	if err := writePinState(statePath, pinState{Pinned: []string{target}}); err != nil {
		t.Fatal(err)
	}

	d := NewPinDaemon(statePath, 0)
	d.reconcile()

	if d.IsHeld(target) {
		t.Fatal("IsHeld = true for a path that doesn't exist")
	}
	broken := d.Broken()
	if _, ok := broken[target]; !ok {
		t.Fatalf("Broken() = %v, want %s marked broken", broken, target)
	}
}

// waitFor polls cond every millisecond for up to 2s - used instead of a
// fixed sleep since the daemon's own first reconcile happens synchronously
// at Run's start, but tests still shouldn't assume a specific timing.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition never became true within 2s")
}
