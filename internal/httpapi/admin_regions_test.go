package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// regionJSONFields are the field names the SPA's region screen (task 11) is
// written against.
var regionJSONFields = []string{
	"id", "name", "oba_base_url", "sidecar_base_url", "language", "active",
	"default_agency_id", "timezone", "latitude", "longitude", "oba_api_key",
}

// regionByID finds one region in a decoded list response.
func regionByID(t *testing.T, list []map[string]any, id int64) map[string]any {
	t.Helper()
	for _, m := range list {
		if int64(m["id"].(float64)) == id {
			return m
		}
	}
	t.Fatalf("region %d not in %v", id, list)
	return nil
}

// TestAdminRegions_List pins the list shape, including the two locally-managed
// fields that exist nowhere in the directory document and are the only reason
// this endpoint is editable at all.
func TestAdminRegions_List(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	got := array(t, f.do(http.MethodGet, "/api/admin/v1/regions", ""), http.StatusOK)

	if len(got) != 3 {
		t.Fatalf("regions = %d, want 3", len(got))
	}
	assertKeys(t, "region", got[0], regionJSONFields)

	// Sorted by id, and region 0 really is in the list: an implementation that
	// treated 0 as "no region" would drop Tampa Bay entirely.
	for i, want := range []int64{regionTampa, regionPuget, regionBare} {
		if id := int64(got[i]["id"].(float64)); id != want {
			t.Errorf("regions[%d].id = %d, want %d (ascending by id)", i, id, want)
		}
	}

	tampa := regionByID(t, got, regionTampa)
	if v := str(t, tampa, "name"); v != "Tampa Bay" {
		t.Errorf("name = %q, want Tampa Bay", v)
	}
	if v := str(t, tampa, "oba_base_url"); v != "https://tampa.example/" {
		t.Errorf("oba_base_url = %q", v)
	}
	if v := str(t, tampa, "sidecar_base_url"); v != "https://sidecar.example/tampa" {
		t.Errorf("sidecar_base_url = %q", v)
	}
	if v := str(t, tampa, "language"); v != "en" {
		t.Errorf("language = %q", v)
	}
	if !boolean(t, tampa, "active") {
		t.Error("active = false, want true")
	}
	if v := str(t, tampa, "default_agency_id"); v != "HART" {
		t.Errorf("default_agency_id = %q, want HART", v)
	}
	if v := str(t, tampa, "timezone"); v != "America/New_York" {
		t.Errorf("timezone = %q, want America/New_York", v)
	}

	bare := regionByID(t, got, regionBare)
	if boolean(t, bare, "active") {
		t.Error("region 2 active = true, want false")
	}
	if v := str(t, bare, "default_agency_id"); v != "" {
		t.Errorf("region 2 default_agency_id = %q, want empty", v)
	}
	// Never configured, so it still holds the schema's default rather than an
	// empty string: the column is NOT NULL DEFAULT 'UTC'.
	if v := str(t, bare, "timezone"); v != "UTC" {
		t.Errorf("region 2 timezone = %q, want the schema default UTC", v)
	}
}

// emptyRegions is a region store with nothing in it: what a fresh database
// looks like before the first directory refresh.
type emptyRegions struct{ failingRegions }

func (emptyRegions) List(context.Context) ([]regions.Region, error) { return nil, nil }

// TestAdminRegions_ListEmptyIsAnArray: the store returns a nil slice for an
// empty table, and a nil slice marshals to null. `null.map(...)` is the same
// SPA crash the alerts list has its own test for, and the region screen is the
// first thing an operator opens on a brand-new deployment -- exactly when the
// table is empty.
func TestAdminRegions_ListEmptyIsAnArray(t *testing.T) {
	t.Parallel()

	repo := newStubAuth()
	repo.addUser("admin", testHash())
	h := NewRouter(Deps{
		Alerts:  failingAlerts{},
		Regions: emptyRegions{},
		Auth:    repo,
		Now:     func() time.Time { return testNow },
		Logger:  discardLogger(),
		Sleep:   func(time.Duration) {},
	})

	rec := sendTo(h, http.MethodGet, "/api/admin/v1/regions", "", adminLogin(t, h))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if body := bodyText(rec); body != "[]" {
		t.Errorf("body = %q, want %q", body, "[]")
	}
}

// TestAdminRegions_PatchSetsOnlyWhatWasSent: both fields are pointers, so an
// omitted one must survive the write. The stored row is written whole, so an
// implementation that forgot to merge would blank the field it did not touch.
func TestAdminRegions_PatchSetsOnlyWhatWasSent(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	t.Run("agency only", func(t *testing.T) {
		got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"default_agency_id":"40"}`), http.StatusOK)
		assertKeys(t, "region", got, regionJSONFields)
		if v := str(t, got, "default_agency_id"); v != "40" {
			t.Errorf("default_agency_id = %q, want 40", v)
		}
		if v := str(t, got, "timezone"); v != "America/Los_Angeles" {
			t.Errorf("timezone = %q, want the untouched America/Los_Angeles", v)
		}
		stored, err := f.store.Regions().Get(context.Background(), regionPuget)
		if err != nil {
			t.Fatalf("stored region: %v", err)
		}
		if stored.DefaultAgencyID != "40" || stored.Timezone != "America/Los_Angeles" {
			t.Errorf("stored = %+v, want agency 40 and the original timezone", stored)
		}
	})

	t.Run("timezone only", func(t *testing.T) {
		got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"timezone":"America/Denver"}`), http.StatusOK)
		if v := str(t, got, "timezone"); v != "America/Denver" {
			t.Errorf("timezone = %q, want America/Denver", v)
		}
		if v := str(t, got, "default_agency_id"); v != "40" {
			t.Errorf("default_agency_id = %q, want the untouched 40", v)
		}
	})

	t.Run("both", func(t *testing.T) {
		got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/1",
			`{"default_agency_id":"1","timezone":"America/Los_Angeles"}`), http.StatusOK)
		if v := str(t, got, "default_agency_id"); v != "1" {
			t.Errorf("default_agency_id = %q, want 1", v)
		}
		if v := str(t, got, "timezone"); v != "America/Los_Angeles" {
			t.Errorf("timezone = %q", v)
		}
	})

	t.Run("region 0 is patchable", func(t *testing.T) {
		got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/0", `{"default_agency_id":"HART-2"}`), http.StatusOK)
		if got["id"] != float64(0) {
			t.Errorf("id = %v, want 0", got["id"])
		}
		if v := str(t, got, "default_agency_id"); v != "HART-2" {
			t.Errorf("default_agency_id = %q, want HART-2", v)
		}
	})

	t.Run("clearing the default agency id is allowed", func(t *testing.T) {
		got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/2", `{"default_agency_id":""}`), http.StatusOK)
		if v := str(t, got, "default_agency_id"); v != "" {
			t.Errorf("default_agency_id = %q, want empty", v)
		}
	})
}

// TestAdminRegions_PatchRejections: a bad timezone has to be caught here, at
// the point of the mistake. Stored unvalidated, it would resurface as silently
// UTC-rendered times in the CLI and the SPA.
func TestAdminRegions_PatchRejections(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	tests := []struct {
		name        string
		path        string
		body        string
		wantStatus  int
		wantInError []string
	}{
		{"no fields", "/api/admin/v1/regions/1", `{}`, http.StatusBadRequest, []string{"default_agency_id", "timezone"}},
		{"null fields", "/api/admin/v1/regions/1", `{"default_agency_id":null,"timezone":null,"oba_api_key":null}`, http.StatusBadRequest, []string{"timezone"}},
		// Both of these are the region scope's single 404, not the handler's
		// own error: an unknown region and an unparseable segment must be
		// indistinguishable, or the status code alone tells a region key
		// which region ids exist (design spec section 2.5).
		{"unknown region", "/api/admin/v1/regions/999", `{"timezone":"UTC"}`, http.StatusNotFound, []string{"region not found"}},
		{"non-integer id", "/api/admin/v1/regions/abc", `{"timezone":"UTC"}`, http.StatusNotFound, []string{"region not found"}},
		{"invalid timezone", "/api/admin/v1/regions/1", `{"timezone":"Mars/Olympus_Mons"}`, http.StatusBadRequest, []string{"timezone", "Mars/Olympus_Mons"}},
		{"empty timezone", "/api/admin/v1/regions/1", `{"timezone":""}`, http.StatusBadRequest, []string{"timezone"}},
		{
			// time.LoadLocation accepts "Local" and resolves it to whatever
			// zone the server happens to run in -- exactly the machine-local
			// dependence this design bans, and invisible once stored.
			"machine-local timezone", "/api/admin/v1/regions/1", `{"timezone":"Local"}`,
			http.StatusBadRequest, []string{"Local", "machine-dependent"},
		},
		{"malformed JSON", "/api/admin/v1/regions/1", `{"timezone":`, http.StatusBadRequest, []string{"invalid JSON"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertContains(t, "error", errorText(t, f.do(http.MethodPatch, tt.path, tt.body), tt.wantStatus), tt.wantInError...)
		})
	}

	// Region 1 must still be exactly as the fixture configured it.
	stored, err := f.store.Regions().Get(context.Background(), regionPuget)
	if err != nil {
		t.Fatalf("stored region: %v", err)
	}
	if stored.DefaultAgencyID != "1" || stored.Timezone != "America/Los_Angeles" {
		t.Errorf("rejected patches mutated region 1: %+v", stored)
	}
}

// TestAdminRegions_PatchThenCreateAlert closes the loop the "no default agency
// id" error promises: the 400 tells the author to PATCH the region, and doing
// exactly that must make the create succeed.
func TestAdminRegions_PatchThenCreateAlert(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/2/alerts", minimalAlertBody("blocked"))
	assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "PATCH /api/admin/v1/regions/2")

	object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/2", `{"default_agency_id":"bare-agency"}`), http.StatusOK)

	got := f.createAlert(t, regionBare, minimalAlertBody("unblocked"))
	if v := str(t, got, "agency_id"); v != "bare-agency" {
		t.Errorf("agency_id = %q, want bare-agency", v)
	}
}

// TestAdminRegions_TimezoneNamedInTimestampErrors: the whole point of storing a
// region timezone is that the naive-datetime 400 can name it. Changing the
// region must change the message.
func TestAdminRegions_TimezoneNamedInTimestampErrors(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"timezone":"Asia/Kathmandu"}`), http.StatusOK)

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", `{"header":"x","start_time":"2026-08-15T14:00:00"}`)
	assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "Asia/Kathmandu")
}

// The key must never leave the server. Asserting against the raw response
// bytes rather than a decoded struct means a field added later without a tag
// change still fails this test.
func TestAdminRegions_NeverEchoesTheKey(t *testing.T) {
	t.Parallel()

	const secret = "SENTINEL-OBA-KEY-do-not-echo"
	f := newAdminFixture(t)
	if err := f.store.Regions().SetLocalFields(context.Background(), regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: secret,
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodGet, "/api/admin/v1/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
		t.Fatalf("region listing leaks the API key: %s", rec.Body.String())
	}
}

// A plain boolean would report false for a region whose vehicle search works
// perfectly via the process default -- the reading an operator would act on
// wrongly. Three states, three distinguishable words.
func TestAdminRegions_KeyStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		regionKey     string
		defaultKeySet bool
		want          string
	}{
		{"region carries its own", "abc", false, "region"},
		{"region carries its own even with a default", "abc", true, "region"},
		{"inherits the process default", "", true, "default"},
		{"nothing configured anywhere", "", false, "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := sqlitetest.Open(t)
			ctx := context.Background()
			if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{
				{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true},
			}, testNow); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
				DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: tt.regionKey,
			}, testNow); err != nil {
				t.Fatalf("set key: %v", err)
			}
			if _, err := store.Auth().CreateUser(ctx, "admin", testHash(), testNow); err != nil {
				t.Fatalf("create user: %v", err)
			}

			handler := NewRouter(Deps{
				Alerts: store.Alerts(), Regions: store.Regions(), Auth: store.Auth(),
				Now: func() time.Time { return testNow }, Logger: discardLogger(),
				Sleep: func(time.Duration) {}, OBADefaultKeySet: tt.defaultKeySet,
			})
			f := &adminFixture{handler: handler, store: store, cookie: adminLogin(t, handler)}

			got := array(t, f.do(http.MethodGet, "/api/admin/v1/regions", ""), http.StatusOK)
			region := regionByID(t, got, regionPuget)
			if v := str(t, region, "oba_api_key"); v != tt.want {
				t.Errorf("oba_api_key = %q, want %q", v, tt.want)
			}
		})
	}
}

// 0,0 is a real coordinate in the Gulf of Guinea, so an unsynced region must
// serialize as null rather than as a point off the coast of Africa.
func TestAdminRegions_CentroidIsNullWhenUnsynced(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	if err := f.store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
	}, testNow); err != nil {
		t.Fatalf("seed centroid: %v", err)
	}

	got := array(t, f.do(http.MethodGet, "/api/admin/v1/regions", ""), http.StatusOK)

	bare := regionByID(t, got, regionBare)
	if bare["latitude"] != nil {
		t.Errorf("unsynced latitude = %v, want null", bare["latitude"])
	}
	if bare["longitude"] != nil {
		t.Errorf("unsynced longitude = %v, want null", bare["longitude"])
	}

	puget := regionByID(t, got, regionPuget)
	if puget["latitude"] != 47.75 {
		t.Errorf("latitude = %v, want 47.75", puget["latitude"])
	}
	if puget["longitude"] != -122.49 {
		t.Errorf("longitude = %v, want -122.49", puget["longitude"])
	}
}

// The PATCH response is rendered through the same toRegionJSON as the list,
// but nothing exercised that call site directly: TestAdminRegions_KeyStatus
// only drives GET, so a PATCH handler that forgot to pass
// h.deps.OBADefaultKeySet through (or hard-coded it) would still pass every
// other test in this file.
func TestAdminRegions_PatchResponseReportsKeyStatus(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	ctx := context.Background()
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{
		{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true},
	}, testNow); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles",
	}, testNow); err != nil {
		t.Fatalf("set fields: %v", err)
	}
	if _, err := store.Auth().CreateUser(ctx, "admin", testHash(), testNow); err != nil {
		t.Fatalf("create user: %v", err)
	}

	handler := NewRouter(Deps{
		Alerts: store.Alerts(), Regions: store.Regions(), Auth: store.Auth(),
		Now: func() time.Time { return testNow }, Logger: discardLogger(),
		Sleep: func(time.Duration) {}, OBADefaultKeySet: true,
	})
	f := &adminFixture{handler: handler, store: store, cookie: adminLogin(t, handler)}

	got := object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"default_agency_id":"99"}`), http.StatusOK)
	if v := str(t, got, "oba_api_key"); v != "default" {
		t.Errorf("PATCH response oba_api_key = %q, want %q", v, "default")
	}
}

// Omission means unchanged. Anything else silently wipes the key on every
// unrelated edit an operator makes.
func TestAdminRegions_PatchOmittedKeyLeavesItIntact(t *testing.T) {
	t.Parallel()

	const secret = "keep-me"
	f := newAdminFixture(t)
	ctx := context.Background()
	if err := f.store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: secret,
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"default_agency_id":"99"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The PATCH response is the single most likely surface to carry a fresh
	// secret into devtools, since it is literally the response to submitting
	// one -- check the raw bytes, not just the stored value.
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
		t.Fatalf("PATCH response leaks the API key: %s", rec.Body.String())
	}

	got, err := f.store.Regions().Get(ctx, regionPuget)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != secret {
		t.Errorf("OBAAPIKey = %q, want %q -- an unrelated PATCH wiped the key", got.OBAAPIKey, secret)
	}
	if got.DefaultAgencyID != "99" {
		t.Errorf("DefaultAgencyID = %q, want 99", got.DefaultAgencyID)
	}
}

func TestAdminRegions_PatchEmptyKeyClearsIt(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	ctx := context.Background()
	if err := f.store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: "clear-me",
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"oba_api_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, err := f.store.Regions().Get(ctx, regionPuget)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != "" {
		t.Errorf("OBAAPIKey = %q, want empty", got.OBAAPIKey)
	}
}

// The guard's message is the kind that goes stale for a year. Pin it.
func TestAdminRegions_PatchEmptyBodyNamesAllThreeFields(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "oba_api_key") {
		t.Errorf("error message does not mention oba_api_key: %s", rec.Body.String())
	}
}
