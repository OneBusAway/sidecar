package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestAlertCommands_URLValidated (SC-07): the CLI applies the same url rule
// as the admin API, on create and on edit.
func TestAlertCommands_URLValidated(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	bad := []string{"javascript:alert(1)", "data:text/html,x", "//evil.example/x", "/detours", "https://example.org/" + strings.Repeat("a", 5000)}
	for _, u := range bad {
		cliErrContains(t, dbPath, "url", "alert", "create", "--region", "1", "--agency-id", "1",
			"--header", "h", "--start", "2026-08-15T14:00:00Z", "--url", u)
	}
	assertNoAlerts(t, store, 1)

	stdout, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--agency-id", "1",
		"--header", "h", "--start", "2026-08-15T14:00:00Z", "--url", "https://example.org/ok")
	if err != nil {
		t.Fatal(err)
	}
	id := parseCreatedID(t, stdout)
	for _, u := range bad {
		cliErrContains(t, dbPath, "url", "alert", "edit", strconv.FormatInt(id, 10), "--url", u)
	}
	a, err := store.Alerts().Get(t.Context(), id)
	if err != nil || a.URL != "https://example.org/ok" {
		t.Fatalf("rejected edit changed the url: %q %v", a.URL, err)
	}
}
