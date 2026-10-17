package shutdownwait

import (
	"testing"
	"time"
)

func TestRunWithTimeoutReturnsWhenFnFinishes(t *testing.T) {
	ran := make(chan struct{})
	start := time.Now()
	runWithTimeout(func() { close(ran) }, time.Second)
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("runWithTimeout took %s for an instant fn, want near-instant", time.Since(start))
	}
	select {
	case <-ran:
	default:
		t.Error("fn was never called")
	}
}

// TestRunWithTimeoutBoundsAHangingFn is the real-hardware bug this
// guards against (2026-10-17): rclone's own unmount call hung forever on
// a kernel-level NFS mount wedge, taking the whole shutdown path down
// with it. runWithTimeout must return within timeout regardless of
// whether fn ever does.
func TestRunWithTimeoutBoundsAHangingFn(t *testing.T) {
	start := time.Now()
	runWithTimeout(func() { select {} }, 100*time.Millisecond) // never returns
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %s, before the 100ms timeout even elapsed", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("runWithTimeout took %s to bound a 100ms timeout - not actually bounded", elapsed)
	}
}
