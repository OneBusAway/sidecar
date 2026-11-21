package alerts

import (
	"errors"
	"fmt"
	"net/url"
)

// MaxURLLen caps an alert's informational url in bytes. Browsers and the
// apps cope with far longer, but nothing legitimate needs more, and the
// value ships in every feed fetch.
const MaxURLLen = 2048

// ValidateURL is the one rule for an alert's url, shared by the admin API,
// the CLI, and import: empty (no link), or an absolute http or https URL
// with a host, at most MaxURLLen bytes. The value is published verbatim in
// the GTFS-realtime feed and lands in link hrefs in the apps and the admin
// SPA, so javascript:, data:, protocol-relative, and relative values are
// refused rather than stored.
func ValidateURL(raw string) error {
	if raw == "" {
		return nil
	}
	if len(raw) > MaxURLLen {
		return fmt.Errorf("url is too long (%d bytes; maximum is %d)", len(raw), MaxURLLen)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("url must be empty or an absolute http:// or https:// URL")
	}
	return nil
}
