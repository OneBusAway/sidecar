// Package obaapi is the sidecar's client for the OneBusAway REST API.
//
// It is the only package permitted to import github.com/OneBusAway/go-sdk.
// The SDK's response types are generated, deeply nested, and carry per-field
// JSON metadata on every struct; letting them reach the domain packages would
// make those untestable without the SDK and pin the sidecar to the SDK's type
// layout. Everything crossing this boundary is a flat local type.
package obaapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	oba "github.com/OneBusAway/go-sdk"
	"github.com/OneBusAway/go-sdk/option"
	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/httpx"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// ErrNotConfigured means neither the region nor the process supplied an API
// key, so no request was attempted.
var ErrNotConfigured = errors.New("obaapi: region has no API key")

// ErrNotFound means the OBA server answered but knows nothing about this
// trip/stop/date combination -- the alarm-reaper's "trip aged out" signal,
// distinct from transient transport failures which must NOT count toward
// the 3-strike streak (spec §5.3).
var ErrNotFound = errors.New("obaapi: arrival-and-departure not found")

// Agency is one transit agency with coverage in a region.
type Agency struct {
	ID   string
	Name string
}

// Vehicle is one vehicle currently reported by an agency's realtime feed.
type Vehicle struct {
	AgencyID   string
	AgencyName string
	VehicleID  string
}

// DepartureQuery keys one arrival-and-departure-for-stop lookup (§5.3).
type DepartureQuery struct {
	StopID       string
	TripID       string
	ServiceDate  int64  // epoch ms
	VehicleID    string // "" = omit
	StopSequence *int64 // nil = omit
}

// Departure is the slice of the OBA response alarms need.
type Departure struct {
	RouteShortName         string
	TripHeadsign           string
	ScheduledDepartureTime int64 // epoch ms
	PredictedDepartureTime int64 // epoch ms; 0 = no realtime
	Predicted              bool
}

// Client reads the OBA REST API for one region at a time. Implementations
// must be safe for concurrent use.
type Client interface {
	// Fleet returns every vehicle currently reported across every agency with
	// coverage in the region, in agencies-with-coverage order then each
	// agency's own response order.
	Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error)

	// ArrivalAndDeparture looks up one trip's arrival/departure at a stop.
	// It returns ErrNotFound when the upstream knows nothing about the
	// trip/stop/date combination, and ErrNotConfigured when the region has
	// no API key.
	ArrivalAndDeparture(ctx context.Context, region regions.Region, q DepartureQuery) (Departure, error)
}

const (
	// perRequestTimeout bounds one HTTP attempt. The SDK documents its
	// request timeout as per-attempt, spanning neither retries nor the
	// surrounding context, which is why retries are disabled below -- with
	// retries on, one logical call is two attempts plus backoff and no
	// timeout arithmetic holds.
	perRequestTimeout = 4 * time.Second

	// maxRetries is zero deliberately; see perRequestTimeout.
	maxRetries = 0

	// agencyConcurrency bounds the vehicles-for-agency fan-out. Twelve keeps
	// every region that exists today to a single round while staying polite
	// to the upstream server.
	agencyConcurrency = 12
)

type client struct {
	defaultKey string
	http       *http.Client
	logger     *slog.Logger
}

// New builds a Client. defaultKey is the process-wide fallback used for any
// region that carries no key of its own; it may be empty.
func New(defaultKey string, httpClient *http.Client, logger *slog.Logger) Client {
	if logger == nil {
		logger = slog.Default()
	}
	// The key travels in the query string, so a followed redirect would leak
	// it to the redirect target via Referer; see httpx.NoRedirectClient.
	return &client{defaultKey: defaultKey, http: httpx.NoRedirectClient(httpClient), logger: logger}
}

func (c *client) Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error) {
	key := region.OBAAPIKey
	if key == "" {
		key = c.defaultKey
	}
	if key == "" {
		return nil, ErrNotConfigured
	}

	sdk := oba.NewClient(
		option.WithBaseURL(region.OBABaseURL),
		option.WithAPIKey(key),
		option.WithHTTPClient(c.http),
		option.WithRequestTimeout(perRequestTimeout),
		option.WithMaxRetries(maxRetries),
	)

	agencies, err := c.agencies(ctx, sdk)
	if err != nil {
		return nil, err
	}

	// Per-agency results are collected into a slice indexed by agency
	// position, not appended as they arrive: parallel completion order is not
	// deterministic and the response must be.
	perAgency := make([][]Vehicle, len(agencies))
	// Counted so an all-agencies-tolerated fetch is observable; see the
	// warning after g.Wait below.
	var tolerated atomic.Int64

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(agencyConcurrency)
	for i, agency := range agencies {
		g.Go(func() error {
			list, err := sdk.VehiclesForAgency.List(gctx, agency.ID, oba.VehiclesForAgencyListParams{})
			if err != nil {
				// An agency listed in agencies-with-coverage but with no
				// realtime feed answers 4xx forever. Failing the whole fetch
				// would take the region's vehicle search down permanently and
				// re-hammer the upstream on every miss. Anything else -- a
				// 5xx, a timeout, a transport error -- is a real failure:
				// caching a fleet with an agency silently missing tells every
				// rider on its routes that their bus does not exist.
				if isClientError(err) {
					tolerated.Add(1)
					c.logger.Warn("obaapi: agency has no vehicle feed",
						"region_id", region.ID, "agency_id", agency.ID, "status", statusOf(err))
					return nil
				}
				return fmt.Errorf("obaapi: vehicles for agency %s in region %d: %w",
					agency.ID, region.ID, redact(err))
			}
			out := make([]Vehicle, 0, len(list.Data.List))
			for _, v := range list.Data.List {
				out = append(out, Vehicle{
					AgencyID:   agency.ID,
					AgencyName: agency.Name,
					VehicleID:  v.VehicleID,
				})
			}
			perAgency[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Every agency answered 4xx, so the fleet is empty and vehicle search will
	// return "no such vehicle" for the next 30 minutes (the fleet cache's TTL).
	// That is the honest answer for a region with no realtime API at all --
	// MTA New York advertises supportsObaRealtimeApis: false in the live
	// regions directory -- but it is indistinguishable here from a key that is
	// valid for the discovery API and not the realtime one, where it hides
	// every bus in the region. The 4xx alone cannot tell those apart, so this
	// logs loudly rather than guessing; see the follow-up on the pull request.
	if len(agencies) > 0 && int(tolerated.Load()) == len(agencies) {
		c.logger.Warn("obaapi: every agency declined a vehicle feed; fleet is empty",
			"region_id", region.ID, "agencies", len(agencies))
	}

	total := 0
	for _, vs := range perAgency {
		total += len(vs)
	}
	fleet := make([]Vehicle, 0, total)
	for _, vs := range perAgency {
		fleet = append(fleet, vs...)
	}
	return fleet, nil
}

func (c *client) ArrivalAndDeparture(ctx context.Context, region regions.Region, q DepartureQuery) (Departure, error) {
	key := region.OBAAPIKey
	if key == "" {
		key = c.defaultKey
	}
	if key == "" {
		return Departure{}, ErrNotConfigured
	}
	sdk := oba.NewClient(
		option.WithBaseURL(region.OBABaseURL),
		option.WithAPIKey(key),
		option.WithHTTPClient(c.http),
		option.WithRequestTimeout(perRequestTimeout),
		option.WithMaxRetries(maxRetries),
	)

	params := oba.ArrivalAndDepartureGetParams{
		TripID:      oba.F(q.TripID),
		ServiceDate: oba.F(q.ServiceDate),
	}
	if q.VehicleID != "" {
		params.VehicleID = oba.F(q.VehicleID)
	}
	if q.StopSequence != nil {
		params.StopSequence = oba.F(*q.StopSequence)
	}

	resp, err := sdk.ArrivalAndDeparture.Get(ctx, q.StopID, params)
	if err != nil {
		// A 404 is the upstream's "no such trip at this stop/date" -- the
		// signal §5.3's reaper counts. Everything else stays a redacted
		// transient error the caller must not count.
		if statusOf(err) == http.StatusNotFound {
			return Departure{}, ErrNotFound
		}
		return Departure{}, fmt.Errorf("obaapi: arrival-and-departure in region %d: %w", region.ID, redact(err))
	}

	entry := resp.Data.Entry
	if entry.TripID == "" {
		// A 200 with an empty entry is the same "nothing here" as a 404.
		return Departure{}, ErrNotFound
	}

	shortName := entry.RouteShortName
	if shortName == "" {
		for _, route := range resp.Data.References.Routes {
			if route.ID == entry.RouteID {
				shortName = route.ShortName
				if shortName == "" {
					shortName = route.LongName
				}
				break
			}
		}
	}
	return Departure{
		RouteShortName:         shortName,
		TripHeadsign:           entry.TripHeadsign,
		ScheduledDepartureTime: entry.ScheduledDepartureTime,
		PredictedDepartureTime: entry.PredictedDepartureTime,
		Predicted:              entry.Predicted,
	}, nil
}

// agencies fetches the region's agencies. Names live in the response's
// references block rather than on the list entries (which carry only ids and
// bounding boxes), so one call yields both.
func (c *client) agencies(ctx context.Context, sdk *oba.Client) ([]Agency, error) {
	resp, err := sdk.AgenciesWithCoverage.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("obaapi: agencies with coverage: %w", redact(err))
	}

	names := make(map[string]string, len(resp.Data.References.Agencies))
	for _, a := range resp.Data.References.Agencies {
		names[a.ID] = a.Name
	}

	out := make([]Agency, 0, len(resp.Data.List))
	for _, a := range resp.Data.List {
		out = append(out, Agency{ID: a.AgencyID, Name: names[a.AgencyID]})
	}
	return out, nil
}

// redact strips any URL-bearing text from an upstream error. Both a non-2xx
// response and a transport failure carry the full request URL somewhere in
// their message -- *url.Error embeds it directly, and the SDK's *oba.Error
// formats its Error() string as "GET \"<url>\": <status> <text>" -- and
// either one carries the OBA key as a query parameter. An error logged
// verbatim writes the secret to disk, undoing the care taken to keep it out
// of every JSON response. Every branch below must build its own message from
// specific fields rather than ever formatting err itself with %s, %v, or %w:
// doing so would splice the offending Error() string back in.
func redact(err error) error {
	if err == nil {
		return nil
	}
	// *url.Error is checked first, ahead of the context-sentinel check below:
	// net/http's client-Timeout error (and a transport's
	// ResponseHeaderTimeout) is an unexported *http.httpError that satisfies
	// errors.Is(err, context.DeadlineExceeded) while ALSO being wrapped in a
	// key-bearing *url.Error. Checking the sentinel first would return that
	// wrapped error verbatim -- URL, key, and all -- the moment anyone sets a
	// Timeout on the client passed to New or a ResponseHeaderTimeout on its
	// transport. Building the message from urlErr.Op and urlErr.Err (never
	// urlErr itself) strips the URL either way, and %w on urlErr.Err still
	// preserves errors.Is(err, context.DeadlineExceeded) /
	// errors.Is(err, context.Canceled) for callers, so nothing is lost by
	// checking this branch first.
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s request failed: %w", urlErr.Op, urlErr.Err)
	}
	// Context cancellation and deadline expiry carry no URL, so the sentinel
	// is safe to return as-is -- and must be, so errors.Is(err,
	// context.Canceled) still works for callers that need to distinguish a
	// shutdown from a real upstream failure. The SDK returns this as a bare
	// sentinel (see requestconfig.Execute's ctx.Err() check after the HTTP
	// call), never wrapped in *url.Error, so it reaches this branch rather
	// than the one above.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if code := statusOf(err); code != 0 {
		return fmt.Errorf("upstream returned status %d", code)
	}
	return errors.New("upstream request failed")
}

// statusOf reports the HTTP status an SDK error carries, or 0.
func statusOf(err error) int {
	var apiErr *oba.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// isClientError reports whether err is a 4xx from the upstream that means
// "this agency permanently has no vehicle feed" rather than a transient
// failure. 408 and 429 are excluded: both are the upstream asking the caller
// to slow down or retry, not a durable fact about the agency, and tolerating
// them would silently drop a rider's bus from the fleet on every rate-limited
// or slow request instead of surfacing the failure.
func isClientError(err error) bool {
	code := statusOf(err)
	if code == http.StatusRequestTimeout || code == http.StatusTooManyRequests {
		return false
	}
	return code >= 400 && code < 500
}
