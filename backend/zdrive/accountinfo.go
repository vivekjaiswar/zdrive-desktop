package zdrive

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/lib/rest"
)

// ErrNotLoggedIn is returned by FetchAccountInfo when zdrive login hasn't
// been run yet (or its saved config was removed) - not a network/API
// error, so callers can tell the two apart.
var ErrNotLoggedIn = errors.New("zdrive: not logged in")

// AccountInfo is the subset of GET /auth/me the tray needs.
type AccountInfo struct {
	Plan         string
	StorageUsed  int64
	StorageLimit int64
}

// accountInfoWire mirrors GET /auth/me's actual JSON shape - confirmed
// against a real response, not assumed: storageUsed/storageLimit come
// back as JSON *strings* ("75762720"), not bare numbers, because they're
// Prisma BigInt fields on the backend and JS can't safely round-trip a
// 64-bit int as a JSON number. A bare int64 field silently fails to
// decode against that (confirmed - the whole struct, including the
// perfectly-fine plan field, came back empty until this was fixed).
type accountInfoWire struct {
	Plan         string `json:"plan"`
	StorageUsed  string `json:"storageUsed"`
	StorageLimit string `json:"storageLimit"`
}

// FetchAccountInfo hits the same backend this mount already talks to,
// reusing the url/token zdrive login saved (the same config values
// openWebApp reads, and the same way NewFs builds its own REST client) -
// no live Fs/mount instance needed, so this works from the tray goroutine
// with no coupling to a specific running mount.
func FetchAccountInfo(ctx context.Context) (*AccountInfo, error) {
	apiURL, ok := config.FileGetValue("zdrive", "url")
	if !ok || apiURL == "" {
		return nil, ErrNotLoggedIn
	}
	token, ok := config.FileGetValue("zdrive", "token")
	if !ok || token == "" {
		return nil, ErrNotLoggedIn
	}
	return fetchAccountInfo(ctx, apiURL, token)
}

// fetchAccountInfo is split out from FetchAccountInfo so a test can drive
// it against an httptest server directly, without touching rclone's real
// (global, on-disk) config.
func fetchAccountInfo(ctx context.Context, apiURL, token string) (*AccountInfo, error) {
	srv := rest.NewClient(fshttp.NewClient(ctx)).
		SetRoot(strings.TrimRight(apiURL, "/")).
		SetHeader("Authorization", "Bearer "+token)
	var wire accountInfoWire
	opts := rest.Opts{Method: "GET", Path: "/auth/me"}
	if _, err := srv.CallJSON(ctx, &opts, nil, &wire); err != nil {
		return nil, err
	}
	used, err := strconv.ParseInt(wire.StorageUsed, 10, 64)
	if err != nil {
		return nil, err
	}
	limit, err := strconv.ParseInt(wire.StorageLimit, 10, 64)
	if err != nil {
		return nil, err
	}
	return &AccountInfo{Plan: wire.Plan, StorageUsed: used, StorageLimit: limit}, nil
}
