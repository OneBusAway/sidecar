package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/csvsafe"
	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
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

// ghostBusCmd dispatches the ghostbus subcommands (export only, this
// slice). The CSV is the agency-facing read surface -- there is
// deliberately no rider-facing read API (spec §8).
const ghostBusExportUsage = "usage: ghostbus export --region N [--since RFC3339]"

func ghostBusCmd(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New(ghostBusExportUsage)
	}
	fs := flag.NewFlagSet("ghostbus export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id to export")
	since := fs.String("since", "", "only reports created at or after this RFC 3339 instant")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// flag.Parse stops at the first non-flag argument, so trailing
	// positionals would otherwise be silently ignored -- and a typo like a
	// misspelled flag would export anyway.
	if fs.NArg() != 0 {
		return errors.New(ghostBusExportUsage)
	}
	if *regionID == 0 {
		return errors.New(ghostBusExportUsage)
	}
	region, err := store.Regions().Get(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("region %d: %w", *regionID, err)
	}
	var sinceUnix int64
	if *since != "" {
		parsed, parseErr := time.Parse(time.RFC3339, *since)
		if parseErr != nil {
			return fmt.Errorf("--since must be RFC 3339 with an explicit UTC offset: %w", parseErr)
		}
		sinceUnix = parsed.Unix()
	}
	reports, err := store.GhostBus().ListForExport(ctx, *regionID, sinceUnix)
	if err != nil {
		return fmt.Errorf("list reports: %w", err)
	}
	return writeGhostBusCSV(stdout, region, reports)
}

// ghostBusSnapshot is the subset of the stored snapshot document the CSV
// renders. Pointer fields because absent-vs-zero matters: a defaulted zero
// coordinate is Null Island, and a distance computed from it is garbage.
type ghostBusSnapshot struct {
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

// writeGhostBusCSV streams one row per report. An unloadable (or empty,
// unconfigured) region timezone falls back to UTC for reported_at_local and
// service_date rather than failing the export.
func writeGhostBusCSV(stdout io.Writer, region regions.Region, reports []ghostbus.Report) error {
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

	w := csv.NewWriter(stdout)
	if err := w.Write(ghostBusHeader); err != nil {
		return err
	}
	for _, r := range reports {
		if err := w.Write(ghostBusRow(r, loc, tripCounts)); err != nil {
			return err
		}
	}
	w.Flush()
	return w.Error()
}

func ghostBusRow(r ghostbus.Report, loc *time.Location, tripCounts map[[2]any]int) []string {
	var snap ghostBusSnapshot
	if r.SnapshotJSON != "" {
		// A malformed or unrecognized snapshot document degrades to blank
		// derived columns, not a failed export -- the raw snapshot_json
		// column still carries the original document for inspection.
		if err := json.Unmarshal([]byte(r.SnapshotJSON), &snap); err != nil {
			snap = ghostBusSnapshot{}
		}
	}

	vehLat, vehLon := ghostBusVehiclePosition(snap)
	distance := ""
	if vehLat != nil && vehLon != nil && snap.Display.StopLat != nil && snap.Display.StopLon != nil {
		d := ghostbus.HaversineMeters(*vehLat, *vehLon, *snap.Display.StopLat, *snap.Display.StopLon)
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
		ghostBusInt64Cell(r.StopSequence),
		csvsafe.Cell(r.VehicleIdentifier),
		ghostBusBoolCell(r.Predicted),
		strconv.FormatInt(r.WaitDurationMinutes, 10),
		csvsafe.Cell(r.Comment),
		ghostBusMsToUTC(r.ScheduledArrivalAt),
		ghostBusMsToUTC(r.PredictedArrivalAt),
		ghostBusInt64Cell(r.ScheduleDeviationMinutes),
		ghostBusMsToUTC(r.PredictionLastUpdatedAt),
		ghostBusStalenessCell(r.ScheduledArrivalAt, r.PredictionLastUpdatedAt),
		csvsafe.Float(r.UserLatitude),
		csvsafe.Float(r.UserLongitude),
		csvsafe.Cell(r.SnapshotStatus),
		ghostBusTimeCell(r.SnapshotCapturedAt),
		csvsafe.Float(vehLat),
		csvsafe.Float(vehLon),
		distance,
		csvsafe.Cell(snap.Status.Phase),
		csvsafe.Cell(r.SnapshotJSON),
	}
}

// ghostBusVehiclePosition resolves the vehicle's last known position:
// lastKnownLocation falling back to position, matching the OBACloud
// reference (status["lastKnownLocation"] || status["position"]).
func ghostBusVehiclePosition(snap ghostBusSnapshot) (lat, lon *float64) {
	pos := snap.Status.LastKnownLocation
	if pos == nil {
		pos = snap.Status.Position
	}
	if pos == nil {
		return nil, nil
	}
	return pos.Lat, pos.Lon
}

func ghostBusInt64Cell(v *int64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatInt(*v, 10)
}

func ghostBusBoolCell(v *bool) string {
	if v == nil {
		return ""
	}
	if *v {
		return "true"
	}
	return "false"
}

func ghostBusMsToUTC(ms *int64) string {
	if ms == nil {
		return ""
	}
	return time.UnixMilli(*ms).UTC().Format(time.RFC3339)
}

func ghostBusTimeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ghostBusStalenessCell is how many minutes before/after the scheduled
// arrival the last prediction update landed. Both timestamps are epoch
// milliseconds, so the divisor to minutes is 60,000 -- NOT 60 (that 1000x
// mistake is caught by the R1 fixture in ghostbus_test.go).
func ghostBusStalenessCell(scheduledArrivalAt, predictionLastUpdatedAt *int64) string {
	if scheduledArrivalAt == nil || predictionLastUpdatedAt == nil {
		return ""
	}
	minutes := math.Round(float64(*scheduledArrivalAt-*predictionLastUpdatedAt) / 60000)
	return strconv.FormatFloat(minutes, 'f', -1, 64)
}
