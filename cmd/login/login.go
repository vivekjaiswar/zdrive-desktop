// Package login implements device-code sign-in (B4) against the ZDrive
// API: approve a short code from a browser you're already logged into,
// and the resulting long-lived token is saved straight into rclone's
// config under the "zdrive" remote, so a later `zdrive mount zdrive: ...`
// needs no manually-copied token/env vars.
package login

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rclone/rclone/cmd"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/lib/oauthutil"
	"github.com/spf13/cobra"
	"github.com/vivekjaiswar/zdrive-desktop/backend/zdrive"
)

var apiURL string

func init() {
	cmd.Root.AddCommand(commandDefinition)
	cmdFlags := commandDefinition.Flags()
	flags.StringVarP(cmdFlags, &apiURL, "url", "", "https://zhdrive.in/api", "ZDrive API base URL", "")
}

var commandDefinition = &cobra.Command{
	Use:   "login",
	Short: "Sign in to ZDrive and save a long-lived token for mounting.",
	Long: `Starts device-code sign-in: prints (and tries to open) a link to
approve this device from a browser you're already logged into, then
polls until approved and saves the resulting token into rclone's
config under the "zdrive" remote - so "zdrive mount zdrive: <path>"
works afterwards with no env vars needed.`,
	RunE: func(command *cobra.Command, args []string) error {
		return doLogin(context.Background(), apiURL, time.Sleep, saveToken)
	},
}

// saveToken is the real tokenSaver: writes straight into rclone's config
// file. Swapped out in tests so they never touch a real config file.
func saveToken(url, token string) {
	config.FileSetValue("zdrive", "type", "zdrive")
	config.FileSetValue("zdrive", "url", url)
	config.FileSetValue("zdrive", "token", token)
	config.SaveConfig()
}

type startResp struct {
	DeviceCode string `json:"deviceCode"`
	UserCode   string `json:"userCode"`
	ExpiresIn  int    `json:"expiresIn"`
	Interval   int    `json:"interval"`
}

type pollResp struct {
	Status      string `json:"status"`
	AccessToken string `json:"accessToken"`
}

// postJSON posts body (or no body, if nil) and decodes a JSON reply into out.
func postJSON(ctx context.Context, url string, body, out any) (int, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, &buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-ZDrive-Client-Version", zdrive.ClientVersion)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// tokenSaver persists a signed-in session. sleep and save are injected so
// tests can run the real polling/status logic without real wall-clock waits
// or touching rclone's actual config file.
type tokenSaver func(url, token string)

func doLogin(ctx context.Context, apiURL string, sleep func(time.Duration), save tokenSaver) error {
	var start startResp
	status, err := postJSON(ctx, apiURL+"/auth/device/start", nil, &start)
	if err != nil {
		return fmt.Errorf("could not reach %s: %w", apiURL, err)
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("could not start sign-in (status %d)", status)
	}

	// The web app and API share one domain with the API under /api (true
	// for both zhdrive.in and staging.zhdrive.in) - derive the approval
	// page's URL from apiURL rather than adding a second flag for it.
	webURL := strings.TrimSuffix(strings.TrimSuffix(apiURL, "/api"), "/")
	verifyURL := webURL + "/device?user_code=" + start.UserCode

	fmt.Printf("Go to %s\nEnter code: %s\n", verifyURL, start.UserCode)
	if err := oauthutil.OpenURL(verifyURL); err != nil {
		fmt.Printf("(couldn't open a browser automatically: %v)\n", err)
	}

	interval := time.Duration(start.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		sleep(interval)

		var poll pollResp
		status, err := postJSON(ctx, apiURL+"/auth/device/poll", map[string]string{"deviceCode": start.DeviceCode}, &poll)
		if err != nil {
			return err
		}

		switch {
		case status >= 200 && status < 300 && poll.Status == "approved":
			save(apiURL, poll.AccessToken)
			fmt.Println("Signed in. Run: zdrive mount zdrive: <path> --vfs-cache-mode full")
			return nil
		case status >= 200 && status < 300 && poll.Status == "pending":
			continue
		case status == http.StatusGone:
			return fmt.Errorf("code expired - run `zdrive login` again")
		case status == http.StatusForbidden:
			return fmt.Errorf("sign-in was denied")
		default:
			return fmt.Errorf("unexpected response while polling (status %d)", status)
		}
	}
	return fmt.Errorf("timed out waiting for approval")
}
