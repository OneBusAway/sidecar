package regions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// base is the fixed instant every subtest builds its timestamps from. Using
// time.Now here would violate the forbidigo ban and make failures
// non-reproducible.
var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func clockAt(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func testOptions() regions.ClientOptions {
	opts := regions.DefaultClientOptions()
	opts.Timeout = 5 * time.Second
	return opts
}

// newRegionStore opens a fresh migrated SQLite store for a test.
func newRegionStore(t *testing.T) regions.Repository {
	t.Helper()
	return sqlitetest.Open(t).Regions()
}

func TestFetch_RealFixture(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.FileServer(http.Dir("testdata")))
	defer srv.Close()

	client := regions.NewClient(srv.URL+"/regions-v3.json", testOptions())
	got, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Fetch returned no regions, want at least one")
	}

	var zero *regions.Region
	for i := range got {
		if got[i].ID == 0 {
			zero = &got[i]
		}
	}
	if zero == nil {
		t.Fatal("region id 0 not present in parsed fixture")
	}
	if zero.Name != "Tampa Bay" {
		t.Errorf("region 0 Name = %q, want %q", zero.Name, "Tampa Bay")
	}
	if !zero.Active {
		t.Error("region 0 Active = false, want true")
	}
}

func TestFetch_RejectsOversizedBody(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.MaxBytes = 1024

	// The body is well-formed JSON that genuinely exceeds MaxBytes (a single
	// entry padded with a long regionName). If it merely failed to parse,
	// deleting the size cap would still make this test pass -- json.Unmarshal
	// would reject it on its own, decoupling the assertion from the cap it's
	// meant to exercise. Padding well past MaxBytes*2 also keeps the test
	// robust to the +1-byte boundary the cap enforces at.
	pad := strings.Repeat("x", int(opts.MaxBytes)*2)
	body := fmt.Sprintf(`{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":1,"regionName":%q,"obaBaseUrl":"https://example.org/","active":true}
	]}}`, pad)
	if int64(len(body)) <= opts.MaxBytes {
		t.Fatalf("test body is %d bytes, want > MaxBytes (%d)", len(body), opts.MaxBytes)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, opts)
	_, err := client.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch: want error for oversized body, got nil")
	}
	if !errors.Is(err, regions.ErrResponseTooLarge) {
		t.Fatalf("Fetch error = %v, want errors.Is(err, regions.ErrResponseTooLarge) -- a body this large that fails for some other reason (e.g. JSON parse) would not prove the size cap fired", err)
	}
}

func TestFetch_RejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.MaxEntries = 10

	type entry struct {
		ID         int64  `json:"id"`
		RegionName string `json:"regionName"`
		OBABaseURL string `json:"obaBaseUrl"`
		Active     bool   `json:"active"`
	}
	list := make([]entry, opts.MaxEntries+1)
	for i := range list {
		list[i] = entry{ID: int64(i), RegionName: fmt.Sprintf("Region %d", i), OBABaseURL: "https://example.org/", Active: true}
	}
	doc := map[string]any{
		"version": 3, "code": 200, "text": "OK",
		"data": map[string]any{"list": list},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(doc)
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, opts)
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch: want error for too many entries, got nil")
	}
}

func TestFetch_SkipsInvalidEntriesKeepsValid(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.MaxFieldLen = 100

	overlongName := strings.Repeat("x", opts.MaxFieldLen+1)

	body := fmt.Sprintf(`{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":-1,"regionName":"Negative","obaBaseUrl":"https://example.org/","active":true},
		{"id":10,"regionName":"First Ten","obaBaseUrl":"https://example.org/ten1/","active":true},
		{"id":10,"regionName":"Second Ten","obaBaseUrl":"https://example.org/ten2/","active":true},
		{"id":11,"regionName":%q,"obaBaseUrl":"https://example.org/","active":true},
		{"id":20,"regionName":"Good Twenty","obaBaseUrl":"https://example.org/twenty/","active":true}
	]}}`, overlongName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, opts)
	got, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("Fetch returned %d regions, want 2: %+v", len(got), got)
	}
	byID := make(map[int64]regions.Region, len(got))
	for _, r := range got {
		byID[r.ID] = r
	}
	first, ok := byID[10]
	if !ok {
		t.Fatal("region id 10 (first occurrence) missing, want kept")
	}
	if first.Name != "First Ten" {
		t.Errorf("region 10 Name = %q, want %q (first occurrence wins, second is a skipped duplicate)", first.Name, "First Ten")
	}
	if _, ok := byID[20]; !ok {
		t.Fatal("region id 20 missing, want kept")
	}
	if _, ok := byID[-1]; ok {
		t.Error("region id -1 present, want skipped (negative id)")
	}
	if _, ok := byID[11]; ok {
		t.Error("region id 11 present, want skipped (regionName exceeds MaxFieldLen)")
	}
}

func TestFetch_HonoursTimeout(t *testing.T) {
	t.Parallel()

	opts := testOptions()
	opts.Timeout = 50 * time.Millisecond

	sleepUnblocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		close(sleepUnblocked)
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, opts)

	start := time.Now()
	_, err := client.Fetch(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Fetch: want error when handler exceeds Timeout, got nil")
	}
	// The handler sleeps up to 2s; asserting well under that (and close to
	// the 50ms Timeout) catches a client that ignores Timeout and simply
	// waits for the handler to finish, which a bare "err != nil" check would
	// miss if the handler eventually returned some other error.
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Fetch took %v, want well under the handler's 2s sleep (Timeout = %v)", elapsed, opts.Timeout)
	}
}

func TestSync_PreservesLocalFields(t *testing.T) {
	t.Parallel()

	repo := newRegionStore(t)
	ctx := context.Background()

	if err := repo.UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Old Name", OBABaseURL: "https://old.example.org/", Active: true,
	}}, base); err != nil {
		t.Fatalf("seed UpsertFromDirectory: %v", err)
	}
	if err := repo.SetLocalFields(ctx, 1, regions.LocalFields{
		DefaultAgencyID: "40", Timezone: "America/Los_Angeles",
	}, base); err != nil {
		t.Fatalf("SetLocalFields: %v", err)
	}

	body := `{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":1,"regionName":"New Name","obaBaseUrl":"https://new.example.org/","active":false}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, testOptions())
	if err := regions.Sync(ctx, client, repo, clockAt(base.Add(time.Hour))); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	got, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "New Name" {
		t.Errorf("Name = %q, want %q (directory field should update)", got.Name, "New Name")
	}
	if got.OBABaseURL != "https://new.example.org/" {
		t.Errorf("OBABaseURL = %q, want %q", got.OBABaseURL, "https://new.example.org/")
	}
	if got.Active {
		t.Error("Active = true, want false (directory field should update)")
	}
	if got.DefaultAgencyID != "40" {
		t.Errorf("DefaultAgencyID = %q, want %q (local field must survive Sync)", got.DefaultAgencyID, "40")
	}
	if got.Timezone != "America/Los_Angeles" {
		t.Errorf("Timezone = %q, want %q (local field must survive Sync)", got.Timezone, "America/Los_Angeles")
	}
}

func TestSync_NeverRemovesRegions(t *testing.T) {
	t.Parallel()

	repo := newRegionStore(t)
	ctx := context.Background()

	bodyBoth := `{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":1,"regionName":"Region 1","obaBaseUrl":"https://example.org/1/","active":true},
		{"id":2,"regionName":"Region 2","obaBaseUrl":"https://example.org/2/","active":true}
	]}}`
	bodyOne := `{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":1,"regionName":"Region 1","obaBaseUrl":"https://example.org/1/","active":true}
	]}}`

	var current string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(current))
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, testOptions())

	current = bodyBoth
	if err := regions.Sync(ctx, client, repo, clockAt(base)); err != nil {
		t.Fatalf("Sync(both): %v", err)
	}

	current = bodyOne
	if err := regions.Sync(ctx, client, repo, clockAt(base.Add(time.Hour))); err != nil {
		t.Fatalf("Sync(one): %v", err)
	}

	if _, err := repo.Get(ctx, 2); err != nil {
		t.Fatalf("Get(region 2) after it vanished upstream: %v, want no error (Sync must never delete)", err)
	}
}

// TestRunSyncLoop_NonPositiveIntervalDoesNotPanic reproduces the finding
// that RunSyncLoop passed interval straight to time.NewTicker, which panics
// on a duration <= 0. cmd/sidecar now rejects --refresh=0 before it ever
// reaches here, but RunSyncLoop is exported and runs on a goroutine with no
// recover, so a bad value from any other caller (a test, a future caller)
// must not be able to take the whole process down. This asserts both halves:
// no panic, and the initial synchronous Sync still ran before the (guarded)
// ticker was set up.
func TestRunSyncLoop_NonPositiveIntervalDoesNotPanic(t *testing.T) {
	t.Parallel()

	repo := newRegionStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	body := `{"version":3,"code":200,"text":"OK","data":{"list":[
		{"id":1,"regionName":"Region 1","obaBaseUrl":"https://example.org/","active":true}
	]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := regions.NewClient(srv.URL, testOptions())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for _, interval := range []time.Duration{0, -time.Hour} {
			regions.RunSyncLoop(ctx, client, repo, interval, clockAt(base), logger)
		}
	}()
	<-done

	if _, err := repo.Get(context.Background(), 1); err != nil {
		t.Fatalf("Get(1) after RunSyncLoop's initial run: %v, want no error", err)
	}
}

func TestSync_FailedFetchLeavesRowsUntouched(t *testing.T) {
	t.Parallel()

	repo := newRegionStore(t)
	ctx := context.Background()

	if err := repo.UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Stable Region", OBABaseURL: "https://example.org/", Active: true,
	}}, base); err != nil {
		t.Fatalf("seed UpsertFromDirectory: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before any request: every Fetch attempt fails

	client := regions.NewClient(srv.URL, testOptions())
	if err := regions.Sync(ctx, client, repo, clockAt(base.Add(time.Hour))); err == nil {
		t.Fatal("Sync: want error when the fetch fails, got nil")
	}

	got, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get after failed Sync: %v", err)
	}
	if got.Name != "Stable Region" {
		t.Errorf("Name = %q after failed Sync, want unchanged %q", got.Name, "Stable Region")
	}
}

// A region whose bounds are unusable must still be kept. Dropping the entry
// would take its alerts feed down too, which is far worse than a missing
// weather card.
func TestFetchKeepsRegionWithUnusableBounds(t *testing.T) {
	body := `{"data":{"list":[
		{"id":1,"regionName":"No Bounds","obaBaseUrl":"https://example.org/","active":true},
		{"id":2,"regionName":"Good","obaBaseUrl":"https://example.org/","active":true,
		 "bounds":[{"lat":47.6,"lon":-122.3,"latSpan":0.2,"lonSpan":0.2}]}
	]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	got, err := regions.NewClient(srv.URL, testOptions()).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d regions, want 2 (a bad centroid must not drop the region)", len(got))
	}
	if got[0].Centroid != nil {
		t.Errorf("region 1 Centroid = %+v, want nil", got[0].Centroid)
	}
	if got[1].Centroid == nil {
		t.Fatal("region 2 Centroid = nil, want a point")
	}
	// A single bound's centroid is its own center in exact arithmetic, but
	// computeCentroid gets there via multiply-then-divide (lat*w/w), which
	// float64 does not guarantee round-trips bit-exactly -- e.g. -122.3
	// comes back as -122.30000000000001. An exact equality check here would
	// be asserting a coincidence of this particular input, not the property
	// under test, so this compares within a tolerance instead.
	const epsilon = 1e-9
	if diff := got[1].Centroid.Lat - 47.6; diff > epsilon || diff < -epsilon {
		t.Errorf("region 2 Lat = %v, want 47.6", got[1].Centroid.Lat)
	}
	if diff := got[1].Centroid.Lon - (-122.3); diff > epsilon || diff < -epsilon {
		t.Errorf("region 2 Lon = %v, want -122.3", got[1].Centroid.Lon)
	}
}
