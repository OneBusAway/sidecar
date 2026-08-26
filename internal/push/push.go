package push

import (
	"context"
	"strings"
	"time"
)

// Platform uses gorush's wire codes so the adapter needs no translation
// table; they are stable public API of that project.
type Platform int

// The platform codes gorush expects on the wire; values are fixed by that
// project's API, not this package.
const (
	PlatformIOS     Platform = 1
	PlatformAndroid Platform = 2
)

// Notification is one push to one or more device tokens. Data is the
// structured payload the app parses (§5.4); it must marshal to JSON.
type Notification struct {
	Tokens   []string
	Platform Platform
	Sandbox  bool // APNs sandbox routing; meaningless for Android
	Title    string
	Message  string
	Data     map[string]any
}

// Sender delivers notifications. Implementations must be safe for
// concurrent use. Send returning nil means the transport accepted the
// notification, not that the device received it -- delivery failures come
// back asynchronously (§6.5) via the feedback webhook.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// Rejection is one token gorush refused synchronously (design spec §2.5):
// in core.sync mode gorush reports inline failures such as BadDeviceToken
// or an oversized payload in the response "logs", and those tokens never
// reach the feedback webhook.
type Rejection struct {
	Token  string
	Reason string
}

// BatchResult is what the transport reported inline for one batch. An empty
// Rejected is the normal case in gorush's default async mode, where every
// failure arrives later via the webhook (§6.5).
type BatchResult struct {
	Rejected []Rejection
}

// BatchSender delivers one notification to many tokens and returns the
// transport's synchronous rejections. notifID is stamped on the request so
// asynchronous feedback can be correlated back to the send (design spec
// §2.8). A nil error means the transport accepted the batch, not that any
// device received it.
type BatchSender interface {
	SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error)
}

// terminalReasons are exactly the spec's list (§6.5). ExpiredToken (also
// never-retry per Apple) is deliberately excluded to stay spec-faithful;
// tokens it would have caught die at the 180-day prune instead.
var terminalReasons = []string{"Unregistered", "BadDeviceToken", "DeviceTokenNotForTopic"}

// IsTerminal reports whether an APNs failure reason means the token is
// dead (spec §4/§6.5): Unregistered, BadDeviceToken, DeviceTokenNotForTopic,
// matched by substring. ExpiredProviderToken is about our JWT and is
// deliberately not terminal.
func IsTerminal(reason string) bool {
	for _, t := range terminalReasons {
		if strings.Contains(reason, t) {
			return true
		}
	}
	return false
}

// LiveActivityPush is one ActivityKit push to one Live Activity token
// (spec §6.6). ContentState marshals to the §6.2 object; it is typed any
// because the concrete type lives in internal/liveactivities, which imports
// this package (design spec §2.7).
type LiveActivityPush struct {
	Token   string
	Sandbox bool   // APNs sandbox routing (spec §2.7)
	Event   string // "update" | "end"
	// ContentState is the §6.2 content-state object.
	ContentState any
	// Timestamp is required and must advance on every push to one activity;
	// APNs silently drops a push whose timestamp does not.
	Timestamp time.Time
	// StaleDate is sent on updates (~10 minutes out); zero omits it.
	StaleDate time.Time
	// DismissalDate is sent on end (~15 minutes out); zero omits it.
	DismissalDate time.Time
}

// LiveActivitySender delivers Live Activity pushes. Send semantics match
// Sender: nil means the transport accepted the push, not that the device
// received it; terminal failures arrive via the feedback webhook (§6.5).
type LiveActivitySender interface {
	SendLiveActivity(ctx context.Context, p LiveActivityPush) error
}
