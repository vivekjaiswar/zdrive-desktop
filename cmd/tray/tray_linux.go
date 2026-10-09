//go:build linux

// Linux is dev-only for this project (per docs/PLAN.md) - no tray icon
// here. This file exists only so main.go's unconditional blank import of
// cmd/tray still compiles on this platform; see tray.go for the real
// (Windows/macOS) implementation.
package tray
