// Pin-to-keep-local (M3): a small set of user-chosen paths that should
// never be evicted from the local VFS cache, regardless of LRU pressure.
//
// rclone's own vendored vfs/vfscache has no pin/priority concept at all -
// purgeOverQuota/purgeClean both select purely by ATime, with the one
// built-in protection being inUse() (item.opens != 0 || item.info.Dirty).
// Holding an open file handle on a pinned path makes it structurally
// invisible to purgeOverQuota's candidate list, and periodically
// re-reading a byte to refresh ATime pushes it to dead-last in
// purgeClean's sort order too - covers every case except "pinned bytes
// alone exceed the configured quota", which no caching scheme can fix
// without more disk (not worth forking rclone over).
//
// There is no reachable *vfs.VFS from this project's own code (vfs.New
// runs entirely inside vendored cmd/mountlib, never handed back to
// NewFs), so pinned files are opened via their real OS path under the
// mount, not through any rclone API - this is not a problematic
// self-referential loop, FUSE/cgofuse/WinFsp/NFS-loopback serve each
// request on a kernel-brokered thread independent of the caller.
package zdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

const pinStateFileName = "zdrive-pins.json"

// var, not const, so tests can shrink these to exercise reconcile/refresh
// without a real multi-second wait (same pattern as retryBaseDelay).
var (
	pinPollInterval  = 5 * time.Second
	pinRefreshPeriod = 2 * time.Minute // how often a held pin's ATime gets refreshed
)

type pinState struct {
	Pinned []string `json:"pinned"`
}

// PinStatePath returns the local pin-list file's path, next to rclone's own
// config file (not inside it - that's a flat credentials store, wrong
// shape for a growing list).
func PinStatePath(configFilePath string) string {
	return filepath.Join(filepath.Dir(configFilePath), pinStateFileName)
}

func readPinState(path string) (pinState, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return pinState{}, nil
	}
	if err != nil {
		return pinState{}, err
	}
	var st pinState
	if len(data) > 0 {
		if err := json.Unmarshal(data, &st); err != nil {
			return pinState{}, err
		}
	}
	return st, nil
}

func writePinState(path string, st pinState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// AddPin adds absPath (an absolute path to a file somewhere under a ZDrive
// mount) to the pin list at statePath. Does not itself check it against a
// cache quota - that check happens in the running mount's own pin daemon
// (PinDaemon), which is the only place that actually knows the active
// --vfs-cache-max-size value: `zdrive pin` and `zdrive mount` are normally
// two separate OS processes, so the pin command has no way to see the
// mount's own in-memory flag value.
func AddPin(statePath, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("can't pin %s: %w", absPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("can't pin %s: pinning a whole directory isn't supported yet, pin individual files", absPath)
	}
	st, err := readPinState(statePath)
	if err != nil {
		return err
	}
	for _, p := range st.Pinned {
		if p == absPath {
			return nil // already pinned
		}
	}
	st.Pinned = append(st.Pinned, absPath)
	return writePinState(statePath, st)
}

// RemovePin removes absPath from the pin list.
func RemovePin(statePath, absPath string) error {
	st, err := readPinState(statePath)
	if err != nil {
		return err
	}
	out := st.Pinned[:0]
	for _, p := range st.Pinned {
		if p != absPath {
			out = append(out, p)
		}
	}
	st.Pinned = out
	return writePinState(statePath, st)
}

// ListPins returns the current pin list.
func ListPins(statePath string) ([]string, error) {
	st, err := readPinState(statePath)
	if err != nil {
		return nil, err
	}
	return st.Pinned, nil
}

// pinnedHandle tracks one currently-held-open pinned file.
type pinnedHandle struct {
	file    *os.File
	size    int64
	modTime time.Time
}

// PinDaemon periodically opens every currently-pinned path and keeps each
// handle open for the mount's lifetime - see the package doc comment for
// why this alone exempts a pinned file from eviction, with no rclone fork.
type PinDaemon struct {
	statePath     string
	maxCacheBytes int64 // 0 = quota unknown/unset, skip the capacity guard

	mu     sync.Mutex
	held   map[string]*pinnedHandle // absPath -> handle
	broken map[string]string        // absPath -> reason
}

// NewPinDaemon constructs a daemon. maxCacheBytes should be the active
// mount's own --vfs-cache-max-size value in bytes (0 if none is
// configured) - read once at mount startup, since this process IS the
// mount and is the only place that actually has it.
func NewPinDaemon(statePath string, maxCacheBytes int64) *PinDaemon {
	return &PinDaemon{
		statePath:     statePath,
		maxCacheBytes: maxCacheBytes,
		held:          map[string]*pinnedHandle{},
		broken:        map[string]string{},
	}
}

// Run blocks until ctx is done, polling the pin-state file and reconciling
// held handles against it, plus a separate ticker to keep ATimes fresh.
func (d *PinDaemon) Run(ctx context.Context) {
	defer RecoverAndLog("pin daemon")
	d.reconcile() // don't wait out the first poll interval before pinning anything already listed
	pollTicker := time.NewTicker(pinPollInterval)
	defer pollTicker.Stop()
	refreshTicker := time.NewTicker(pinRefreshPeriod)
	defer refreshTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			d.closeAll()
			return
		case <-pollTicker.C:
			d.reconcile()
		case <-refreshTicker.C:
			d.refreshAll()
		}
	}
}

// heldBytes returns the total size of every currently-held pin except the
// one named excl (pass "" to total everything) - used to check whether
// adding one more would exceed the quota.
func (d *PinDaemon) heldBytes(excl string) int64 {
	var total int64
	for p, h := range d.held {
		if p == excl {
			continue
		}
		total += h.size
	}
	return total
}

func (d *PinDaemon) reconcile() {
	st, err := readPinState(d.statePath)
	if err != nil {
		fs.Logf(nil, "pin daemon: reading pin state: %v", err)
		return
	}
	wanted := make(map[string]bool, len(st.Pinned))
	for _, p := range st.Pinned {
		wanted[p] = true
		d.ensureOpen(p)
	}
	d.mu.Lock()
	for absPath, h := range d.held {
		if !wanted[absPath] {
			h.file.Close()
			delete(d.held, absPath)
			delete(d.broken, absPath)
		}
	}
	d.mu.Unlock()
}

// ensureOpen (re-)opens absPath if it isn't already held with the same
// identity (size+modTime) as last confirmed. Re-resolves fresh every call
// rather than trusting a previously-held handle's path to still be valid -
// this project's own 30s change-feed poller flushes the dircache on any
// remote change, so a pinned path going stale (moved/renamed/deleted
// remotely) between pin-daemon ticks is an expected case, not hypothetical.
func (d *PinDaemon) ensureOpen(absPath string) {
	info, err := os.Stat(absPath)
	if err != nil {
		d.markBroken(absPath, err)
		return
	}

	d.mu.Lock()
	h, stillHeld := d.held[absPath]
	if stillHeld && h.modTime.Equal(info.ModTime()) && h.size == info.Size() {
		d.mu.Unlock()
		return // unchanged, nothing to do
	}
	if d.maxCacheBytes > 0 && d.heldBytes(absPath)+info.Size() > d.maxCacheBytes {
		d.mu.Unlock()
		d.markBroken(absPath, fmt.Errorf("%d bytes would exceed the %d byte cache quota together with other pinned files", info.Size(), d.maxCacheBytes))
		return
	}
	d.mu.Unlock()

	if stillHeld {
		h.file.Close() // identity changed (moved/replaced) - reopen fresh
	}
	f, err := os.Open(absPath)
	if err != nil {
		d.markBroken(absPath, err)
		return
	}
	// Eager fill: force every byte range through the existing lazy
	// download path so the file becomes fully resident now, not just
	// exempted-once-cached whenever something else happens to read it.
	if _, err := io.Copy(io.Discard, f); err != nil {
		f.Close()
		d.markBroken(absPath, err)
		return
	}
	d.mu.Lock()
	d.held[absPath] = &pinnedHandle{file: f, size: info.Size(), modTime: info.ModTime()}
	delete(d.broken, absPath)
	d.mu.Unlock()
}

func (d *PinDaemon) markBroken(absPath string, err error) {
	d.mu.Lock()
	if h, ok := d.held[absPath]; ok {
		h.file.Close()
		delete(d.held, absPath)
	}
	d.broken[absPath] = err.Error()
	d.mu.Unlock()
	fs.Logf(nil, "pin daemon: %s is broken: %v", absPath, err)
}

// refreshAll re-reads one byte of every held file to keep its ATime fresh -
// the backstop for purgeClean, which (unlike purgeOverQuota) doesn't check
// opens at all, only IsDirty, so a stale ATime on an otherwise-open pinned
// file could still make it a reset candidate under sustained over-quota
// pressure. Best-effort: a transient read failure here isn't treated as
// the pin going broken, only a real re-resolution failure in ensureOpen is.
func (d *PinDaemon) refreshAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	buf := make([]byte, 1)
	for _, h := range d.held {
		if _, err := h.file.Seek(0, io.SeekStart); err != nil {
			continue
		}
		_, _ = h.file.Read(buf)
	}
}

func (d *PinDaemon) closeAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.held {
		h.file.Close()
	}
	d.held = map[string]*pinnedHandle{}
}

// Broken returns the absPath->reason map for pins that could not be
// (re-)opened, e.g. for a status command to surface later.
func (d *PinDaemon) Broken() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]string, len(d.broken))
	for k, v := range d.broken {
		out[k] = v
	}
	return out
}

// IsHeld reports whether absPath currently has an open handle.
func (d *PinDaemon) IsHeld(absPath string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.held[absPath]
	return ok
}
