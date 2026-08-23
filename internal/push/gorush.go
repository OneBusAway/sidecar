// Package push sends alarm and alert notifications to devices through
// gorush, the sidecar's push gateway (spec §5.4/§6). It also defines the
// terminal-failure classification (§6.5) the feedback webhook uses to prune
// dead tokens.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpx"
)

// gorushTimeout bounds one Send call. Without it, a gorush connection that
// hangs (rather than erroring) never returns, and the alarm scheduler's
// errgroup -- which caps concurrent sends -- eventually has every slot
// stuck on a dead send and stalls firing for every rider, permanently.
const gorushTimeout = 10 * time.Second

// gorushNotification is the subset of gorush's request schema this sidecar
// uses (POST /api/push). Priority is always "high": alarm pushes are
// time-sensitive by definition, and gorush maps it to APNs priority 10 /
// FCM high so an idle phone does not hold the wake-the-rider push.
type gorushNotification struct {
	Tokens      []string       `json:"tokens"`
	Platform    int            `json:"platform"`
	Title       string         `json:"title,omitempty"`
	Message     string         `json:"message"`
	Priority    string         `json:"priority"`
	Development bool           `json:"development,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
}

// Gorush is a Sender backed by one gorush instance's HTTP push API.
type Gorush struct {
	pushURL string
	http    *http.Client
}

// NewGorush builds a Gorush that posts to baseURL's /api/push. A nil
// httpClient defaults to http.DefaultClient. If the given client has no
// Timeout set, NewGorush uses a copy with a 10-second Timeout rather than
// the caller's client as-is -- see gorushTimeout -- following
// httpx.NoRedirectClient's copy-don't-mutate rule so a shared
// http.DefaultClient is never altered out from under other callers.
func NewGorush(baseURL string, httpClient *http.Client) *Gorush {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := httpx.NoRedirectClient(httpClient)
	if client.Timeout == 0 {
		client.Timeout = gorushTimeout
	}
	return &Gorush{
		pushURL: strings.TrimRight(baseURL, "/") + "/api/push",
		http:    client,
	}
}

// Send posts n to gorush as a single-notification batch and reports a
// non-2xx response or transport failure as an error. A nil return means
// gorush accepted the notification, not that the device received it --
// delivery failures come back asynchronously via the feedback webhook
// (§6.5).
func (g *Gorush) Send(ctx context.Context, n Notification) error {
	gn := gorushNotification{
		Tokens:   n.Tokens,
		Platform: int(n.Platform),
		Title:    n.Title,
		Message:  n.Message,
		Priority: "high",
		Data:     n.Data,
	}
	if n.Platform == PlatformIOS {
		gn.Development = n.Sandbox
	}
	body, err := json.Marshal(map[string]any{"notifications": []gorushNotification{gn}})
	if err != nil {
		return fmt.Errorf("push: marshal notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		// The transport error can embed the gateway URL; that is
		// operator-configured and not secret, but tokens never appear in it,
		// so it is safe to wrap as-is.
		return fmt.Errorf("push: gorush request: %w", err)
	}
	defer resp.Body.Close()
	// Draining lets the connection be reused; a drain failure doesn't
	// change the outcome, which StatusCode below already determines.
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain, see comment above
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Never include the response body: gorush error bodies echo the
		// notification, tokens included.
		return fmt.Errorf("push: gorush returned status %d", resp.StatusCode)
	}
	return nil
}
