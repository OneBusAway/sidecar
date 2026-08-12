package alerts

import (
	"fmt"
	"time"
)

// minStart is the earliest acceptable alert start time. TimeRange.start/end
// are uint64 in the GTFS-realtime proto, so an instant before the epoch
// wraps to an enormous value on the wire instead of failing outright;
// rejecting anything before 2000 catches that along with ordinary year
// typos.
var minStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// ValidateWindow rejects windows that would either corrupt the wire
// representation or silently never show riders anything.
//
// This lives in the domain, not just the CLI, so every caller of
// Repository.Create/Update inherits it -- a future HTTP admin API or bulk
// importer that calls the repository directly gets the same protection an
// author typing at the CLI gets, rather than depending on every future
// caller remembering to re-implement it by convention.
func ValidateWindow(start time.Time, end *time.Time, now time.Time) error {
	// TimeRange.start/end are uint64 in the proto, so a negative epoch wraps
	// to an enormous value instead of failing.
	if start.Before(minStart) {
		return fmt.Errorf("start %s is before %s; check the year", start, minStart.Format("2006"))
	}
	if start.After(now.AddDate(10, 0, 0)) {
		return fmt.Errorf("start %s is more than 10 years out; check the year", start)
	}
	if end != nil && !end.After(start) {
		// Publishing this would succeed and the alert would appear in the
		// feed, but apps hide out-of-window alerts, so riders would never
		// see it and nothing would report an error.
		return fmt.Errorf("end %s must be after start %s", end, start)
	}
	return nil
}
