package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// The three seeded regions. Region 0 is deliberately one of them: it is a real
// region (Tampa Bay), so every "absent" sentinel in this API has to be
// something other than zero, and only a fixture that actually uses region 0
// can catch a handler that confuses the two.
const (
	regionTampa = 0 // default agency "HART", tz America/New_York
	regionPuget = 1 // default agency "1", tz America/Los_Angeles
	regionBare  = 2 // never configured: no default agency id, schema-default timezone
)

// adminFixture is the whole wired router -- real migrated SQLite store, real
// auth repository, real login -- with one admin account already logged in.
// These handlers exist to map HTTP onto real repository semantics (window
// validation, ErrNotFound, translation staleness hashes, region 0), so a stub
// store would only test that mapping against a second guess at those
// semantics rather than against the semantics themselves.
type adminFixture struct {
	handler http.Handler
	store   *sqlite.Store
	cookie  *http.Cookie
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()

	store := sqlitetest.Open(t)
	ctx := context.Background()

	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{
		{ID: regionTampa, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", SidecarBaseURL: "https://sidecar.example/tampa", Language: "en", Active: true},
		{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", SidecarBaseURL: "https://sidecar.example/puget", Language: "en", Active: true},
		{ID: regionBare, Name: "Unconfigured", OBABaseURL: "https://bare.example/", Language: "en", Active: false},
	}, testNow); err != nil {
		t.Fatalf("seed regions: %v", err)
	}
	if err := store.Regions().SetLocalFields(ctx, regionTampa, regions.LocalFields{
		DefaultAgencyID: "HART", Timezone: "America/New_York",
	}, testNow); err != nil {
		t.Fatalf("configure region %d: %v", regionTampa, err)
	}
	if err := store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles",
	}, testNow); err != nil {
		t.Fatalf("configure region %d: %v", regionPuget, err)
	}

	if _, err := store.Auth().CreateUser(ctx, "admin", testHash(), testNow); err != nil {
		t.Fatalf("create admin user: %v", err)
	}

	f := &adminFixture{store: store}
	f.handler = NewRouter(Deps{
		Alerts:  store.Alerts(),
		Regions: store.Regions(),
		Auth:    store.Auth(),
		Now:     func() time.Time { return testNow },
		Logger:  discardLogger(),
		Sleep:   func(time.Duration) {},
	})
	f.cookie = adminLogin(t, f.handler)
	return f
}

// adminLogin goes through the real login endpoint, so every request below
// carries a cookie the real session layer issued.
func adminLogin(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	rec := sendTo(h, http.MethodPost, "/api/admin/v1/session", credentials("admin", testPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	return responseSessionCookie(t, rec)
}

// sendTo issues a request with the given cookie (nil means unauthenticated).
// The headers are what a same-origin fetch from the SPA looks like, so the
// cross-site guard passes.
func sendTo(h http.Handler, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	req.Host = "sidecar.test"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// do issues an authenticated request.
func (f *adminFixture) do(method, target, body string) *httptest.ResponseRecorder {
	return sendTo(f.handler, method, target, body, f.cookie)
}

// object decodes a JSON object response, failing the test on a wrong status.
func object(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode object: %v; body = %s", err, rec.Body.String())
	}
	return m
}

// array decodes a JSON array response, failing the test on a wrong status.
func array(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) []map[string]any {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, wantStatus, rec.Body.String())
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode array: %v; body = %s", err, rec.Body.String())
	}
	return got
}

// errorText returns the {"error": ...} message of a response.
func errorText(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) string {
	t.Helper()
	return str(t, object(t, rec, wantStatus), "error")
}

// assertContains checks every fragment the caller expects in a message.
func assertContains(t *testing.T, what, got string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", what, got, want)
		}
	}
}

// createAlert posts an alert and returns its decoded body, failing on non-201.
func (f *adminFixture) createAlert(t *testing.T, body string) map[string]any {
	t.Helper()
	return object(t, f.do(http.MethodPost, "/api/admin/v1/alerts", body), http.StatusCreated)
}

// createAlertID posts an alert and returns its id.
func (f *adminFixture) createAlertID(t *testing.T, body string) int64 {
	t.Helper()
	return alertID(t, f.createAlert(t, body))
}

// countAlerts is the "nothing was written" check the rejection tests use.
func (f *adminFixture) countAlerts(t *testing.T) int {
	t.Helper()
	list, err := f.store.Alerts().List(context.Background(), alerts.ListFilter{})
	if err != nil {
		t.Fatalf("count alerts: %v", err)
	}
	return len(list)
}

// storedAlert reads an alert straight out of the store, for the assertions
// that must not go back through the API being tested.
func (f *adminFixture) storedAlert(t *testing.T, id int64) alerts.Alert {
	t.Helper()
	a, err := f.store.Alerts().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("stored alert %d: %v", id, err)
	}
	return a
}

// alertID reads the id out of a decoded alert body.
func alertID(t *testing.T, m map[string]any) int64 {
	t.Helper()
	v, ok := m["id"].(float64)
	if !ok {
		t.Fatalf("id = %v (%T), want a number", m["id"], m["id"])
	}
	return int64(v)
}

// str reads a string field, failing if it is absent or the wrong type.
func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("%s = %v (%T), want a string", key, m[key], m[key])
	}
	return v
}

func boolean(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("%s = %v (%T), want a bool", key, m[key], m[key])
	}
	return v
}

// assertKeys pins an object's exact field names. The SPA (tasks 10-11) is
// written against these; a rename is a silent break of a client this repo does
// not compile.
func assertKeys(t *testing.T, what string, m map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Errorf("%s fields = %v, want exactly %v", what, got, sorted)
	}
}

var (
	alertJSONFields = []string{
		"id", "region_id", "agency_id", "header", "description", "url",
		"cause", "effect", "severity", "start_time", "end_time",
		"published", "is_test", "created_at", "updated_at", "translations",
	}
	translationJSONFields = []string{"language", "header", "description"}
)

// translationsOf returns the translations array of a decoded alert.
func translationsOf(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["translations"].([]any)
	if !ok {
		t.Fatalf("translations = %v (%T), want an array", m["translations"], m["translations"])
	}
	out := make([]map[string]any, len(raw))
	for i, v := range raw {
		obj, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("translations[%d] = %v (%T), want an object", i, v, v)
		}
		out[i] = obj
	}
	return out
}

// minimalAlert is the smallest valid create body for a region that has a
// default agency id.
func minimalAlert(regionID int64, header string) string {
	return fmt.Sprintf(`{"region_id":%d,"header":%q,"start_time":"2026-08-15T14:00:00-07:00"}`, regionID, header)
}

func alertPath(id int64, suffix string) string {
	return fmt.Sprintf("/api/admin/v1/alerts/%d%s", id, suffix)
}

// ---------------------------------------------------------------------------
// route wiring
// ---------------------------------------------------------------------------

// TestAdminRoutes_EveryRouteRequiresASession is the assurance that no admin
// route can reach a handler without its middleware. http.ServeMux cannot be
// enumerated, so registerAdminRoutes registers from a table and this test
// walks the same table: a route added tomorrow is covered the moment it
// exists. The table declares which routes are deliberately session-free
// (login and logout, for documented reasons); every other one is proven to
// answer 401 unauthenticated through the fully wired router, so a route
// registered without requireSession fails here rather than shipping open.
func TestAdminRoutes_EveryRouteRequiresASession(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	// The only two routes allowed to skip requireSession, and why: login has
	// no session yet, and logout must stay idempotent for a SPA tidying up
	// after a 401 (design spec §4.4/§4.5). The set is pinned rather than
	// derived so a third route cannot quietly join it.
	sessionFree := map[string]bool{
		"POST /api/admin/v1/session":   true,
		"DELETE /api/admin/v1/session": true,
	}

	routes := adminRoutes(Deps{Logger: discardLogger()})
	if want := 14; len(routes) != want {
		t.Errorf("admin route table has %d routes, want %d (3 session + 9 alerts + 2 regions)", len(routes), want)
	}

	seen := map[string]bool{}
	for _, rt := range routes {
		if seen[rt.pattern] {
			t.Errorf("route %q registered twice", rt.pattern)
		}
		seen[rt.pattern] = true

		wantSession := !sessionFree[rt.pattern]
		if rt.requiresSession != wantSession {
			t.Errorf("route %q: requiresSession = %v, want %v", rt.pattern, rt.requiresSession, wantSession)
		}
		if !wantSession {
			continue
		}

		method, target := concreteRoute(t, rt.pattern)
		rec := sendTo(f.handler, method, target, "", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauthenticated: status = %d, want 401; body = %s",
				method, target, rec.Code, rec.Body.String())
			continue
		}
		if got := bodyText(rec); got != unauthorizedBody {
			t.Errorf("%s %s unauthenticated: body = %q, want %q", method, target, got, unauthorizedBody)
		}
	}

	for pattern := range sessionFree {
		if !seen[pattern] {
			t.Errorf("route table lost %q", pattern)
		}
	}
}

// TestAdminRoutes_EveryWriteIsCrossSiteGuarded: the guard covers every route,
// not just the session ones, so a browser-marked cross-site write is refused
// before the handler runs even with a valid cookie (design spec §4.4).
func TestAdminRoutes_EveryWriteIsCrossSiteGuarded(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	for _, rt := range adminRoutes(Deps{Logger: discardLogger()}) {
		method, target := concreteRoute(t, rt.pattern)
		if method == http.MethodGet || method == http.MethodHead {
			continue // the guard deliberately ignores safe methods
		}
		req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader("{}"))
		req.Host = "sidecar.test"
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(f.cookie)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s cross-site: status = %d, want 403; body = %s",
				method, target, rec.Code, rec.Body.String())
			continue
		}
		if got := bodyText(rec); got != crossSiteBody {
			t.Errorf("%s %s cross-site: body = %q, want %q", method, target, got, crossSiteBody)
		}
	}
}

// concreteRoute turns a ServeMux pattern into a method and a concrete path a
// request can actually be sent to.
func concreteRoute(t *testing.T, pattern string) (method, target string) {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("route pattern %q has no method", pattern)
	}
	path = strings.ReplaceAll(path, "{id}", "1")
	path = strings.ReplaceAll(path, "{lang}", "es")
	if strings.ContainsAny(path, "{}") {
		t.Fatalf("route pattern %q has a wildcard this test does not know how to fill", pattern)
	}
	return method, path
}

// ---------------------------------------------------------------------------
// POST /alerts
// ---------------------------------------------------------------------------

// TestAdminAlerts_CreateSuccess pins the whole 201 contract, including the
// exact field names tasks 10-11 build against.
func TestAdminAlerts_CreateSuccess(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/alerts", `{
		"region_id": 1,
		"agency_id": "explicit-agency",
		"header": "Route 44 detoured",
		"description": "Use 11th Ave",
		"url": "https://example.org/44",
		"cause": "construction",
		"effect": "DETOUR",
		"severity": "WARNING",
		"start_time": "2026-08-15T14:00:00-07:00",
		"end_time": "2026-08-16T14:00:00-07:00",
		"is_test": true
	}`)
	got := object(t, rec, http.StatusCreated)
	assertKeys(t, "alert", got, alertJSONFields)

	id := alertID(t, got)
	if want := alertPath(id, ""); rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	if got["region_id"] != float64(regionPuget) {
		t.Errorf("region_id = %v, want %d", got["region_id"], regionPuget)
	}
	// The explicit agency id must win over the region default ("1"). Nothing
	// else in the response distinguishes the two.
	if v := str(t, got, "agency_id"); v != "explicit-agency" {
		t.Errorf("agency_id = %q, want %q (the explicit value, not the region default)", v, "explicit-agency")
	}
	if v := str(t, got, "header"); v != "Route 44 detoured" {
		t.Errorf("header = %q", v)
	}
	if v := str(t, got, "description"); v != "Use 11th Ave" {
		t.Errorf("description = %q", v)
	}
	if v := str(t, got, "url"); v != "https://example.org/44" {
		t.Errorf("url = %q", v)
	}
	if v := str(t, got, "cause"); v != "CONSTRUCTION" {
		t.Errorf("cause = %q, want CONSTRUCTION (ParseCause normalizes case)", v)
	}
	if v := str(t, got, "effect"); v != "DETOUR" {
		t.Errorf("effect = %q", v)
	}
	if v := str(t, got, "severity"); v != "WARNING" {
		t.Errorf("severity = %q", v)
	}
	// Responses are RFC 3339 UTC regardless of the offset the author sent.
	if v := str(t, got, "start_time"); v != "2026-08-15T21:00:00Z" {
		t.Errorf("start_time = %q, want 2026-08-15T21:00:00Z", v)
	}
	if v := str(t, got, "end_time"); v != "2026-08-16T21:00:00Z" {
		t.Errorf("end_time = %q, want 2026-08-16T21:00:00Z", v)
	}
	if boolean(t, got, "published") {
		t.Error("published = true; a newly created alert is a draft")
	}
	if !boolean(t, got, "is_test") {
		t.Error("is_test = false, want true")
	}
	if v := str(t, got, "created_at"); v != testNow.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want %q (the injected clock)", v, testNow.Format(time.RFC3339))
	}
	if v := str(t, got, "updated_at"); v != testNow.Format(time.RFC3339) {
		t.Errorf("updated_at = %q, want %q", v, testNow.Format(time.RFC3339))
	}
	if tr := translationsOf(t, got); len(tr) != 0 {
		t.Errorf("translations = %v, want an empty array", tr)
	}

	stored := f.storedAlert(t, id)
	if stored.AgencyID != "explicit-agency" {
		t.Errorf("stored agency_id = %q, want explicit-agency", stored.AgencyID)
	}
	if want := time.Date(2026, 8, 15, 21, 0, 0, 0, time.UTC); !stored.StartTime.Equal(want) {
		t.Errorf("stored start = %v, want %v", stored.StartTime, want)
	}
}

// TestAdminAlerts_CreateResolvesAgency mirrors the CLI: explicit value, else
// the region default, else a 400 that says how to fix it.
func TestAdminAlerts_CreateResolvesAgency(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	t.Run("falls back to the region default", func(t *testing.T) {
		got := f.createAlert(t, minimalAlert(regionPuget, "region default"))
		if v := str(t, got, "agency_id"); v != "1" {
			t.Errorf("agency_id = %q, want the region default %q", v, "1")
		}
	})

	t.Run("region 0 is a real region", func(t *testing.T) {
		got := f.createAlert(t, minimalAlert(regionTampa, "tampa"))
		if got["region_id"] != float64(0) {
			t.Errorf("region_id = %v, want 0", got["region_id"])
		}
		if v := str(t, got, "agency_id"); v != "HART" {
			t.Errorf("agency_id = %q, want region 0's default %q", v, "HART")
		}
	})

	t.Run("no explicit value and no region default", func(t *testing.T) {
		rec := f.do(http.MethodPost, "/api/admin/v1/alerts", minimalAlert(regionBare, "bare"))
		assertContains(t, "error", errorText(t, rec, http.StatusBadRequest),
			"no agency_id given", "region 2", "PATCH /api/admin/v1/regions/2", "agency_id")
	})
}

// TestAdminAlerts_CreateRejections covers every 4xx the create path owes the
// SPA, each with the status that tells the client what to do about it, and
// each proving nothing was written.
func TestAdminAlerts_CreateRejections(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	tests := []struct {
		name        string
		body        string
		wantStatus  int
		wantInError []string
	}{
		{
			// The whole reason createAlertRequest.RegionID is a *int64: an
			// absent region_id must not silently become region 0, which is a
			// real region someone would then find surprise alerts in.
			name: "missing region_id", body: `{"header":"x","start_time":"2026-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"region_id"},
		},
		{
			name: "null region_id", body: `{"region_id":null,"header":"x","start_time":"2026-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"region_id"},
		},
		{
			name: "unknown region", body: `{"region_id":404,"header":"x","start_time":"2026-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusNotFound, wantInError: []string{"region", "404"},
		},
		{
			name:       "naive start time",
			body:       `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00"}`,
			wantStatus: http.StatusBadRequest,
			// A naive datetime is never guessed at: the message names the
			// region's configured zone so the author can write the offset.
			wantInError: []string{"RFC 3339", "explicit offset", "region 1", "America/Los_Angeles"},
		},
		{
			name: "naive end time", body: `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00-07:00","end_time":"2026-08-16T14:00:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"explicit offset"},
		},
		{
			name: "end before start", body: `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00-07:00","end_time":"2026-08-14T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"must be after start"},
		},
		{
			name: "start before the epoch guard", body: `{"region_id":1,"header":"x","start_time":"1999-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"check the year"},
		},
		{
			name: "unknown cause", body: `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00-07:00","cause":"NOT_A_CAUSE"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"unknown cause", "CONSTRUCTION"},
		},
		{
			name: "unknown effect", body: `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00-07:00","effect":"NOT_AN_EFFECT"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"unknown effect", "DETOUR"},
		},
		{
			name: "unknown severity", body: `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00-07:00","severity":"CATASTROPHIC"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"unknown severity", "WARNING"},
		},
		{
			name: "empty agency id", body: `{"region_id":1,"agency_id":"","header":"x","start_time":"2026-08-15T14:00:00-07:00"}`,
			// With a plain `string` field, JSON cannot distinguish an empty
			// agency_id from an absent one, so an empty value falls back to the
			// region default rather than erroring. The CLI *can* tell them
			// apart -- it tracks which flags were visited, so an explicit
			// `--agency-id ""` reaches the "no default agency id" error instead
			// of falling back. This row pins the API's answer, which must
			// never be an empty stored agency_id: that produces a feed entry
			// no OBA app matches.
			wantStatus: http.StatusCreated,
		},
		{
			name: "malformed JSON", body: `{"region_id":1,`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"invalid JSON"},
		},
		{
			name: "empty body", body: ` `,
			wantStatus: http.StatusBadRequest, wantInError: []string{"invalid JSON"},
		},
		{
			// alertRepo.Create also rejects this, but the handler must catch
			// it first: reaching the repository would surface as a bare 500,
			// and riders would never see a header-less alert reach the feed
			// in the first place.
			name: "missing header", body: `{"region_id":1,"start_time":"2026-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"header"},
		},
		{
			name: "empty header", body: `{"region_id":1,"header":"","start_time":"2026-08-15T14:00:00-07:00"}`,
			wantStatus: http.StatusBadRequest, wantInError: []string{"header"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.countAlerts(t)
			rec := f.do(http.MethodPost, "/api/admin/v1/alerts", tt.body)

			if tt.wantStatus == http.StatusCreated {
				got := object(t, rec, http.StatusCreated)
				if v := str(t, got, "agency_id"); v != "1" {
					t.Errorf("agency_id = %q, want the region default %q", v, "1")
				}
				return
			}

			assertContains(t, "error", errorText(t, rec, tt.wantStatus), tt.wantInError...)
			if after := f.countAlerts(t); after != before {
				t.Errorf("rejected create wrote %d alert(s)", after-before)
			}
		})
	}
}

// TestAdminAlerts_CreateEmptyEnumsDegrade: the enum fields are optional, and
// ParseX already turns "" into the UNKNOWN_* fallback.
func TestAdminAlerts_CreateEmptyEnumsDegrade(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	got := f.createAlert(t, minimalAlert(regionPuget, "no enums"))
	if v := str(t, got, "cause"); v != "UNKNOWN_CAUSE" {
		t.Errorf("cause = %q, want UNKNOWN_CAUSE", v)
	}
	if v := str(t, got, "effect"); v != "UNKNOWN_EFFECT" {
		t.Errorf("effect = %q, want UNKNOWN_EFFECT", v)
	}
	if v := str(t, got, "severity"); v != "UNKNOWN_SEVERITY" {
		t.Errorf("severity = %q, want UNKNOWN_SEVERITY", v)
	}
	if _, ok := got["end_time"]; !ok {
		t.Error("end_time is absent; it must be present and null so the SPA can read it unconditionally")
	}
	if got["end_time"] != nil {
		t.Errorf("end_time = %v, want null for an open-ended alert", got["end_time"])
	}
}

// ---------------------------------------------------------------------------
// GET /alerts
// ---------------------------------------------------------------------------

// TestAdminAlerts_List covers the filter semantics, the easiest thing in this
// API to get subtly wrong: `region` absent means every region, and region 0 is
// a real value rather than "unset".
func TestAdminAlerts_List(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	tampa := f.createAlertID(t, minimalAlert(regionTampa, "tampa alert"))
	puget := f.createAlertID(t, minimalAlert(regionPuget, "puget alert"))

	idsOf := func(t *testing.T, rec *httptest.ResponseRecorder) []int64 {
		t.Helper()
		var out []int64
		for _, m := range array(t, rec, http.StatusOK) {
			out = append(out, alertID(t, m))
		}
		sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
		return out
	}
	equal := func(a, b []int64) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	t.Run("no region parameter lists every region", func(t *testing.T) {
		got := idsOf(t, f.do(http.MethodGet, "/api/admin/v1/alerts", ""))
		if want := []int64{tampa, puget}; !equal(got, want) {
			t.Errorf("ids = %v, want %v (an absent region filter means all regions, not region 0)", got, want)
		}
	})

	t.Run("region=0 filters to region 0", func(t *testing.T) {
		got := idsOf(t, f.do(http.MethodGet, "/api/admin/v1/alerts?region=0", ""))
		if want := []int64{tampa}; !equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("region=1 filters to region 1", func(t *testing.T) {
		got := idsOf(t, f.do(http.MethodGet, "/api/admin/v1/alerts?region=1", ""))
		if want := []int64{puget}; !equal(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
	})

	t.Run("unknown region is an empty array, not a 404", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/api/admin/v1/alerts?region=999", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		// Literally "[]": a nil slice marshals to null, which every caller in
		// the SPA would then have to special-case.
		if body := bodyText(rec); body != "[]" {
			t.Errorf("body = %q, want %q", body, "[]")
		}
	})

	t.Run("non-integer region is a 400", func(t *testing.T) {
		for _, q := range []string{"?region=abc", "?region=", "?region=1.5", "?region=" + strings.Repeat("9", 30)} {
			rec := f.do(http.MethodGet, "/api/admin/v1/alerts"+q, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET /alerts%s: status = %d, want 400; body = %s", q, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("list items carry the full alert shape", func(t *testing.T) {
		items := array(t, f.do(http.MethodGet, "/api/admin/v1/alerts?region=1", ""), http.StatusOK)
		if len(items) != 1 {
			t.Fatalf("items = %d, want 1", len(items))
		}
		assertKeys(t, "listed alert", items[0], alertJSONFields)
	})

	t.Run("drafts are included; this is the authoring view", func(t *testing.T) {
		items := array(t, f.do(http.MethodGet, "/api/admin/v1/alerts", ""), http.StatusOK)
		if len(items) == 0 {
			t.Fatal("the authoring list dropped every draft")
		}
		for _, m := range items {
			if boolean(t, m, "published") {
				t.Errorf("alert %d is published; the fixture only created drafts", alertID(t, m))
			}
		}
	})
}

// ---------------------------------------------------------------------------
// GET /alerts/{id}
// ---------------------------------------------------------------------------

func TestAdminAlerts_Get(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, minimalAlert(regionPuget, "fetch me"))

	t.Run("found", func(t *testing.T) {
		got := object(t, f.do(http.MethodGet, alertPath(id, ""), ""), http.StatusOK)
		assertKeys(t, "alert", got, alertJSONFields)
		if alertID(t, got) != id {
			t.Errorf("id = %v, want %d", got["id"], id)
		}
		if v := str(t, got, "header"); v != "fetch me" {
			t.Errorf("header = %q, want %q", v, "fetch me")
		}
	})

	t.Run("unknown id is a 404", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/api/admin/v1/alerts/99999", "")
		assertContains(t, "error", errorText(t, rec, http.StatusNotFound), "not found")
	})

	t.Run("non-integer id is a 400", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/api/admin/v1/alerts/abc", "")
		assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "id")
	})
}

// ---------------------------------------------------------------------------
// PATCH /alerts/{id}
// ---------------------------------------------------------------------------

// TestAdminAlerts_PatchAppliesOnlyWhatWasSent is the pointer-field contract:
// an absent field leaves the stored value alone.
func TestAdminAlerts_PatchAppliesOnlyWhatWasSent(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	created := f.createAlert(t, `{
		"region_id": 1, "agency_id": "before", "header": "before header",
		"description": "before description", "url": "https://before.example",
		"cause": "CONSTRUCTION", "effect": "DETOUR", "severity": "WARNING",
		"start_time": "2026-08-15T14:00:00-07:00", "end_time": "2026-08-20T14:00:00-07:00",
		"is_test": true
	}`)
	id := alertID(t, created)

	got := object(t, f.do(http.MethodPatch, alertPath(id, ""),
		`{"header":"after header","severity":"SEVERE"}`), http.StatusOK)
	assertKeys(t, "alert", got, alertJSONFields)

	if v := str(t, got, "header"); v != "after header" {
		t.Errorf("header = %q, want %q", v, "after header")
	}
	if v := str(t, got, "severity"); v != "SEVERE" {
		t.Errorf("severity = %q, want SEVERE", v)
	}
	for _, field := range []string{"region_id", "agency_id", "description", "url", "cause", "effect", "start_time", "end_time", "published", "is_test", "created_at"} {
		if got[field] != created[field] {
			t.Errorf("%s = %v, want %v unchanged", field, got[field], created[field])
		}
	}
}

// TestAdminAlerts_PatchEndTime covers the one field JSON cannot express with
// null alone: clear_end_time is a distinct flag, and ignoring it would leave a
// stale end time on an alert an author explicitly reopened.
func TestAdminAlerts_PatchEndTime(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	windowed := `{"region_id":1,"header":"windowed","start_time":"2026-08-15T14:00:00-07:00","end_time":"2026-08-20T14:00:00-07:00"}`

	t.Run("clear_end_time reopens the alert", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		got := object(t, f.do(http.MethodPatch, alertPath(id, ""), `{"clear_end_time":true}`), http.StatusOK)
		if got["end_time"] != nil {
			t.Errorf("end_time = %v, want null after clear_end_time", got["end_time"])
		}
		if stored := f.storedAlert(t, id); stored.EndTime != nil {
			t.Errorf("stored end time = %v, want nil; clear_end_time was ignored", *stored.EndTime)
		}
	})

	t.Run("end_time replaces the window", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		got := object(t, f.do(http.MethodPatch, alertPath(id, ""),
			`{"end_time":"2026-08-25T14:00:00-07:00"}`), http.StatusOK)
		if v := str(t, got, "end_time"); v != "2026-08-25T21:00:00Z" {
			t.Errorf("end_time = %q, want 2026-08-25T21:00:00Z", v)
		}
	})

	t.Run("an absent end_time leaves the window alone", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		got := object(t, f.do(http.MethodPatch, alertPath(id, ""), `{"header":"only the header"}`), http.StatusOK)
		if v := str(t, got, "end_time"); v != "2026-08-20T21:00:00Z" {
			t.Errorf("end_time = %q, want the original 2026-08-20T21:00:00Z", v)
		}
	})

	t.Run("end_time and clear_end_time together are a 400", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		rec := f.do(http.MethodPatch, alertPath(id, ""),
			`{"end_time":"2026-08-25T14:00:00-07:00","clear_end_time":true}`)
		assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "end_time", "clear_end_time")
		if stored := f.storedAlert(t, id); stored.EndTime == nil {
			t.Error("a rejected patch cleared the end time anyway")
		}
	})

	t.Run("a new end time is validated against the stored start", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		rec := f.do(http.MethodPatch, alertPath(id, ""), `{"end_time":"2026-08-01T14:00:00-07:00"}`)
		assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "must be after start")
	})

	t.Run("a new start time is validated against the stored end", func(t *testing.T) {
		id := f.createAlertID(t, windowed)
		rec := f.do(http.MethodPatch, alertPath(id, ""), `{"start_time":"2026-09-01T14:00:00-07:00"}`)
		assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "must be after start")
	})

	t.Run("clear_end_time rescues a start moved past the old end", func(t *testing.T) {
		// The merged view is what gets validated: clearing the end and moving
		// the start past it in one request is legal, and a handler that
		// validated the new start against the *stored* end would reject it.
		id := f.createAlertID(t, windowed)
		got := object(t, f.do(http.MethodPatch, alertPath(id, ""),
			`{"start_time":"2026-09-01T14:00:00-07:00","clear_end_time":true}`), http.StatusOK)
		if got["end_time"] != nil {
			t.Errorf("end_time = %v, want null", got["end_time"])
		}
		if v := str(t, got, "start_time"); v != "2026-09-01T21:00:00Z" {
			t.Errorf("start_time = %q, want 2026-09-01T21:00:00Z", v)
		}
	})
}

func TestAdminAlerts_PatchRejections(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, minimalAlert(regionPuget, "patch target"))
	target := alertPath(id, "")

	tests := []struct {
		name        string
		path        string
		body        string
		wantStatus  int
		wantInError []string
	}{
		{"unknown id", "/api/admin/v1/alerts/99999", `{"header":"x"}`, http.StatusNotFound, []string{"not found"}},
		{"non-integer id", "/api/admin/v1/alerts/abc", `{"header":"x"}`, http.StatusBadRequest, []string{"id"}},
		{"naive start time", target, `{"start_time":"2026-08-15T14:00:00"}`, http.StatusBadRequest, []string{"explicit offset", "America/Los_Angeles"}},
		{"naive end time", target, `{"end_time":"2026-08-15T14:00:00"}`, http.StatusBadRequest, []string{"explicit offset"}},
		{"empty agency id", target, `{"agency_id":""}`, http.StatusBadRequest, []string{"agency_id"}},
		{"empty header", target, `{"header":""}`, http.StatusBadRequest, []string{"header"}},
		{"unknown cause", target, `{"cause":"NOPE"}`, http.StatusBadRequest, []string{"unknown cause"}},
		{"unknown effect", target, `{"effect":"NOPE"}`, http.StatusBadRequest, []string{"unknown effect"}},
		{"unknown severity", target, `{"severity":"NOPE"}`, http.StatusBadRequest, []string{"unknown severity"}},
		{"malformed JSON", target, `{"header":`, http.StatusBadRequest, []string{"invalid JSON"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertContains(t, "error", errorText(t, f.do(http.MethodPatch, tt.path, tt.body), tt.wantStatus), tt.wantInError...)
		})
	}

	// None of the rejected patches may have touched the row.
	stored := f.storedAlert(t, id)
	if stored.HeaderText != "patch target" || stored.AgencyID != "1" {
		t.Errorf("rejected patches mutated the alert: header = %q, agency = %q", stored.HeaderText, stored.AgencyID)
	}
	if !stored.UpdatedAt.Equal(testNow) {
		t.Errorf("stored updated_at = %v, want %v", stored.UpdatedAt, testNow)
	}
}

// TestAdminAlerts_PatchIsTestFalse: is_test is a *bool precisely so that
// sending false clears the flag instead of reading as "absent". A test alert
// that stays flagged after an author promotes it never reaches riders.
func TestAdminAlerts_PatchIsTestFalse(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, `{"region_id":1,"header":"promote me","start_time":"2026-08-15T14:00:00-07:00","is_test":true}`)

	got := object(t, f.do(http.MethodPatch, alertPath(id, ""), `{"is_test":false}`), http.StatusOK)
	if boolean(t, got, "is_test") {
		t.Error("is_test = true, want false")
	}
	if f.storedAlert(t, id).IsTest {
		t.Error("stored is_test = true, want false")
	}
}

// TestAdminAlerts_PatchKeepsTranslations: the PATCH response is the full alert,
// which means the translations have to come back with it -- the repository's
// Update return value carries none, so a handler that skipped the re-read
// would tell the SPA every translation had just been deleted.
func TestAdminAlerts_PatchKeepsTranslations(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, minimalAlert(regionPuget, "translated"))
	f.do(http.MethodPut, alertPath(id, "/translations/es"), `{"header":"Encabezado"}`)

	got := object(t, f.do(http.MethodPatch, alertPath(id, ""), `{"url":"https://example.org"}`), http.StatusOK)
	if tr := translationsOf(t, got); len(tr) != 1 {
		t.Errorf("translations = %v, want the one es translation", tr)
	}
}

// ---------------------------------------------------------------------------
// publish / unpublish / delete
// ---------------------------------------------------------------------------

func TestAdminAlerts_PublishUnpublish(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, minimalAlert(regionPuget, "publish me"))

	got := object(t, f.do(http.MethodPost, alertPath(id, "/publish"), ""), http.StatusOK)
	assertKeys(t, "published alert", got, alertJSONFields)
	if !boolean(t, got, "published") {
		t.Error("published = false after POST /publish")
	}
	if !f.storedAlert(t, id).Published {
		t.Error("stored published = false after POST /publish")
	}

	got = object(t, f.do(http.MethodPost, alertPath(id, "/unpublish"), ""), http.StatusOK)
	if boolean(t, got, "published") {
		t.Error("published = true after POST /unpublish")
	}
	if f.storedAlert(t, id).Published {
		t.Error("stored published = true after POST /unpublish")
	}

	for _, suffix := range []string{"/publish", "/unpublish"} {
		rec := f.do(http.MethodPost, "/api/admin/v1/alerts/99999"+suffix, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /alerts/99999%s: status = %d, want 404; body = %s", suffix, rec.Code, rec.Body.String())
		}
	}
}

func TestAdminAlerts_Delete(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, minimalAlert(regionPuget, "delete me"))
	target := alertPath(id, "")

	rec := f.do(http.MethodDelete, target, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 body = %q, want empty", rec.Body.String())
	}

	if after := f.do(http.MethodGet, target, ""); after.Code != http.StatusNotFound {
		t.Errorf("GET after delete: status = %d, want 404", after.Code)
	}
	if again := f.do(http.MethodDelete, target, ""); again.Code != http.StatusNotFound {
		t.Errorf("second delete: status = %d, want 404", again.Code)
	}
}

// ---------------------------------------------------------------------------
// translations
// ---------------------------------------------------------------------------

// TestAdminAlerts_Translations covers the per-field storage / per-language API
// mapping, and the staleness hash that decides whether the feed will ever show
// a translation to a rider.
func TestAdminAlerts_Translations(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertID(t, `{"region_id":1,"header":"English header","description":"English description","start_time":"2026-08-15T14:00:00-07:00"}`)

	t.Run("header only leaves description null", func(t *testing.T) {
		got := object(t, f.do(http.MethodPut, alertPath(id, "/translations/es"), `{"header":"Encabezado"}`), http.StatusOK)
		assertKeys(t, "alert", got, alertJSONFields)
		tr := translationsOf(t, got)
		if len(tr) != 1 {
			t.Fatalf("translations = %v, want exactly one", tr)
		}
		assertKeys(t, "translation", tr[0], translationJSONFields)
		if v := str(t, tr[0], "language"); v != "es" {
			t.Errorf("language = %q, want es", v)
		}
		if v := str(t, tr[0], "header"); v != "Encabezado" {
			t.Errorf("header = %q", v)
		}
		if tr[0]["description"] != nil {
			t.Errorf("description = %v, want null (no translation for that field)", tr[0]["description"])
		}
	})

	t.Run("source hash is the current English of that field", func(t *testing.T) {
		f.do(http.MethodPut, alertPath(id, "/translations/fr"), `{"header":"En-tete","description":"La description"}`)
		var checked int
		for _, tr := range f.storedAlert(t, id).Translations {
			if tr.Language != "fr" {
				continue
			}
			checked++
			var want string
			switch tr.Field {
			case alerts.FieldHeader:
				want = alerts.SourceHash("English header")
			case alerts.FieldDescription:
				want = alerts.SourceHash("English description")
			default:
				t.Fatalf("unexpected field %q", tr.Field)
			}
			// A wrong hash here is invisible in every API response and makes
			// the feed withhold the translation forever.
			if tr.SourceSHA256 != want {
				t.Errorf("%s SourceSHA256 = %q, want the hash of the current English text (%q)",
					tr.Field, tr.SourceSHA256, want)
			}
		}
		if checked != 2 {
			t.Errorf("stored fr rows = %d, want 2 (header and description)", checked)
		}
	})

	t.Run("re-upserting replaces the text", func(t *testing.T) {
		got := object(t, f.do(http.MethodPut, alertPath(id, "/translations/es"), `{"header":"Encabezado nuevo"}`), http.StatusOK)
		for _, tr := range translationsOf(t, got) {
			if str(t, tr, "language") == "es" {
				if v := str(t, tr, "header"); v != "Encabezado nuevo" {
					t.Errorf("es header = %q, want the replacement", v)
				}
			}
		}
	})

	t.Run("language tags are normalized", func(t *testing.T) {
		f.do(http.MethodPut, alertPath(id, "/translations/DE"), `{"header":"Kopfzeile"}`)
		got := object(t, f.do(http.MethodGet, alertPath(id, ""), ""), http.StatusOK)
		var found bool
		for _, tr := range translationsOf(t, got) {
			switch str(t, tr, "language") {
			case "de":
				found = true
			case "DE":
				t.Error("language DE was stored unnormalized")
			}
		}
		if !found {
			t.Error("no de translation after PUT .../translations/DE")
		}
	})

	t.Run("translations are grouped per language", func(t *testing.T) {
		tr := translationsOf(t, object(t, f.do(http.MethodGet, alertPath(id, ""), ""), http.StatusOK))
		seen := map[string]bool{}
		for _, one := range tr {
			lang := str(t, one, "language")
			if seen[lang] {
				t.Errorf("language %q appears twice; storage is per field but the API groups per language", lang)
			}
			seen[lang] = true
		}
		for _, lang := range []string{"es", "fr", "de"} {
			if !seen[lang] {
				t.Errorf("missing %q in %v", lang, tr)
			}
		}
	})

	t.Run("neither field is a 400", func(t *testing.T) {
		rec := f.do(http.MethodPut, alertPath(id, "/translations/it"), `{}`)
		if got, want := errorText(t, rec, http.StatusBadRequest), "provide header and/or description"; got != want {
			t.Errorf("error = %q, want %q", got, want)
		}
	})

	t.Run("null fields are a 400", func(t *testing.T) {
		rec := f.do(http.MethodPut, alertPath(id, "/translations/it"), `{"header":null,"description":null}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("an empty string is a real translation, not an omission", func(t *testing.T) {
		got := object(t, f.do(http.MethodPut, alertPath(id, "/translations/pt"), `{"description":""}`), http.StatusOK)
		var found bool
		for _, tr := range translationsOf(t, got) {
			if str(t, tr, "language") != "pt" {
				continue
			}
			found = true
			if tr["description"] == nil {
				t.Error("pt description = null, want the empty string that was sent")
			}
			if tr["header"] != nil {
				t.Errorf("pt header = %v, want null", tr["header"])
			}
		}
		if !found {
			t.Error("no pt translation stored")
		}
	})

	t.Run("unknown alert is a 404", func(t *testing.T) {
		rec := f.do(http.MethodPut, "/api/admin/v1/alerts/99999/translations/es", `{"header":"x"}`)
		assertContains(t, "error", errorText(t, rec, http.StatusNotFound), "not found")
	})

	t.Run("delete removes every field row for the language", func(t *testing.T) {
		rec := f.do(http.MethodDelete, alertPath(id, "/translations/fr"), "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != 0 {
			t.Errorf("204 body = %q, want empty", rec.Body.String())
		}
		for _, tr := range f.storedAlert(t, id).Translations {
			if tr.Language == "fr" {
				t.Errorf("fr/%s survived the delete", tr.Field)
			}
		}
	})

	t.Run("deleting an absent language is a 404", func(t *testing.T) {
		rec := f.do(http.MethodDelete, alertPath(id, "/translations/nl"), "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("deleting on an unknown alert is a 404", func(t *testing.T) {
		rec := f.do(http.MethodDelete, "/api/admin/v1/alerts/99999/translations/es", "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
		}
	})

	// A tag that normalizes to nothing is the same malformed request whichever
	// method carries it. PUT must refuse it because an empty language would
	// store a translation row the feed emits with no language tag at all;
	// DELETE answers the same way rather than reporting 404 (which is what
	// "matched no rows" would otherwise produce) so identical input does not
	// get two different explanations.
	t.Run("a blank language tag is a 400 on both methods", func(t *testing.T) {
		for _, m := range []struct {
			method, body string
		}{
			{http.MethodPut, `{"header":"x"}`},
			{http.MethodDelete, ""},
		} {
			rec := f.do(m.method, alertPath(id, "/translations/%20"), m.body)
			assertContains(t, m.method+" error", errorText(t, rec, http.StatusBadRequest), "language")
		}
		for _, tr := range f.storedAlert(t, id).Translations {
			if alerts.NormalizeLanguage(tr.Language) == "" {
				t.Errorf("stored a translation with a blank language tag: %+v", tr)
			}
		}
	})
}

// TestFormatInstant pins the global rule that response timestamps are RFC 3339
// UTC. The store happens to hand back UTC times today, so nothing that goes
// through it can catch a dropped .UTC() -- only a value carrying a real offset
// can.
func TestFormatInstant(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{"offset is converted", time.Date(2026, 8, 15, 14, 0, 0, 0, time.FixedZone("PDT", -7*3600)), "2026-08-15T21:00:00Z"},
		{"positive offset is converted", time.Date(2026, 8, 16, 2, 45, 0, 0, time.FixedZone("NPT", 5*3600+45*60)), "2026-08-15T21:00:00Z"},
		{"already UTC", time.Date(2026, 8, 15, 21, 0, 0, 0, time.UTC), "2026-08-15T21:00:00Z"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := formatInstant(tt.in); got != tt.want {
				t.Errorf("formatInstant(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// recordingMux collects the patterns registerAdminRoutes registers.
type recordingMux struct {
	patterns []string
}

func (m *recordingMux) Handle(pattern string, _ http.Handler) {
	m.patterns = append(m.patterns, pattern)
}

// TestRegisterAdminRoutes_RegistersExactlyTheTable is the other half of the
// route-wiring assurance. TestAdminRoutes_EveryRouteRequiresASession proves
// every route *in the table* carries its middleware; this proves nothing is
// mounted that is not in the table. A bare mux.Handle added inside
// registerAdminRoutes would otherwise ship an admin handler with no session
// requirement that no test in this package could see, because http.ServeMux
// cannot be enumerated.
func TestRegisterAdminRoutes_RegistersExactlyTheTable(t *testing.T) {
	t.Parallel()

	deps := Deps{Logger: discardLogger()}
	rec := &recordingMux{}
	registerAdminRoutes(rec, deps)

	want := make([]string, 0, len(adminRoutes(deps)))
	for _, rt := range adminRoutes(deps) {
		want = append(want, rt.pattern)
	}
	got := append([]string(nil), rec.patterns...)
	sort.Strings(got)
	sort.Strings(want)

	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("registered patterns:\n  got  %v\n  want %v (exactly the adminRoutes table)", got, want)
	}
}

// ---------------------------------------------------------------------------
// store failures
// ---------------------------------------------------------------------------

// errStoreBroken is what the failing repositories below return; no part of it
// may ever appear in a response body.
var errStoreBroken = errors.New("database is locked")

// failingAlerts fails every call, so the handlers' store-error paths can be
// exercised without corrupting a real database. Every method fails: a
// repository that broke only on the call a test happened to pick would let a
// missed error check survive.
type failingAlerts struct{}

func (failingAlerts) Create(context.Context, alerts.NewAlert, time.Time) (alerts.Alert, error) {
	return alerts.Alert{}, errStoreBroken
}
func (failingAlerts) Get(context.Context, int64) (alerts.Alert, error) {
	return alerts.Alert{}, errStoreBroken
}
func (failingAlerts) Update(context.Context, int64, alerts.Patch, time.Time) (alerts.Alert, error) {
	return alerts.Alert{}, errStoreBroken
}
func (failingAlerts) SetPublished(context.Context, int64, bool, time.Time) error {
	return errStoreBroken
}
func (failingAlerts) Delete(context.Context, int64) error { return errStoreBroken }
func (failingAlerts) List(context.Context, alerts.ListFilter) ([]alerts.Alert, error) {
	return nil, errStoreBroken
}
func (failingAlerts) Feed(context.Context, int64, bool, int) ([]alerts.Alert, error) {
	return nil, errStoreBroken
}
func (failingAlerts) UpsertTranslation(context.Context, int64, alerts.Translation, time.Time) error {
	return errStoreBroken
}
func (failingAlerts) DeleteTranslation(context.Context, int64, string) error { return errStoreBroken }

// failingRegions fails every call, for the same reason failingAlerts does.
type failingRegions struct{}

func (failingRegions) Get(context.Context, int64) (regions.Region, error) {
	return regions.Region{}, errStoreBroken
}
func (failingRegions) List(context.Context) ([]regions.Region, error) { return nil, errStoreBroken }
func (failingRegions) UpsertFromDirectory(context.Context, []regions.Region, time.Time) error {
	return errStoreBroken
}
func (failingRegions) SetLocalFields(context.Context, int64, regions.LocalFields, time.Time) error {
	return errStoreBroken
}

// TestAdminAPI_StoreFailuresAre500 pins the last rule of the API contract: a
// broken store is a logged 500 with one fixed body on every route, never a 4xx
// that would send an operator hunting for a client mistake, and never the
// driver's own message on the client's screen. The auth store still works, so
// these are the handlers' own error paths rather than the middleware's.
func TestAdminAPI_StoreFailuresAre500(t *testing.T) {
	t.Parallel()

	repo := newStubAuth()
	repo.addUser("admin", testHash())
	h := NewRouter(Deps{
		Alerts:  failingAlerts{},
		Regions: failingRegions{},
		Auth:    repo,
		Now:     func() time.Time { return testNow },
		Logger:  discardLogger(),
		Sleep:   func(time.Duration) {},
	})
	cookie := adminLogin(t, h)

	for _, rt := range adminRoutes(Deps{Logger: discardLogger()}) {
		if strings.HasSuffix(rt.pattern, "/session") {
			continue // the session routes do not touch these two stores
		}
		method, target := concreteRoute(t, rt.pattern)
		body := ""
		switch {
		case method == http.MethodPost && strings.HasSuffix(target, "/alerts"):
			body = minimalAlert(regionPuget, "x")
		case method == http.MethodPatch && strings.Contains(target, "/regions/"):
			body = `{"default_agency_id":"x"}`
		case method == http.MethodPatch:
			body = `{"header":"x"}`
		case method == http.MethodPut:
			body = `{"header":"x"}`
		}

		rec := sendTo(h, method, target, body, cookie)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s %s: status = %d, want 500; body = %s", method, target, rec.Code, rec.Body.String())
			continue
		}
		if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
			t.Errorf("%s %s: body = %q, want %q", method, target, got, want)
		}
		if strings.Contains(rec.Body.String(), errStoreBroken.Error()) {
			t.Errorf("%s %s leaked the store error: %s", method, target, rec.Body.String())
		}
	}
}

// TestNewRouter_RequiresTheAdminStores extends the fail-loud startup guard to
// the two stores the admin handlers dereference. Without it, a router wired
// with Auth but no Alerts or Regions accepts the first admin request and
// nil-derefs inside the handler, which net/http recovers per connection: the
// operator sees a reset request some time after deployment instead of a
// startup error with a stack trace.
func TestNewRouter_RequiresTheAdminStores(t *testing.T) {
	t.Parallel()

	base := func() Deps {
		return Deps{
			Alerts:  failingAlerts{},
			Regions: failingRegions{},
			Auth:    newStubAuth(),
			Now:     func() time.Time { return testNow },
			Logger:  discardLogger(),
		}
	}

	tests := []struct {
		name   string
		mutate func(*Deps)
		want   []string
	}{
		{"alerts missing", func(d *Deps) { d.Alerts = nil }, []string{"Deps.Alerts"}},
		{"regions missing", func(d *Deps) { d.Regions = nil }, []string{"Deps.Regions"}},
		{
			"everything missing names everything",
			func(d *Deps) { d.Alerts, d.Regions, d.Now = nil, nil, nil },
			[]string{"Deps.Now", "Deps.Alerts", "Deps.Regions"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				r := recover()
				if r == nil {
					t.Fatal("NewRouter returned normally with a required admin dependency missing")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("panic value = %v (%T), want a string message", r, r)
				}
				assertContains(t, "panic message", msg, tt.want...)
			}()
			deps := base()
			tt.mutate(&deps)
			NewRouter(deps)
		})
	}

	t.Run("a fully wired admin router does not panic", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("NewRouter panicked with every dependency set: %v", r)
			}
		}()
		NewRouter(base())
	})
}

// TestParseInstantJSON is the unit-level guard on the rule the whole timestamp
// contract rests on: a naive datetime is rejected, never guessed at. Guessing
// would place an alert hours from where its author meant it.
func TestParseInstantJSON(t *testing.T) {
	t.Parallel()

	region := regions.Region{ID: 7, Timezone: "America/Los_Angeles"}

	t.Run("an explicit offset is required", func(t *testing.T) {
		for _, in := range []string{
			"2026-08-15T14:00:00",
			"2026-08-15 14:00:00-07:00",
			"2026-08-15",
			"",
			"tomorrow",
		} {
			got, err := parseInstantJSON(in, region)
			if err == nil {
				t.Errorf("parseInstantJSON(%q) = %v, nil; want an error naming the region timezone", in, got)
				continue
			}
			assertContains(t, fmt.Sprintf("parseInstantJSON(%q) error", in), err.Error(),
				"RFC 3339", "explicit offset", "region 7", "America/Los_Angeles")
		}
	})

	t.Run("offsets are normalized to UTC", func(t *testing.T) {
		got, err := parseInstantJSON("2026-08-15T14:00:00-07:00", region)
		if err != nil {
			t.Fatalf("parseInstantJSON: %v", err)
		}
		if want := time.Date(2026, 8, 15, 21, 0, 0, 0, time.UTC); !got.Equal(want) {
			t.Errorf("= %v, want %v", got, want)
		}
		if got.Location() != time.UTC {
			t.Errorf("location = %v, want UTC", got.Location())
		}
	})

	t.Run("Z counts as an explicit offset", func(t *testing.T) {
		if _, err := parseInstantJSON("2026-08-15T21:00:00Z", region); err != nil {
			t.Errorf("parseInstantJSON(Z form): %v", err)
		}
	})

	// A region with no timezone used to render as "region 16 is configured as
	// :" -- a sentence that stops mid-clause and reads like a bug in the error
	// message rather than the fact it is reporting.
	t.Run("an unconfigured timezone reads as a sentence", func(t *testing.T) {
		_, err := parseInstantJSON("2026-08-15T14:00:00", regions.Region{ID: 16})
		if err == nil {
			t.Fatal("parseInstantJSON accepted a naive datetime")
		}
		assertContains(t, "unconfigured-zone error", err.Error(),
			"RFC 3339", "explicit offset", "region 16 has no configured timezone")
		if strings.Contains(err.Error(), "configured as :") {
			t.Errorf("error still has the truncated clause: %v", err)
		}
	})
}
