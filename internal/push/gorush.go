package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/httpx"
)

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

type Gorush struct {
	pushURL string
	http    *http.Client
	logger  *slog.Logger
}

func NewGorush(baseURL string, httpClient *http.Client, logger *slog.Logger) *Gorush {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gorush{
		pushURL: strings.TrimRight(baseURL, "/") + "/api/push",
		http:    httpx.NoRedirectClient(httpClient),
		logger:  logger,
	}
}

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
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Never include the response body: gorush error bodies echo the
		// notification, tokens included.
		return fmt.Errorf("push: gorush returned status %d", resp.StatusCode)
	}
	return nil
}
