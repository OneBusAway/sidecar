package sqlite

import (
	"bytes"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/export"
	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// validateRiderRows holds imported rider state to the rules the HTTP layer
// applies to the same fields on the way in, so a migrated row is never one
// the rider API would have refused: identifier and token length caps,
// coordinate ranges, the wait-duration choices, and a snapshot that is a
// JSON object of sane size. Each check is a rejection naming the row, not a
// normalization -- identifiers are dedupe keys and coordinates are data, so
// silently truncating or nulling either would change what the row means;
// the operator fixes or deletes the row in OBACloud and re-exports. OBACloud
// validates most of these itself, so a real export should not trip them.
// The one deliberate normalization -- a non-test device's description -- is
// in importer.registrations, matching the HTTP layer's own rule.
func validateRiderRows(doc *export.Document) error {
	for _, r := range doc.SurveyResponses {
		row := fmt.Sprintf("survey response %s", r.PublicID)
		if err := firstErr(
			maxBytes(row, "user_identifier", r.UserIdentifier, surveys.MaxIdentifierLen),
			maxBytes(row, "stop_identifier", r.StopIdentifier, surveys.MaxIdentifierLen),
			inRange(row, "stop_latitude", r.StopLatitude, 90),
			inRange(row, "stop_longitude", r.StopLongitude, 180),
		); err != nil {
			return err
		}
	}
	for _, p := range doc.PushRegistrations {
		// The token is a secret; name the row by its tail only.
		row := fmt.Sprintf("push registration …%s", tail(p.Token, 8))
		if err := maxBytes(row, "token", p.Token, pushreg.MaxTokenLen); err != nil {
			return err
		}
		if p.TestDevice {
			if err := maxBytes(row, "description", p.Description, pushreg.MaxDescriptionLen); err != nil {
				return err
			}
		}
	}
	for _, g := range doc.GhostBusReports {
		if err := validateReport(g); err != nil {
			return err
		}
	}
	return nil
}

func validateReport(g export.GhostBusReport) error {
	row := fmt.Sprintf("ghost bus report %s", g.PublicID)
	if err := firstErr(
		maxBytes(row, "user_identifier", g.UserIdentifier, ghostbus.MaxIdentifierLen),
		maxBytes(row, "trip_identifier", g.TripIdentifier, ghostbus.MaxIdentifierLen),
		maxBytes(row, "route_identifier", g.RouteIdentifier, ghostbus.MaxIdentifierLen),
		maxBytes(row, "stop_identifier", g.StopIdentifier, ghostbus.MaxIdentifierLen),
		maxBytes(row, "vehicle_identifier", g.VehicleIdentifier, ghostbus.MaxIdentifierLen),
		inRange(row, "user_latitude", g.UserLatitude, 90),
		inRange(row, "user_longitude", g.UserLongitude, 180),
	); err != nil {
		return err
	}
	if n := utf8.RuneCountInString(g.Comment); n > ghostbus.CommentMaxLen {
		return fmt.Errorf("export: %s: comment is %d characters (maximum is %d)", row, n, ghostbus.CommentMaxLen)
	}
	if !ghostbus.ValidWaitDuration(g.WaitDurationMinutes) {
		return fmt.Errorf("export: %s: wait_duration_minutes %d is not one of %v", row, g.WaitDurationMinutes, ghostbus.WaitDurationChoices)
	}
	if g.SnapshotStatus == ghostbus.SnapshotCaptured {
		if len(g.Snapshot) > ghostbus.MaxSnapshotBytes {
			return fmt.Errorf("export: %s: snapshot is %d bytes (maximum is %d)", row, len(g.Snapshot), ghostbus.MaxSnapshotBytes)
		}
		if trimmed := bytes.TrimSpace(g.Snapshot); len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("export: %s: a captured snapshot must be a JSON object", row)
		}
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func maxBytes(row, field, v string, limit int) error {
	if len(v) > limit {
		return fmt.Errorf("export: %s: %s is %d bytes (maximum is %d)", row, field, len(v), limit)
	}
	return nil
}

func inRange(row, field string, v *float64, limit float64) error {
	if v != nil && (math.IsNaN(*v) || math.Abs(*v) > limit) {
		return fmt.Errorf("export: %s: %s %v is outside -%v..%v", row, field, *v, limit, limit)
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
