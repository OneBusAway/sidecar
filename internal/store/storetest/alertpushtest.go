package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

type newAlertPushStoreFunc func(*testing.T) (alertpush.Repository, alerts.Repository, regions.Repository)

// RunAlertPushRepository exercises an alertpush.Repository against the
// contract the dispatcher, admin API, and feedback webhook depend on.
func RunAlertPushRepository(t *testing.T, newStore newAlertPushStoreFunc) {
	t.Helper()
	t.Run("CreateGetRoundTrip", func(t *testing.T) { testAlertPushCreateGetRoundTrip(t, newStore) })
	t.Run("CreateRejectsInFlightDuplicate", func(t *testing.T) { testAlertPushCreateRejectsInFlight(t, newStore) })
	t.Run("ListByAlertNewestFirst", func(t *testing.T) { testAlertPushListByAlert(t, newStore) })
	t.Run("ClaimTakesQueuedAndStuckOnly", func(t *testing.T) { testAlertPushClaim(t, newStore) })
	t.Run("AdvanceCursorIsConditional", func(t *testing.T) { testAlertPushAdvanceCursor(t, newStore) })
	t.Run("RecordFailureDedupsAndCounts", func(t *testing.T) { testAlertPushRecordFailure(t, newStore) })
	t.Run("RecordAttemptAndMarkCompleted", func(t *testing.T) { testAlertPushAttemptsAndCompletion(t, newStore) })
	t.Run("CancelTransitions", func(t *testing.T) { testAlertPushCancel(t, newStore) })
	t.Run("CascadesWithAlert", func(t *testing.T) { testAlertPushCascade(t, newStore) })
}

// seedPushAlert creates a region and an alert to hang pushes on.
func seedPushAlert(t *testing.T, alertsRepo alerts.Repository, regionsRepo regions.Repository, regionID int64) alerts.Alert {
	t.Helper()
	putStoretestRegion(t, regionsRepo, regionID)
	a, err := alertsRepo.Create(context.Background(), alerts.NewAlert{
		RegionID: regionID, AgencyID: "1", HeaderText: "Route 44 detour",
		DescriptionText: "Buses skip 3rd Ave.", Cause: "CONSTRUCTION", Effect: "DETOUR",
		Severity: "WARNING", StartTime: base,
	}, base)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}
	return a
}

func newPushFor(a alerts.Alert) alertpush.NewPush {
	return alertpush.NewPush{AlertID: a.ID, RegionID: a.RegionID, Audience: alertpush.AudienceAll,
		Messages: alertpush.Messages{"en": {Title: "Route 44 detour", Body: "Buses skip 3rd Ave."}, "es": {Title: "Desvío", Body: "Buses skip 3rd Ave."}}}
}

// idsOf projects a push slice down to its ids, so an ordering assertion can
// report what it actually got.
func idsOf(ps []alertpush.Push) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// createPush creates one push for a and fails the test if the repository
// refuses -- the fixture path, not an assertion.
func createPush(t *testing.T, repo alertpush.Repository, a alerts.Alert, now time.Time) alertpush.Push {
	t.Helper()
	p, err := repo.Create(context.Background(), newPushFor(a), now)
	if err != nil {
		t.Fatalf("Create push for alert %d: %v", a.ID, err)
	}
	return p
}

// getPush reads one push back, failing the test on any error.
func getPush(t *testing.T, repo alertpush.Repository, id int64) alertpush.Push {
	t.Helper()
	p, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%d): %v", id, err)
	}
	return p
}

// claimPushes runs one dispatcher claim, failing the test on any error.
func claimPushes(t *testing.T, repo alertpush.Repository, now, stuckBefore time.Time) []alertpush.Push {
	t.Helper()
	claimed, err := repo.Claim(context.Background(), now, stuckBefore)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return claimed
}

// completePush drives one queued push all the way to sent, which is the only
// way a test frees an alert's in-flight slot without canceling: Claim moves
// it to sending, MarkCompleted stamps the terminal status.
func completePush(t *testing.T, repo alertpush.Repository, id int64) {
	t.Helper()
	claimPushes(t, repo, base, base)
	ok, err := repo.MarkCompleted(context.Background(), id, alertpush.StatusSent, "", base)
	if err != nil {
		t.Fatalf("completePush(%d): MarkCompleted: %v", id, err)
	}
	if !ok {
		t.Fatalf("completePush(%d): MarkCompleted = false, want true", id)
	}
}

func testAlertPushCreateGetRoundTrip(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)

	created := createPush(t, repo, a, base)
	got := getPush(t, repo, created.ID)
	if got.Status != alertpush.StatusQueued || got.Audience != alertpush.AudienceAll || got.AlertID != a.ID || got.RegionID != 1 {
		t.Errorf("round trip = %+v", got)
	}
	if got.Messages["es"].Title != "Desvío" {
		t.Errorf("Messages not round-tripped: %+v", got.Messages)
	}
	if !got.CreatedAt.Equal(base) || !got.UpdatedAt.Equal(base) || got.StartedAt != nil || got.CompletedAt != nil {
		t.Errorf("timestamps = created %v updated %v started %v completed %v", got.CreatedAt, got.UpdatedAt, got.StartedAt, got.CompletedAt)
	}
	if got.BatchCursor != 0 || got.DeviceCount != 0 || got.SubmittedCount != 0 || got.FailedCount != 0 || got.Attempts != 0 || got.LastError != "" {
		t.Errorf("counters on a fresh push = %+v, want all zero", got)
	}
	if len(got.FailureReasons) != 0 {
		t.Errorf("FailureReasons = %v, want empty", got.FailureReasons)
	}
	if _, err := repo.Get(ctx, 999); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Get(999) = %v, want ErrNotFound", err)
	}
}

func testAlertPushCreateRejectsInFlight(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	first := createPush(t, repo, a, base)

	inflight, inflightErr := repo.InFlightForAlert(ctx, a.ID)
	if inflightErr != nil || !inflight {
		t.Fatalf("InFlightForAlert = %v, %v; want true", inflight, inflightErr)
	}
	if _, err := repo.Create(ctx, newPushFor(a), base); !errors.Is(err, alertpush.ErrInFlight) {
		t.Errorf("second Create = %v, want ErrInFlight", err)
	}
	if err := repo.Cancel(ctx, first.ID, base.Add(time.Minute)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	inflight, inflightErr = repo.InFlightForAlert(ctx, a.ID)
	if inflightErr != nil {
		t.Fatalf("InFlightForAlert after cancel: %v", inflightErr)
	}
	if inflight {
		t.Error("InFlightForAlert after cancel = true, want false")
	}
	if _, err := repo.Create(ctx, newPushFor(a), base.Add(2*time.Minute)); err != nil {
		t.Errorf("Create after cancel: %v, want success (terminal pushes do not block)", err)
	}
}

func testAlertPushListByAlert(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	b := seedPushAlert(t, alertsRepo, regionsRepo, 2)

	p1 := createPush(t, repo, a, base)
	completePush(t, repo, p1.ID)
	p2 := createPush(t, repo, a, base.Add(time.Minute))
	createPush(t, repo, b, base)

	if _, err := repo.RecordFailure(ctx, p2.ID, "tok", "Unregistered", base); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	list, listErr := repo.ListByAlert(ctx, a.ID)
	if listErr != nil {
		t.Fatalf("ListByAlert: %v", listErr)
	}
	if len(list) != 2 || list[0].ID != p2.ID || list[1].ID != p1.ID {
		t.Fatalf("ListByAlert ids = %v, want [%d %d] (newest first, alert-scoped)", idsOf(list), p2.ID, p1.ID)
	}
	if len(list[0].FailureReasons) != 1 || list[0].FailureReasons[0].Reason != "Unregistered" {
		t.Errorf("ListByAlert must attach FailureReasons per row: %+v", list[0].FailureReasons)
	}
	if len(list[1].FailureReasons) != 0 {
		t.Errorf("push with no failures = %+v, want empty FailureReasons", list[1].FailureReasons)
	}
	empty, emptyErr := repo.ListByAlert(ctx, 999)
	if emptyErr != nil || len(empty) != 0 {
		t.Errorf("ListByAlert(999) = %v, %v; want empty, nil", idsOf(empty), emptyErr)
	}
}

func testAlertPushClaim(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	b := seedPushAlert(t, alertsRepo, regionsRepo, 2)
	c := seedPushAlert(t, alertsRepo, regionsRepo, 3)

	queued := createPush(t, repo, a, base)
	fresh := createPush(t, repo, b, base)
	stale := createPush(t, repo, c, base)

	// Put all three into sending at updated_at = base, then re-queue one by
	// canceling and recreating so the claim below sees one queued row.
	claimPushes(t, repo, base, base.Add(-time.Hour))
	if err := repo.Cancel(ctx, queued.ID, base); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	queued = createPush(t, repo, a, base.Add(time.Minute))
	// Touch `fresh` so it is not stuck.
	if err := repo.SetDeviceCount(ctx, fresh.ID, 1, base.Add(20*time.Minute)); err != nil {
		t.Fatalf("SetDeviceCount: %v", err)
	}
	// A failure on `stale` gives the FailureReasons assertion below something
	// to find if Claim ever starts attaching the rollup.
	if _, err := repo.RecordFailure(ctx, stale.ID, "tok", "Unregistered", base); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	now := base.Add(21 * time.Minute)
	claimed := claimPushes(t, repo, now, now.Add(-alertpush.StuckAfter))
	got := idsOf(claimed)
	if len(got) != 2 || got[0] != stale.ID || got[1] != queued.ID {
		t.Fatalf("Claim ids = %v, want [%d %d] (queued + stuck, ascending; fresh sending row untouched)", got, stale.ID, queued.ID)
	}
	for _, p := range claimed {
		if p.Status != alertpush.StatusSending || p.StartedAt == nil || !p.UpdatedAt.Equal(now) {
			t.Errorf("claimed %d = status %s started %v updated %v", p.ID, p.Status, p.StartedAt, p.UpdatedAt)
		}
		// Claim is the dispatcher's hot path: it does not pay for the
		// per-row failure rollup that Get and ListByAlert attach.
		if len(p.FailureReasons) != 0 {
			t.Errorf("claimed %d FailureReasons = %v, want empty (Claim does not attach them)", p.ID, p.FailureReasons)
		}
	}
	// stale's original started_at (base) survives a reclaim.
	if reclaimed := getPush(t, repo, stale.ID); reclaimed.StartedAt == nil || !reclaimed.StartedAt.Equal(base) {
		t.Errorf("reclaimed started_at = %v, want %v (preserved)", reclaimed.StartedAt, base)
	}
	// The untouched sending row kept its own updated_at.
	if untouched := getPush(t, repo, fresh.ID); !untouched.UpdatedAt.Equal(base.Add(20 * time.Minute)) {
		t.Errorf("fresh sending row updated_at = %v, want %v (not claimed)", untouched.UpdatedAt, base.Add(20*time.Minute))
	}
	// Nothing left to claim right away.
	if again := claimPushes(t, repo, now, now.Add(-alertpush.StuckAfter)); len(again) != 0 {
		t.Errorf("second Claim = %v, want empty", idsOf(again))
	}
}

func testAlertPushAdvanceCursor(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p := createPush(t, repo, a, base)

	// Not sending yet: refused.
	ok, advanceErr := repo.AdvanceCursor(ctx, p.ID, 0, 10, 5, base)
	if advanceErr != nil || ok {
		t.Fatalf("AdvanceCursor on queued = %v, %v; want false", ok, advanceErr)
	}
	claimPushes(t, repo, base, base)

	ok, advanceErr = repo.AdvanceCursor(ctx, p.ID, 0, 10, 5, base.Add(time.Second))
	if advanceErr != nil || !ok {
		t.Fatalf("AdvanceCursor(0->10) = %v, %v; want true", ok, advanceErr)
	}
	// Wrong previous cursor: refused, nothing changes.
	ok, advanceErr = repo.AdvanceCursor(ctx, p.ID, 0, 20, 5, base.Add(2*time.Second))
	if advanceErr != nil || ok {
		t.Fatalf("AdvanceCursor with stale prev = %v, %v; want false", ok, advanceErr)
	}
	ok, advanceErr = repo.AdvanceCursor(ctx, p.ID, 10, 20, 7, base.Add(3*time.Second))
	if advanceErr != nil || !ok {
		t.Fatalf("AdvanceCursor(10->20) = %v, %v; want true", ok, advanceErr)
	}
	got := getPush(t, repo, p.ID)
	if got.BatchCursor != 20 || got.SubmittedCount != 12 || !got.UpdatedAt.Equal(base.Add(3*time.Second)) {
		t.Errorf("after advances: cursor %d submitted %d updated %v", got.BatchCursor, got.SubmittedCount, got.UpdatedAt)
	}

	// A committed page resets the failure streak (design spec section 2.6).
	if _, err := repo.RecordAttempt(ctx, p.ID, "blip", base.Add(3*time.Second)); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}
	ok, advanceErr = repo.AdvanceCursor(ctx, p.ID, 20, 25, 1, base.Add(4*time.Second))
	if advanceErr != nil || !ok {
		t.Fatalf("AdvanceCursor(20->25) = %v, %v; want true", ok, advanceErr)
	}
	if got = getPush(t, repo, p.ID); got.Attempts != 0 || got.LastError != "" {
		t.Errorf("after progress: attempts %d last_error %q, want 0 and empty", got.Attempts, got.LastError)
	}

	// Canceled mid-send: refused, and the cursor stands.
	if err := repo.Cancel(ctx, p.ID, base.Add(4*time.Second)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	ok, advanceErr = repo.AdvanceCursor(ctx, p.ID, 25, 30, 1, base.Add(5*time.Second))
	if advanceErr != nil || ok {
		t.Errorf("AdvanceCursor after cancel = %v, %v; want false", ok, advanceErr)
	}
	if got = getPush(t, repo, p.ID); got.BatchCursor != 25 {
		t.Errorf("cursor after a refused advance = %d, want 25 (unchanged)", got.BatchCursor)
	}
}

func testAlertPushRecordFailure(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p := createPush(t, repo, a, base)

	for i, c := range []struct {
		token, reason string
		wantNew       bool
	}{
		{"t1", "Unregistered", true},
		{"t1", "Unregistered", false}, // replay
		{"t2", "BadDeviceToken", true},
		{"t3", "Unregistered", true},
	} {
		isNew, failErr := repo.RecordFailure(ctx, p.ID, c.token, c.reason, base.Add(time.Duration(i)*time.Second))
		if failErr != nil || isNew != c.wantNew {
			t.Errorf("RecordFailure #%d = %v, %v; want %v", i, isNew, failErr, c.wantNew)
		}
	}
	got := getPush(t, repo, p.ID)
	if got.FailedCount != 3 {
		t.Errorf("FailedCount = %d, want 3 (replay not double-counted)", got.FailedCount)
	}
	want := []alertpush.FailureReason{{Reason: "Unregistered", Count: 2}, {Reason: "BadDeviceToken", Count: 1}}
	if len(got.FailureReasons) != 2 || got.FailureReasons[0] != want[0] || got.FailureReasons[1] != want[1] {
		t.Errorf("FailureReasons = %v, want %v (by count desc)", got.FailureReasons, want)
	}
	if _, err := repo.RecordFailure(ctx, 999, "t", "x", base); err == nil {
		t.Error("RecordFailure(unknown push) error = nil, want error (FK)")
	}
	// The sqlite adapter test additionally opens the raw database and asserts
	// that alert_push_failures.token_sha256 never holds a plaintext token --
	// see TestAlertPushFailuresStoreOnlyHashes.
}

func testAlertPushAttemptsAndCompletion(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p := createPush(t, repo, a, base)
	claimPushes(t, repo, base, base)

	n, attemptErr := repo.RecordAttempt(ctx, p.ID, "gorush: 502", base.Add(time.Second))
	if attemptErr != nil || n != 1 {
		t.Fatalf("RecordAttempt = %d, %v; want 1", n, attemptErr)
	}
	n, attemptErr = repo.RecordAttempt(ctx, p.ID, "gorush: 503", base.Add(2*time.Second))
	if attemptErr != nil || n != 2 {
		t.Errorf("second RecordAttempt = %d, %v; want 2", n, attemptErr)
	}
	got := getPush(t, repo, p.ID)
	if got.LastError != "gorush: 503" || got.Attempts != 2 || !got.UpdatedAt.Equal(base.Add(2*time.Second)) {
		t.Errorf("after attempts: %+v", got)
	}

	ok, completeErr := repo.MarkCompleted(ctx, p.ID, alertpush.StatusSent, "", base.Add(3*time.Second))
	if completeErr != nil || !ok {
		t.Fatalf("MarkCompleted = %v, %v; want true", ok, completeErr)
	}
	got = getPush(t, repo, p.ID)
	if got.Status != alertpush.StatusSent || got.CompletedAt == nil || !got.CompletedAt.Equal(base.Add(3*time.Second)) || got.LastError != "" {
		t.Errorf("after MarkCompleted: %+v", got)
	}
	// Terminal rows are never re-completed.
	ok, completeErr = repo.MarkCompleted(ctx, p.ID, alertpush.StatusFailed, "late", base.Add(4*time.Second))
	if completeErr != nil || ok {
		t.Errorf("MarkCompleted on sent row = %v, %v; want false", ok, completeErr)
	}
	// A queued (not sending) row is not completable either.
	q := createPush(t, repo, a, base.Add(5*time.Second))
	ok, completeErr = repo.MarkCompleted(ctx, q.ID, alertpush.StatusFailed, "x", base.Add(6*time.Second))
	if completeErr != nil || ok {
		t.Errorf("MarkCompleted on queued row = %v, %v; want false", ok, completeErr)
	}
}

func testAlertPushCancel(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p := createPush(t, repo, a, base)

	if err := repo.Cancel(ctx, p.ID, base.Add(time.Second)); err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	got := getPush(t, repo, p.ID)
	if got.Status != alertpush.StatusCanceled || got.CompletedAt == nil {
		t.Errorf("after cancel: %+v", got)
	}
	if err := repo.Cancel(ctx, p.ID, base.Add(2*time.Second)); !errors.Is(err, alertpush.ErrTerminal) {
		t.Errorf("Cancel twice = %v, want ErrTerminal", err)
	}
	if err := repo.Cancel(ctx, 999, base); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Cancel(999) = %v, want ErrNotFound", err)
	}
	// Sending rows cancel too (the dispatcher yields at its next cursor advance).
	p2 := createPush(t, repo, a, base.Add(3*time.Second))
	claimPushes(t, repo, base.Add(3*time.Second), base)
	if err := repo.Cancel(ctx, p2.ID, base.Add(4*time.Second)); err != nil {
		t.Errorf("Cancel sending: %v", err)
	}
}

func testAlertPushCascade(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p := createPush(t, repo, a, base)

	if _, err := repo.RecordFailure(ctx, p.ID, "t", "Unregistered", base); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := alertsRepo.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete alert: %v", err)
	}
	if _, err := repo.Get(ctx, p.ID); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Get after alert delete = %v, want ErrNotFound (cascade)", err)
	}
}
