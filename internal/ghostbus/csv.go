package ghostbus

import (
	"encoding/csv"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/csvsafe"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// ghostBusHeader is the design §2.8 export column list, in order.
var ghostBusHeader = []string{
	"public_identifier", "reported_at_utc", "reported_at_local", "service_date",
	"route_id", "route_short_name", "headsign", "trip_id", "trip_report_count",
	"stop_id", "stop_name", "stop_sequence", "vehicle_id",
	"predicted", "wait_duration_minutes", "comment",
	"scheduled_arrival_utc", "predicted_arrival_utc", "schedule_deviation_minutes",
	"prediction_last_updated_utc", "prediction_staleness_minutes",
	"user_latitude", "user_longitude",
	"snapshot_status", "snapshot_captured_at_utc",
	"vehicle_last_lat", "vehicle_last_lon", "vehicle_distance_from_stop_m",
	"snapshot_trip_phase", "snapshot_json",
}

// csvSnapshot is the subset of the stored snapshot document the CSV
// renders. Pointer fields because absent-vs-zero matters: a defaulted zero
// coordinate is Null Island, and a distance computed from it is garbage.
type csvSnapshot struct {
	Status struct {
		Phase             string     `json:"phase"`
		LastKnownLocation *csvLatLon `json:"lastKnownLocation"`
		Position          *csvLatLon `json:"position"`
	} `json:"status"`
	Display struct {
		RouteShortName string   `json:"route_short_name"`
		Headsign       string   `json:"headsign"`
		StopName       string   `json:"stop_name"`
		StopLat        *float64 `json:"stop_lat"`
		StopLon        *float64 `json:"stop_lon"`
	} `json:"display"`
}

type csvLatLon struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}

// WriteReportsCSV streams one row per report. An unloadable (or empty,
// unconfigured) region timezone falls back to UTC for reported_at_local and
// service_date rather than failing the export. region is needed only for
// its timezone -- the column set otherwise depends on nothing but reports
// itself.
func WriteReportsCSV(w io.Writer, region regions.Region, reports []Report) error {
	loc, err := time.LoadLocation(region.Timezone)
	if err != nil {
		loc = time.UTC
	}

	// One pre-pass over the exported slice: how many reports share a trip
	// instance (trip_identifier, service_date), so the agency can see
	// "this ghost bus was reported by 2 different riders" without a
	// spreadsheet pivot.
	tripCounts := make(map[[2]any]int, len(reports))
	for _, r := range reports {
		tripCounts[[2]any{r.TripIdentifier, r.ServiceDate}]++
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(ghostBusHeader); err != nil {
		return err
	}
	for _, r := range reports {
		if err := cw.Write(reportRow(r, loc, tripCounts)); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

func reportRow(r Report, loc *time.Location, tripCounts map[[2]any]int) []string {
	var snap csvSnapshot
	if r.SnapshotJSON != "" {
		// A malformed or unrecognized snapshot document degrades to blank
		// derived columns, not a failed export -- the raw snapshot_json
		// column still carries the original document for inspection.
		if err := json.Unmarshal([]byte(r.SnapshotJSON), &snap); err != nil {
			snap = csvSnapshot{}
		}
	}

	vehLat, vehLon := vehiclePosition(snap)
	distance := ""
	if vehLat != nil && vehLon != nil && snap.Display.StopLat != nil && snap.Display.StopLon != nil {
		d := HaversineMeters(*vehLat, *vehLon, *snap.Display.StopLat, *snap.Display.StopLon)
		distance = strconv.FormatFloat(d, 'f', -1, 64)
	}

	return []string{
		csvsafe.Cell(r.PublicID),
		r.CreatedAt.UTC().Format(time.RFC3339),
		r.CreatedAt.In(loc).Format(time.RFC3339),
		time.UnixMilli(r.ServiceDate).In(loc).Format("2006-01-02"),
		csvsafe.Cell(r.RouteIdentifier),
		csvsafe.Cell(snap.Display.RouteShortName),
		csvsafe.Cell(snap.Display.Headsign),
		csvsafe.Cell(r.TripIdentifier),
		strconv.Itoa(tripCounts[[2]any{r.TripIdentifier, r.ServiceDate}]),
		csvsafe.Cell(r.StopIdentifier),
		csvsafe.Cell(snap.Display.StopName),
		int64Cell(r.StopSequence),
		csvsafe.Cell(r.VehicleIdentifier),
		boolCell(r.Predicted),
		strconv.FormatInt(r.WaitDurationMinutes, 10),
		csvsafe.Cell(r.Comment),
		msToUTC(r.ScheduledArrivalAt),
		msToUTC(r.PredictedArrivalAt),
		int64Cell(r.ScheduleDeviationMinutes),
		msToUTC(r.PredictionLastUpdatedAt),
		stalenessCell(r.ScheduledArrivalAt, r.PredictionLastUpdatedAt),
		csvsafe.Float(r.UserLatitude),
		csvsafe.Float(r.UserLongitude),
		csvsafe.Cell(r.SnapshotStatus),
		timeCell(r.SnapshotCapturedAt),
		csvsafe.Float(vehLat),
		csvsafe.Float(vehLon),
		distance,
		csvsafe.Cell(snap.Status.Phase),
		csvsafe.Cell(r.SnapshotJSON),
	}
}

// vehiclePosition resolves the vehicle's last known position:
// lastKnownLocation falling back to position, matching the OBACloud
// reference (status["lastKnownLocation"] || status["position"]).
func vehiclePosition(snap csvSnapshot) (lat, lon *float64) {
	pos := snap.Status.LastKnownLocation
	if pos == nil {
		pos = snap.Status.Position
	}
	if pos == nil {
		return nil, nil
	}
	return pos.Lat, pos.Lon
}

func int64Cell(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func boolCell(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "true"
	}
	return "false"
}

func msToUTC(ms *int64) string {
	if ms == nil {
		return ""
	}
	return time.UnixMilli(*ms).UTC().Format(time.RFC3339)
}

func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// stalenessCell is how many minutes before/after the scheduled arrival the
// last prediction update landed. Both timestamps are epoch milliseconds, so
// the divisor to minutes is 60,000 -- NOT 60 (that 1000x mistake is caught
// by the R1 fixture in csv_test.go).
func stalenessCell(scheduledArrivalAt, predictionLastUpdatedAt *int64) string {
	if scheduledArrivalAt == nil || predictionLastUpdatedAt == nil {
		return ""
	}
	minutes := math.Round(float64(*scheduledArrivalAt-*predictionLastUpdatedAt) / 60000)
	return strconv.FormatFloat(minutes, 'f', -1, 64)
}
