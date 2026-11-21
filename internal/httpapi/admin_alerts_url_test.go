package httpapi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// badAlertURLs are the SC-07 shapes every authoring path must refuse.
var badAlertURLs = map[string]string{
	"javascript":    "javascript:alert(1)",
	"data":          "data:text/html,<b>x</b>",
	"protocol-rel":  "//evil.example/x",
	"relative path": "/detours",
	"5KB":           "https://example.org/" + strings.Repeat("a", 5000),
}

// TestAdminAlerts_URLValidated (SC-07): create and patch refuse a url that
// is not empty or an absolute http(s) URL with a host, and a refused patch
// leaves the stored url alone.
func TestAdminAlerts_URLValidated(t *testing.T) {
	t.Parallel()
	f := newAdminFixture(t)
	for name, u := range badAlertURLs {
		body := fmt.Sprintf(`{"header":"x","start_time":"2026-08-15T14:00:00-07:00","url":%q}`, u)
		assertContains(t, name, errorText(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", body), http.StatusBadRequest), "url")
	}
	if n := f.countAlerts(t); n != 0 {
		t.Fatalf("rejected creates stored %d alerts", n)
	}

	id := f.createAlertIn(t, regionPuget, `{"header":"x","start_time":"2026-08-15T14:00:00-07:00","url":"https://example.org/ok"}`)
	for name, u := range badAlertURLs {
		assertContains(t, name, errorText(t, f.do(http.MethodPatch, alertPath(regionPuget, id, ""), fmt.Sprintf(`{"url":%q}`, u)), http.StatusBadRequest), "url")
	}
	if got := f.storedAlert(t, id).URL; got != "https://example.org/ok" {
		t.Fatalf("rejected patch changed the url to %q", got)
	}
	// Clearing it is still allowed.
	if rec := f.do(http.MethodPatch, alertPath(regionPuget, id, ""), `{"url":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clearing the url: %d %s", rec.Code, rec.Body)
	}
}
