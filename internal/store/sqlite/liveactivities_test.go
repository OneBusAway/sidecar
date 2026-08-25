package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// TestListWithCorruptContentStateTreatsRowAsEmpty pins fromRow's read-side
// guard (final-review finding 1a): the conformance suite cannot write a
// corrupt last_content_state through the Repository, so this test reaches
// into the database directly, following the surveys_test.go precedent
// (TestGetSurveyCorruptContentFails) for raw-SQL corruption of a generated
// column. Two ways a cell can be unusable are both covered: content that
// fails to parse at all, and content that parses but decodes to a null
// "arrivals" list -- json.Unmarshal leaves a nil slice for the latter, which
// is exactly what a naive `err != nil` guard alone would miss.
func TestListWithCorruptContentStateTreatsRowAsEmpty(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err = store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err = store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Region", OBABaseURL: "https://example.org/", Active: true,
	}}, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}
	repo := store.LiveActivities()

	newActivity := func(activityID string) liveactivities.NewLiveActivity {
		return liveactivities.NewLiveActivity{
			RegionID: 1, Token: "tok-" + activityID, ExpiresAt: now.Add(8 * time.Hour),
			ActivityID: activityID, PushToken: "push-" + activityID,
			StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
		}
	}

	// intact: a row with a real, non-empty content state, to prove other
	// rows survive List untouched alongside the corrupt ones.
	intact, err := repo.Upsert(ctx, newActivity("intact"), now)
	if err != nil {
		t.Fatalf("Upsert intact: %v", err)
	}
	wantState := liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{
		{DepartureTime: 100, ScheduleStatus: "on_time"},
	}}
	if err = repo.RecordPush(ctx, intact.ID, wantState, now); err != nil {
		t.Fatalf("RecordPush intact: %v", err)
	}

	badParse, err := repo.Upsert(ctx, newActivity("bad-parse"), now)
	if err != nil {
		t.Fatalf("Upsert bad-parse: %v", err)
	}
	nullArrivals, err := repo.Upsert(ctx, newActivity("null-arrivals"), now)
	if err != nil {
		t.Fatalf("Upsert null-arrivals: %v", err)
	}

	corrupt := func(id int64, content string) {
		t.Helper()
		if _, execErr := store.db.ExecContext(ctx,
			`UPDATE live_activities SET last_content_state = ? WHERE id = ?`, content, id); execErr != nil {
			t.Fatalf("corrupt row %d: %v", id, execErr)
		}
	}
	corrupt(badParse.ID, "not json")
	corrupt(nullArrivals.ID, `{"arrivals":null}`)

	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(rows))
	}
	byID := make(map[int64]liveactivities.LiveActivity, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}

	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"not json", badParse.ID},
		{`{"arrivals":null}`, nullArrivals.ID},
	} {
		row, ok := byID[tc.id]
		if !ok {
			t.Fatalf("%s: row %d missing from List", tc.name, tc.id)
		}
		if row.LastContentState.Arrivals == nil {
			t.Errorf("%s: LastContentState.Arrivals = nil, want non-nil empty slice", tc.name)
		}
		if len(row.LastContentState.Arrivals) != 0 {
			t.Errorf("%s: LastContentState.Arrivals = %+v, want empty", tc.name, row.LastContentState.Arrivals)
		}
	}

	got, ok := byID[intact.ID]
	if !ok {
		t.Fatal("intact row missing from List")
	}
	if len(got.LastContentState.Arrivals) != 1 || got.LastContentState.Arrivals[0] != wantState.Arrivals[0] {
		t.Errorf("intact row's content state was disturbed by an unrelated row's corruption: %+v", got.LastContentState)
	}
}

// TestMapInsertErrTranslatesUniqueViolation pins the Upsert -> ErrDuplicate
// mapping (final-review finding 1b) against a real driver error rather than
// a hand-built one: two InsertLiveActivity calls for the same
// (region_id, activity_id) race exactly like two concurrent first
// registrations (see Upsert's doc comment), and the second must violate
// live_activities_activity_idx. mapInsertErr is the adapter's own mapping,
// exercised directly so this test fails if either the SQLite error text or
// the mapping's string match ever drifts apart.
func TestMapInsertErrTranslatesUniqueViolation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Region", OBABaseURL: "https://example.org/", Active: true,
	}}, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	params := func(token string) gen.InsertLiveActivityParams {
		return gen.InsertLiveActivityParams{
			RegionID: 1, Token: token, ActivityID: "act-1", PushToken: "push-" + token,
			StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
			ExpiresAt: now.Add(8 * time.Hour).Unix(), Now: now.Unix(),
		}
	}
	if _, err := store.q.InsertLiveActivity(ctx, params("tok-a")); err != nil {
		t.Fatalf("first InsertLiveActivity: %v", err)
	}
	_, dupErr := store.q.InsertLiveActivity(ctx, params("tok-b"))
	if dupErr == nil {
		t.Fatal("second InsertLiveActivity with the same (region_id, activity_id) succeeded, want a UNIQUE violation")
	}

	mapped := mapInsertErr(dupErr, 1)
	if !errors.Is(mapped, liveactivities.ErrDuplicate) {
		t.Fatalf("mapInsertErr(%v) = %v, want errors.Is(_, ErrDuplicate)", dupErr, mapped)
	}

	// An unrelated driver error must pass through unmapped: referencing a
	// region that does not exist violates the FK constraint, not the unique
	// index, and must not be reported as ErrDuplicate.
	_, fkErr := store.q.InsertLiveActivity(ctx, gen.InsertLiveActivityParams{
		RegionID: 999, Token: "tok-c", ActivityID: "act-2", PushToken: "push-c",
		StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
		ExpiresAt: now.Add(8 * time.Hour).Unix(), Now: now.Unix(),
	})
	if fkErr == nil {
		t.Fatal("InsertLiveActivity with an unknown region_id succeeded, want a foreign key violation")
	}
	if mapped := mapInsertErr(fkErr, 999); errors.Is(mapped, liveactivities.ErrDuplicate) {
		t.Fatalf("mapInsertErr(%v) = %v, want it NOT mapped to ErrDuplicate", fkErr, mapped)
	}
}
