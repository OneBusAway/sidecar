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

// redact strips any URL-bearing text from an upstream error. The OBA key
// travels in the URL as a query parameter, and *url.Error embeds the full
// URL in its message. An error logged verbatim writes the secret to disk,
// undoing the care taken to keep it out of every JSON response.
func redact(err error) error {
	if err == nil {
		return nil
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

// isClientError reports whether err is a 4xx from the upstream.
func isClientError(err error) bool {
	code := statusOf(err)
	return code >= 400 && code < 500
}
