package liveactivities

import (
	"cmp"
	"slices"
	"time"

	"github.com/OneBusAway/sidecar/internal/obaapi"
)

// ContentState is the §6.2 wire contract the iOS widget decodes with a
// default JSONDecoder. Keys and types are fixed; Arrivals marshals as []
// never null. NO timestamp or other always-changing field may ever be
// added: Changed compares consecutive states to decide whether to push.
type ContentState struct {
	Arrivals []ArrivalInfo `json:"arrivals"`
}

// ArrivalInfo is one row of the widget. DepartureTime is epoch SECONDS
// (ActivityKit dates decode from seconds; the rest of OBA is ms).
type ArrivalInfo struct {
	DepartureTime     int64  `json:"departure_time"`
	ScheduleStatus    string `json:"schedule_status"`    // early | on_time | delayed | unknown
	ScheduleDeviation int64  `json:"schedule_deviation"` // seconds; 0 when schedule-only
	IsArrival         bool   `json:"is_arrival"`
}

// EmptyContentState is the state with no arrivals, as stored before the
// first push and as sent on an end push with no history.
func EmptyContentState() ContentState { return ContentState{Arrivals: []ArrivalInfo{}} }

// Changed reports whether next differs from prev (arrivals only).
func Changed(prev, next ContentState) bool {
	return !slices.Equal(prev.Arrivals, next.Arrivals)
}

// Thresholds mirror OBAKitCore ArrivalDeparture.scheduleStatus exactly, in
// minutes, half-open on the late side: exactly -1.5 is on_time, exactly
// +1.5 is delayed (spec §6.2).
const (
	earlyThresholdMinutes  = -1.5
	onTimeThresholdMinutes = 1.5
)

// visitIdentity is OBAKitCore ArrivalDeparture.id's components: one trip's
// visit to one stop. Never tripId alone -- a loop route visits a stop twice
// per trip at different sequences, and a tripId recurs across service dates.
type visitIdentity struct {
	stopID, tripID, routeID string
	serviceDate, stopSeq    int64
}

// BuildContentState is a pure port of OBACloud's LiveActivityContentState
// and of the app's own client-side dedupe (design spec §2.3): filter to the
// bookmark, collapse duplicate vehicle reports, choose arrival vs departure
// and predicted vs scheduled times, drop the past, sort, cap. Both sides
// must pick the SAME survivor or a pushed card and a local refresh disagree.
func BuildContentState(entries []obaapi.StopArrival, routeShortName, tripHeadsign string, now time.Time) ContentState {
	matching := make([]obaapi.StopArrival, 0, len(entries))
	for _, e := range entries {
		if e.RouteShortName == routeShortName && e.TripHeadsign == tripHeadsign {
			matching = append(matching, e)
		}
	}
	nowSec := now.Unix()
	arrivals := make([]ArrivalInfo, 0, MaxArrivals)
	for _, e := range collapseDuplicateVehicleReports(matching) {
		if a, ok := arrivalInfo(e, nowSec); ok {
			arrivals = append(arrivals, a)
		}
	}
	slices.SortStableFunc(arrivals, func(a, b ArrivalInfo) int {
		return cmp.Compare(a.DepartureTime, b.DepartureTime)
	})
	if len(arrivals) > MaxArrivals {
		arrivals = arrivals[:MaxArrivals]
	}
	return ContentState{Arrivals: arrivals}
}

// collapseDuplicateVehicleReports keeps, per visit identity, the entry with
// the newest LastUpdateTime (ties: first in response order -- iOS replaces
// only on a strictly newer report). Entries without a complete identity are
// never collapsed: showing a duplicate row is cosmetic, hiding a bus is not.
func collapseDuplicateVehicleReports(entries []obaapi.StopArrival) []obaapi.StopArrival {
	out := make([]obaapi.StopArrival, 0, len(entries))
	survivor := make(map[visitIdentity]int) // identity -> index in out
	for _, e := range entries {
		if !e.HasIdentity {
			out = append(out, e)
			continue
		}
		id := visitIdentity{e.StopID, e.TripID, e.RouteID, e.ServiceDate, e.StopSequence}
		if i, seen := survivor[id]; seen {
			if e.LastUpdateTime > out[i].LastUpdateTime {
				out[i] = e
			}
			continue
		}
		survivor[id] = len(out)
		out = append(out, e)
	}
	return out
}

// arrivalInfo maps one entry per §6.2: any non-first stop is an "arrival"
// showing arrival times (falling back to departure times when a feed omits
// them); predicted times only when flagged and positive. Returns false when
// the chosen time is already past.
func arrivalInfo(e obaapi.StopArrival, nowSec int64) (ArrivalInfo, bool) {
	isArrival := e.StopSequence != 0
	useArrival := isArrival && (e.ScheduledArrivalTime > 0 || e.PredictedArrivalTime > 0)
	predictedMs, scheduledMs := e.PredictedDepartureTime, e.ScheduledDepartureTime
	if useArrival {
		predictedMs, scheduledMs = e.PredictedArrivalTime, e.ScheduledArrivalTime
	}
	predicted := e.Predicted && predictedMs > 0
	timeMs := scheduledMs
	var deviation int64
	if predicted {
		timeMs = predictedMs
		deviation = (predictedMs - scheduledMs) / 1000
	}
	timeSec := timeMs / 1000
	if timeSec < nowSec {
		return ArrivalInfo{}, false
	}
	return ArrivalInfo{
		DepartureTime:     timeSec,
		ScheduleStatus:    scheduleStatus(predicted, deviation),
		ScheduleDeviation: deviation,
		IsArrival:         isArrival,
	}, true
}

// scheduleStatus classifies a deviation into the widget's four-value enum.
func scheduleStatus(predicted bool, deviationSec int64) string {
	if !predicted {
		return "unknown"
	}
	minutes := float64(deviationSec) / 60.0
	switch {
	case minutes < earlyThresholdMinutes:
		return "early"
	case minutes < onTimeThresholdMinutes:
		return "on_time"
	default:
		return "delayed"
	}
}
