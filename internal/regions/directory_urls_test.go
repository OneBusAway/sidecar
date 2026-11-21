package regions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// TestFetch_ValidatesBaseURLs (SC-22): obaBaseUrl is where this server
// sends region API keys, and sidecarBaseUrl is echoed to the apps in alarm
// and Live Activity URLs, so an entry whose obaBaseUrl -- or non-empty
// sidecarBaseUrl -- is not an absolute http(s) URL with a host is skipped
// like any other invalid entry. Plain http and loopback/private hosts stay
// allowed: local development and OBACloud's contract harness use
// http://127.0.0.1, and the directory is operator-controlled.
func TestFetch_ValidatesBaseURLs(t *testing.T) {
	t.Parallel()
	entries := []struct {
		id           int
		oba, sidecar string
		want         bool
	}{
		{1, "https://api.example.org/", "https://sidecar.example.org", true},
		{2, "http://127.0.0.1:8080/", "http://127.0.0.1:8481", true},
		{3, "http://10.0.0.5/", "", true},
		{4, "javascript:alert(1)", "", false},
		{5, "file:///etc/passwd", "", false},
		{6, "//api.example.org/", "", false},
		{7, "/relative/", "", false},
		{8, "https://", "", false},
		{9, "gopher://api.example.org/", "", false},
		{10, "https://api.example.org/", "javascript:alert(1)", false},
		{11, "https://api.example.org/", "ftp://sidecar.example.org", false},
		{12, "https://api.example.org/", "sidecar.example.org", false},
	}
	var list []string
	for _, e := range entries {
		list = append(list, fmt.Sprintf(`{"id":%d,"regionName":"R%d","obaBaseUrl":%q,"sidecarBaseUrl":%q,"active":true}`, e.id, e.id, e.oba, e.sidecar))
	}
	body := `{"version":3,"code":200,"text":"OK","data":{"list":[` + strings.Join(list, ",") + `]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	got, err := regions.NewClient(srv.URL, testOptions()).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kept := map[int64]bool{}
	for _, r := range got {
		kept[r.ID] = true
	}
	for _, e := range entries {
		if kept[int64(e.id)] != e.want {
			t.Errorf("entry %d (oba %q, sidecar %q): kept=%v, want %v", e.id, e.oba, e.sidecar, kept[int64(e.id)], e.want)
		}
	}
}
