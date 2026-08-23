package push

import (
	"context"
	"strings"
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
