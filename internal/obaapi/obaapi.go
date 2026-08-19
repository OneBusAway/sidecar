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
	"time"

	oba "github.com/OneBusAway/go-sdk"
	"github.com/OneBusAway/go-sdk/option"
	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// ErrNotConfigured means neither the region nor the process supplied an API
// key, so no request was attempted.
var ErrNotConfigured = errors.New("obaapi: region has no API key")

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

// Client reads the OBA REST API for one region at a time. Implementations
// must be safe for concurrent use.
type Client interface {
	// Fleet returns every vehicle currently reported across every agency with
	// coverage in the region, in agencies-with-coverage order then each
	// agency's own response order.
	Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error)
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
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &client{defaultKey: defaultKey, http: httpClient, logger: logger}
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
	// Context cancellation and deadline expiry carry no URL, so the sentinel
	// is safe to return as-is -- and must be, so errors.Is(err,
	// context.Canceled) still works for callers that need to distinguish a
	// shutdown from a real upstream failure. This must come before the
	// *url.Error and status checks below: the SDK returns this as a bare
	// sentinel (see requestconfig.Execute's ctx.Err() check after the HTTP
	// call), never wrapped in *url.Error, so it would otherwise fall through
	// to the generic message and lose its identity.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s request failed: %w", urlErr.Op, urlErr.Err)
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
