package zdrive

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// capturingHandler is a minimal slog.Handler that just records whether it
// was called - enough to prove teeHandler actually forwards to both sides,
// without needing a real file or stderr.
type capturingHandler struct {
	mu      sync.Mutex
	called  bool
	lastMsg string
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.called = true
	h.lastMsg = r.Message
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

func (h *capturingHandler) wasCalledWith(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.called && h.lastMsg == msg
}

func TestTeeHandlerForwardsToBoth(t *testing.T) {
	file := &capturingHandler{}
	orig := &capturingHandler{}
	h := &teeHandler{file: file, orig: orig}

	logger := slog.New(h)
	logger.Info("test message")

	if !file.wasCalledWith("test message") {
		t.Error("file handler never received the record")
	}
	if !orig.wasCalledWith("test message") {
		t.Error("orig handler never received the record - SetupEventLogging would silently swallow normal console output")
	}
}

func TestEventLogWriterRotatesPastMaxSize(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.log")
	w := &eventLogWriter{path: path}

	// Write past eventLogMaxBytes in one go - the rotation check happens
	// on the NEXT write after crossing it, so write twice.
	big := make([]byte, eventLogMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := w.Write(big); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("more\n")); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("rotated file %s.1 doesn't exist: %v", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "more\n" {
		t.Fatalf("current log file = %q, want just the post-rotation write", data)
	}
}

func TestRecoverAndLogLogsThenRePanics(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "events.log")
	origHandler := slog.Default().Handler()
	SetupEventLogging(logPath)
	defer slog.SetDefault(slog.New(origHandler)) // doesn't undo fs.SetLogger, but no other test in this package relies on fs's logger state

	done := make(chan any, 1)
	func() {
		defer func() { done <- recover() }()
		func() {
			defer RecoverAndLog("test-goroutine")
			panic("boom")
		}()
	}()

	rec := <-done
	if rec != "boom" {
		t.Fatalf("recovered value = %v, want the original panic to propagate past RecoverAndLog", rec)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading event log: %v", err)
	}
	if !strings.Contains(string(data), "test-goroutine") || !strings.Contains(string(data), "boom") {
		t.Fatalf("event log = %q, want it to mention the goroutine name and panic value", data)
	}
}
