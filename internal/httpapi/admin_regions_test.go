package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// regionJSONFields are the field names the SPA's region screen (task 11) is
// written against.
var regionJSONFields = []string{
	"id", "name", "oba_base_url", "sidecar_base_url", "language", "active",
	"default_agency_id", "timezone",
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
		{"null fields", "/api/admin/v1/regions/1", `{"default_agency_id":null,"timezone":null}`, http.StatusBadRequest, []string{"timezone"}},
		{"unknown region", "/api/admin/v1/regions/999", `{"timezone":"UTC"}`, http.StatusNotFound, []string{"region", "999"}},
		{"non-integer id", "/api/admin/v1/regions/abc", `{"timezone":"UTC"}`, http.StatusBadRequest, []string{"id"}},
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

	rec := f.do(http.MethodPost, "/api/admin/v1/alerts", minimalAlert(regionBare, "blocked"))
	assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "PATCH /api/admin/v1/regions/2")

	object(t, f.do(http.MethodPatch, "/api/admin/v1/regions/2", `{"default_agency_id":"bare-agency"}`), http.StatusOK)

	got := f.createAlert(t, minimalAlert(regionBare, "unblocked"))
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

	rec := f.do(http.MethodPost, "/api/admin/v1/alerts", `{"region_id":1,"header":"x","start_time":"2026-08-15T14:00:00"}`)
	assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "Asia/Kathmandu")
}
