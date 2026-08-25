// Package push sends alarm, alert, and Live Activity notifications to
// devices through gorush, the sidecar's push gateway (spec §5.4/§6). It also
// defines the terminal-failure classification (§6.5) the feedback webhook
// uses to prune dead tokens.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	Tokens      []string `json:"tokens"`
	Platform    int      `json:"platform"`
	Title       string   `json:"title,omitempty"`
	Message     string   `json:"message"`
	Priority    string   `json:"priority"`
	Development bool     `json:"development,omitempty"`
	// Topic is the APNs topic (the app's bundle id). Required by Apple under
	// token-based (.p8) auth -- without it every push bounces MissingTopic --
	// and gorush has no global setting for it, so it rides on each request.
	Topic string         `json:"topic,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// Gorush is a Sender backed by one gorush instance's HTTP push API.
type Gorush struct {
	pushURL   string
	apnsTopic string
	http      *http.Client
}

// NewGorush builds a Gorush that posts to baseURL's /api/push (a bare
// host:port is treated as http://) and stamps
// apnsTopic (the iOS app's bundle id) onto every iOS notification; an empty
// topic is sent as no field, which APNs rejects under .p8 auth, so callers
// should treat empty as misconfiguration (main warns at boot). A nil
// httpClient defaults to http.DefaultClient. If the given client has no
// Timeout set, NewGorush uses a copy with a 10-second Timeout rather than
// the caller's client as-is -- see gorushTimeout -- following
// httpx.NoRedirectClient's copy-don't-mutate rule so a shared
// http.DefaultClient is never altered out from under other callers.
func NewGorush(baseURL, apnsTopic string, httpClient *http.Client) *Gorush {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := httpx.NoRedirectClient(httpClient)
	if client.Timeout == 0 {
		client.Timeout = gorushTimeout
	}
	// A scheme-less address is taken as plain HTTP: Render's Blueprint
	// fromService "hostport" hands out "name-xxxx:8088", and the private
	// network it names is unencrypted.
	if !strings.Contains(baseURL, "://") {
		baseURL = "http://" + baseURL
	}
	return &Gorush{
		pushURL:   strings.TrimRight(baseURL, "/") + "/api/push",
		apnsTopic: apnsTopic,
		http:      client,
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
		gn.Topic = g.apnsTopic
	}
	return g.post(ctx, map[string]any{"notifications": []gorushNotification{gn}})
}

// post submits one /api/push batch. Never include the response body in the
// error: gorush error bodies echo the notification, tokens included.
func (g *Gorush) post(ctx context.Context, batch any) error {
	body, err := json.Marshal(batch)
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
		return fmt.Errorf("push: gorush request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain so the connection is reused
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("push: gorush returned status %d", resp.StatusCode)
	}
	return nil
}

// liveActivityTopicSuffix is appended to the app bundle id to form the APNs
// topic for Live Activity pushes (spec §6.6). gorush does not derive it.
const liveActivityTopicSuffix = ".push-type.liveactivity"

// gorushLiveActivity is gorush's Live Activity request shape. The date keys
// and content-state are HYPHENATED because they map 1:1 onto APNs aps keys;
// gorush's unmarshaller silently drops snake_case variants and the activity
// then never updates. No title/message: a Live Activity push has no alert.
type gorushLiveActivity struct {
	Tokens        []string `json:"tokens"`
	Platform      int      `json:"platform"`
	PushType      string   `json:"push_type"`
	Priority      string   `json:"priority"`
	Topic         string   `json:"topic"`
	Development   bool     `json:"development,omitempty"`
	Event         string   `json:"event"`
	ContentState  any      `json:"content-state"`
	Timestamp     int64    `json:"timestamp"`
	StaleDate     int64    `json:"stale-date,omitempty"`
	DismissalDate int64    `json:"dismissal-date,omitempty"`
}

// SendLiveActivity posts p as a liveactivity push at APNs priority 10
// (gorush "high"); at priority 5 an idle phone holds every push and the
// Lock Screen freezes (spec §6.6). An empty APNs topic is refused without a
// request: unlike Send, where gorush's own config might supply a topic, a
// bare ".push-type.liveactivity" would bounce BadTopic every minute for
// eight hours (design spec §2.7).
func (g *Gorush) SendLiveActivity(ctx context.Context, p LiveActivityPush) error {
	if g.apnsTopic == "" {
		return errors.New("push: live activity push requires an APNs topic (--apns-topic)")
	}
	if p.Timestamp.IsZero() {
		return errors.New("push: live activity push requires a timestamp")
	}
	n := gorushLiveActivity{
		Tokens:       []string{p.Token},
		Platform:     int(PlatformIOS),
		PushType:     "liveactivity",
		Priority:     "high",
		Topic:        g.apnsTopic + liveActivityTopicSuffix,
		Development:  p.Sandbox,
		Event:        p.Event,
		ContentState: p.ContentState,
		Timestamp:    p.Timestamp.Unix(),
	}
	if !p.StaleDate.IsZero() {
		n.StaleDate = p.StaleDate.Unix()
	}
	if !p.DismissalDate.IsZero() {
		n.DismissalDate = p.DismissalDate.Unix()
	}
	return g.post(ctx, map[string]any{"notifications": []gorushLiveActivity{n}})
}
