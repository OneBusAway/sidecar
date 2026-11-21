package alerts_test

import (
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

// TestValidateURL (SC-07): the alert url ships verbatim in the public feed
// and is dropped into link hrefs by the apps and the admin SPA, so it must
// be empty or an absolute http(s) URL with a host.
func TestValidateURL(t *testing.T) {
	t.Parallel()
	ok := []string{"", "https://example.org/detours", "http://example.org", "HTTPS://Example.org/a?b=c#d"}
	for _, u := range ok {
		if err := alerts.ValidateURL(u); err != nil {
			t.Errorf("ValidateURL(%q) = %v; want ok", u, err)
		}
	}
	bad := []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"//example.org/path",
		"/relative/path",
		"example.org/path",
		"https://",
		"https:///nohost",
		"ftp://example.org/file",
		" https://example.org",
		"https://example.org/\x00",
		"https://example.org/" + strings.Repeat("a", 5000),
	}
	for _, u := range bad {
		if err := alerts.ValidateURL(u); err == nil {
			t.Errorf("ValidateURL(%.60q) accepted it", u)
		}
	}
	if err := alerts.ValidateURL("https://example.org/" + strings.Repeat("a", alerts.MaxURLLen-len("https://example.org/"))); err != nil {
		t.Errorf("URL of exactly MaxURLLen rejected: %v", err)
	}
	if err := alerts.ValidateURL("https://example.org/" + strings.Repeat("a", alerts.MaxURLLen-len("https://example.org/")+1)); err == nil {
		t.Error("URL one byte over MaxURLLen accepted")
	}
}
