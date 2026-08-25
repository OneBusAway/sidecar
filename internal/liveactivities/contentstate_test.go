package liveactivities_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
)

// now is the fixed instant every builder test measures "past" against.
var now = time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

// entry builds a fully-identified, predicted arrival at now+offset with the
// given deviation, at stopSequence 3 (an "arrival" per §6.2).
func entry(offset, deviation time.Duration) obaapi.StopArrival {
	sched := now.Add(offset)
	pred := sched.Add(deviation)
	return obaapi.StopArrival{
		StopID: "1_570", TripID: "1_" + sched.Format("150405"), RouteID: "1_100044",
		ServiceDate: 1754809200000, StopSequence: 3, HasIdentity: true,
		LastUpdateTime: ms(now), RouteShortName: "44", TripHeadsign: "Ballard", Predicted: true,
		ScheduledArrivalTime: ms(sched), PredictedArrivalTime: ms(pred),
		ScheduledDepartureTime: ms(sched.Add(time.Second)), PredictedDepartureTime: ms(pred.Add(time.Second)),
	}
}

func build(entries ...obaapi.StopArrival) liveactivities.ContentState {
	return liveactivities.BuildContentState(entries, "44", "Ballard", now)
}

func TestFixtureRoundTripsWithDefaultDecoder(t *testing.T) {
	raw, err := os.ReadFile("testdata/live_activity_content_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var state liveactivities.ContentState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(state.Arrivals) != 3 || state.Arrivals[0].DepartureTime != 1767980460 ||
		state.Arrivals[0].ScheduleStatus != "on_time" || state.Arrivals[0].ScheduleDeviation != 60 ||
		state.Arrivals[0].IsArrival || state.Arrivals[1].ScheduleStatus != "delayed" ||
		state.Arrivals[2].ScheduleStatus != "unknown" {
		t.Fatalf("decoded fixture = %+v", state)
	}
	// Canonical form of the fixture vs canonical form of our encoding: any
	// key rename or type change diverges here.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	wantCanon, _ := json.Marshal(generic)
	ours, _ := json.Marshal(state)
	var oursGeneric any
	_ = json.Unmarshal(ours, &oursGeneric)
	gotCanon, _ := json.Marshal(oursGeneric)
	if !bytes.Equal(wantCanon, gotCanon) {
		t.Errorf("encoding drifted from fixture:\n got %s\nwant %s", gotCanon, wantCanon)
	}
}

func TestEmptyStateMarshalsArrivalsAsEmptyArray(t *testing.T) {
	b, _ := json.Marshal(liveactivities.EmptyContentState())
	if string(b) != `{"arrivals":[]}` {
		t.Errorf("got %s", b)
	}
	b, _ = json.Marshal(build())
	if string(b) != `{"arrivals":[]}` {
		t.Errorf("BuildContentState with no entries: got %s", b)
	}
}

func TestBuildFiltersByRouteAndHeadsign(t *testing.T) {
	other := entry(5*time.Minute, 0)
	other.TripHeadsign = "Downtown"
	wrongRoute := entry(6*time.Minute, 0)
	wrongRoute.RouteShortName = "45"
	got := build(other, wrongRoute, entry(7*time.Minute, 0))
	if len(got.Arrivals) != 1 || got.Arrivals[0].DepartureTime != now.Add(7*time.Minute).Unix() {
		t.Errorf("got %+v", got.Arrivals)
	}
}

func TestBuildCollapsesDuplicateVehicleReports(t *testing.T) {
	a := entry(5*time.Minute, 0)
	b := a
	b.LastUpdateTime = a.LastUpdateTime + 1
	b.PredictedArrivalTime += 60_000 // newer report, different minute
	got := build(a, b)
	if len(got.Arrivals) != 1 {
		t.Fatalf("want 1 arrival, got %+v", got.Arrivals)
	}
	if got.Arrivals[0].DepartureTime != b.PredictedArrivalTime/1000 {
		t.Errorf("survivor should be the newest report; got %d want %d", got.Arrivals[0].DepartureTime, b.PredictedArrivalTime/1000)
	}
}

func TestBuildTieKeepsFirstInResponseOrder(t *testing.T) {
	a := entry(5*time.Minute, 0)
	b := a
	b.PredictedArrivalTime += 60_000
	got := build(a, b)
	if len(got.Arrivals) != 1 || got.Arrivals[0].DepartureTime != a.PredictedArrivalTime/1000 {
		t.Errorf("tie must keep first in order; got %+v", got.Arrivals)
	}
}

func TestBuildNeverCollapsesLoopRouteOrCrossServiceDateOrUnidentified(t *testing.T) {
	loopA := entry(5*time.Minute, 0)
	loopB := loopA
	loopB.StopSequence = 9
	loopB.PredictedArrivalTime += 600_000
	crossDate := loopA
	crossDate.ServiceDate += 86_400_000
	crossDate.PredictedArrivalTime += 1_200_000
	unidentA := loopA
	unidentA.HasIdentity = false
	unidentB := unidentA
	got := build(loopA, loopB, crossDate, unidentA, unidentB)
	if len(got.Arrivals) != 3 {
		t.Errorf("want cap of 3 distinct arrivals, got %d: %+v", len(got.Arrivals), got.Arrivals)
	}
	got = build(unidentA, unidentB)
	if len(got.Arrivals) != 2 {
		t.Errorf("unidentified entries must never collapse; got %+v", got.Arrivals)
	}
}

func TestBuildArrivalVsDepartureSelection(t *testing.T) {
	first := entry(5*time.Minute, 0)
	first.StopSequence = 0
	got := build(first)
	if got.Arrivals[0].IsArrival {
		t.Error("stopSequence 0 is a departure")
	}
	if got.Arrivals[0].DepartureTime != first.PredictedDepartureTime/1000 {
		t.Errorf("departure pair expected; got %d", got.Arrivals[0].DepartureTime)
	}
	noArr := entry(5*time.Minute, 0)
	noArr.ScheduledArrivalTime, noArr.PredictedArrivalTime = 0, 0
	got = build(noArr)
	if !got.Arrivals[0].IsArrival || got.Arrivals[0].DepartureTime != noArr.PredictedDepartureTime/1000 {
		t.Errorf("omitted arrival times must fall back to departure pair; got %+v", got.Arrivals[0])
	}
}

func TestBuildPredictedOnlyWhenFlaggedAndPositive(t *testing.T) {
	sched := entry(5*time.Minute, 2*time.Minute)
	sched.Predicted = false
	got := build(sched)
	a := got.Arrivals[0]
	if a.DepartureTime != sched.ScheduledArrivalTime/1000 || a.ScheduleStatus != "unknown" || a.ScheduleDeviation != 0 {
		t.Errorf("unpredicted: %+v", a)
	}
	zeroPred := entry(5*time.Minute, 2*time.Minute)
	zeroPred.PredictedArrivalTime = 0
	got = build(zeroPred)
	if got.Arrivals[0].ScheduleStatus != "unknown" {
		t.Errorf("predicted with 0 time must be unknown: %+v", got.Arrivals[0])
	}
}

func TestBuildDropsPastSortsAndCaps(t *testing.T) {
	past := entry(-2*time.Minute, 0)
	atNow := entry(0, 0)
	got := build(entry(30*time.Minute, 0), past, entry(10*time.Minute, 0), atNow, entry(20*time.Minute, 0), entry(40*time.Minute, 0))
	if len(got.Arrivals) != 3 {
		t.Fatalf("cap: got %d", len(got.Arrivals))
	}
	if got.Arrivals[0].DepartureTime != now.Unix() {
		t.Errorf("an entry at exactly now must survive; first = %d", got.Arrivals[0].DepartureTime)
	}
	for i := 1; i < len(got.Arrivals); i++ {
		if got.Arrivals[i].DepartureTime < got.Arrivals[i-1].DepartureTime {
			t.Errorf("not sorted: %+v", got.Arrivals)
		}
	}
	if got.Arrivals[2].DepartureTime != now.Add(20*time.Minute).Unix() {
		t.Errorf("third should be +20m, got %d", got.Arrivals[2].DepartureTime)
	}
}

func TestScheduleStatusThresholds(t *testing.T) {
	cases := []struct {
		dev  time.Duration
		want string
	}{
		{-91 * time.Second, "early"}, {-90 * time.Second, "on_time"},
		{89 * time.Second, "on_time"}, {90 * time.Second, "delayed"},
	}
	for _, c := range cases {
		got := build(entry(5*time.Minute, c.dev)).Arrivals[0]
		if got.ScheduleStatus != c.want || got.ScheduleDeviation != int64(c.dev/time.Second) {
			t.Errorf("dev %v: got %+v, want %s", c.dev, got, c.want)
		}
	}
}

func TestChangedComparesArrivalsOnly(t *testing.T) {
	a := build(entry(5*time.Minute, 0))
	b := build(entry(5*time.Minute, 0))
	if liveactivities.Changed(a, b) {
		t.Error("identical states must not be changed")
	}
	c := build(entry(6*time.Minute, 0))
	if !liveactivities.Changed(a, c) {
		t.Error("different departure time must be changed")
	}
	if !liveactivities.Changed(liveactivities.EmptyContentState(), a) {
		t.Error("empty vs non-empty must be changed")
	}
}
