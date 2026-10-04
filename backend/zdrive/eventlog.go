// Crash/mount event logging (M3): a local, durable JSON-lines record of
// what this process did and any failure it hit - every rclone log record
// (including a real mount failure's fs.Fatalf, which routes through the
// same logger before os.Exit), plus any panic recovered from a goroutine
// this project spawns itself.
//
// Deliberately local-only for v1: no backend endpoint exists anywhere to
// upload these events to (checked, not assumed - the backend's only
// related thing, ErrorLog+Sentry, is a passive side effect of the
// SERVER's own unhandled 500s, not something a desktop client can POST
// to), and designing one is a cross-repo schema/auth decision for later,
// not something to guess at here. This log is exactly the data that
// would get shipped once a real endpoint exists - nothing here needs to
// change to support that later, it just adds an upload step.
package zdrive

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"

	"github.com/rclone/rclone/fs"
)

const (
	eventLogFileName = "zdrive-events.log"
	eventLogMaxBytes = 5 * 1024 * 1024 // rotate past this, keeping exactly one previous file
)

// EventLogPath returns the local event-log file's path, next to rclone's
// own config file (not inside it).
func EventLogPath(configFilePath string) string {
	return filepath.Join(filepath.Dir(configFilePath), eventLogFileName)
}

// eventLogWriter is a minimal, mutex-guarded, size-rotated writer -
// deliberately not relying on slog's own (rotation-less) default, since
// this log is meant to be left running indefinitely across an always-on
// mount process and must not grow unbounded.
type eventLogWriter struct {
	mu   sync.Mutex
	path string
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if info, err := os.Stat(w.path); err == nil && info.Size() > eventLogMaxBytes {
		_ = os.Rename(w.path, w.path+".1") // best-effort - never fail the write over a rotation hiccup
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return f.Write(p)
}

// teeHandler sends every log record to both a JSON-lines file and
// whatever handler was already installed (rclone's normal console
// output), so SetupEventLogging doesn't silently swallow output every
// other command already relies on seeing.
type teeHandler struct {
	file slog.Handler
	orig slog.Handler
}

func (h *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.file.Enabled(ctx, level) || h.orig.Enabled(ctx, level)
}

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = h.file.Handle(ctx, r.Clone()) // best-effort - a logging hiccup must never block the real operation that triggered it
	return h.orig.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &teeHandler{file: h.file.WithAttrs(attrs), orig: h.orig.WithAttrs(attrs)}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{file: h.file.WithGroup(name), orig: h.orig.WithGroup(name)}
}

// SetupEventLogging tees every subsequent fs.Logf/Fatalf/etc. call into a
// local JSON-lines file at path, in addition to whatever rclone's own
// logger was already doing. Call once, as early as possible in main().
func SetupEventLogging(path string) {
	jsonHandler := slog.NewJSONHandler(&eventLogWriter{path: path}, nil)
	fs.SetLogger(&teeHandler{file: jsonHandler, orig: slog.Default().Handler()})
}

// RecoverAndLog should be deferred as the first line of any goroutine this
// project spawns itself with a bare `go` (pollChanges; the pin daemon,
// PR #4, needs the same treatment once both land - independent PRs, each
// must compile and work standalone). recover() only catches a panic on
// the same goroutine it's deferred in, so a single top-level main()
// recover would never see one of these. Logs the panic with its stack,
// then re-panics so the process still crashes visibly rather than
// silently swallowing a real bug.
func RecoverAndLog(goroutineName string) {
	if r := recover(); r != nil {
		fs.Logf(nil, "PANIC in %s: %v\n%s", goroutineName, r, debug.Stack())
		panic(r)
	}
}
