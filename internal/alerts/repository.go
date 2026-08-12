package alerts

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a lookup finds no row. Callers distinguish it
// with errors.Is; the HTTP layer maps it to 404.
var ErrNotFound = errors.New("alert not found")

// NewAlert is the input to Create. AgencyID is already resolved — the CLI
// applies the region default before calling, so the stored value never changes
// underneath a published alert. Create rejects an empty AgencyID and an
// invalid start/end window itself, rather than trusting every caller to have
// checked first — see alertRepo.Create in the sqlite adapter and
// ValidateWindow.
type NewAlert struct {
	RegionID        int64
	AgencyID        string
	HeaderText      string
	DescriptionText string
	URL             string
	Cause           string
	Effect          string
	Severity        string
	StartTime       time.Time
	EndTime         *time.Time
	IsTest          bool
}

// Patch carries an edit. A nil field means "leave unchanged"; this is why
// every field is a pointer rather than a value.
type Patch struct {
	AgencyID        *string
	HeaderText      *string
	DescriptionText *string
	URL             *string
	Cause           *string
	Effect          *string
	Severity        *string
	StartTime       *time.Time
	EndTime         *time.Time
	ClearEndTime    bool // distinct from EndTime == nil, which means "unchanged"
	IsTest          *bool
}

// ListFilter selects alerts for administrative listing.
// A nil RegionID means every region. It is a pointer rather than a
// zero-sentinel because region 0 is a real region (Tampa Bay), so 0 must
// mean "region 0", not "all".
type ListFilter struct {
	RegionID *int64
}

// Repository stores alerts. Implementations must be safe for concurrent use.
type Repository interface {
	Create(ctx context.Context, in NewAlert, now time.Time) (Alert, error)
	Get(ctx context.Context, id int64) (Alert, error)
	Update(ctx context.Context, id int64, p Patch, now time.Time) (Alert, error)
	SetPublished(ctx context.Context, id int64, published bool, now time.Time) error
	Delete(ctx context.Context, id int64) error
	List(ctx context.Context, f ListFilter) ([]Alert, error)

	// Feed returns published alerts for one region, newest first, capped at
	// limit, with translations attached. Implementations run both queries in a
	// single read transaction.
	Feed(ctx context.Context, regionID int64, includeTest bool, limit int) ([]Alert, error)

	UpsertTranslation(ctx context.Context, alertID int64, t Translation, now time.Time) error
}
