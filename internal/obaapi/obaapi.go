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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
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

// StopArrivalsQuery selects arrivals-and-departures-for-stop's window
// around now, in minutes.
type StopArrivalsQuery struct {
	StopID        string
	MinutesBefore int64
	MinutesAfter  int64
}

// StopArrival is one arrivalsAndDepartures entry (design spec §2.4a). Times
// are epoch ms with 0 meaning absent. HasIdentity reports whether all five
// visit-identity fields (StopID, TripID, RouteID, ServiceDate, StopSequence)
// were present and well-typed upstream: the SDK zero-fills absent fields, and
// StopSequence 0 is a real value (the trip's first stop), so presence has to
// travel separately (design spec §2.4).
type StopArrival struct {
	StopID       string
	TripID       string
	RouteID      string
	ServiceDate  int64
	StopSequence int64
	HasIdentity  bool

	LastUpdateTime int64
	RouteShortName string // entry value, else references.routes shortName/longName
	TripHeadsign   string // entry value, else references.trips tripHeadsign
	Predicted      bool

	ScheduledArrivalTime   int64
	PredictedArrivalTime   int64
	ScheduledDepartureTime int64
	PredictedDepartureTime int64
}

// TripDetailsQuery identifies the trip instance a ghost bus report named,
// plus the report's own route/stop ids used to resolve display names.
type TripDetailsQuery struct {
	TripID      string
	ServiceDate int64  // epoch ms
	RouteID     string // report's route_identifier; "" = fall back to the trip reference's routeId
	StopID      string // report's stop_identifier; "" = no stop display block
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

	// ArrivalsAndDeparturesForStop lists every arrival/departure at a stop
	// inside the query window. ErrNotFound on a 404 or a null/absent body;
	// an empty list is not an error. ErrNotConfigured when the region has
	// no API key.
	ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q StopArrivalsQuery) ([]StopArrival, error)

	// TripDetails fetches trip-details for one trip instance and returns the
	// pruned spec-§2.6 snapshot document. ErrNotFound on a definitive miss
	// (404 or empty entry); ErrNotConfigured when the region has no API key.
	TripDetails(ctx context.Context, region regions.Region, q TripDetailsQuery) (json.RawMessage, error)
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

	// sdks memoizes one *oba.Client per (base URL, key) pair. oba.NewClient
	// builds out the SDK's full set of sub-service structs, and
	// ArrivalAndDeparture calls sdkFor on every invocation -- once per
	// pending alarm on the scheduler's 60-second sweep -- so without this a
	// region with N alarms rebuilds N identical clients every cycle. Fleet
	// already builds one and shares it across its agency fan-out; this gives
	// the per-alarm path the same treatment. The key is part of the cache
	// key, so a rotated API key lands on a fresh entry instead of serving a
	// stale client.
	sdkMu sync.Mutex
	sdks  map[string]*oba.Client // cacheKey -> client
}

// New builds a Client. defaultKey is the process-wide fallback used for any
// region that carries no key of its own; it may be empty.
func New(defaultKey string, httpClient *http.Client, logger *slog.Logger) Client {
	if logger == nil {
		logger = slog.Default()
	}
	// The key travels in the query string, so a followed redirect would leak
	// it to the redirect target via Referer; see httpx.NoRedirectClient.
	return &client{
		defaultKey: defaultKey,
		http:       httpx.NoRedirectClient(httpClient),
		logger:     logger,
		sdks:       make(map[string]*oba.Client),
	}
}

// sdkFor resolves the region's API key (falling back to the process-wide
// default) and builds an SDK client for its OBA server. ErrNotConfigured
// when neither source supplies a key; no request is attempted.
func (c *client) sdkFor(region regions.Region) (*oba.Client, error) {
	key := region.OBAAPIKey
	if key == "" {
		key = c.defaultKey
	}
	if key == "" {
		return nil, ErrNotConfigured
	}
	// NUL separates the fields so no base URL and key pair can collide with
	// a different pair by concatenation.
	cacheKey := region.OBABaseURL + "\x00" + key
	c.sdkMu.Lock()
	cached, ok := c.sdks[cacheKey]
	c.sdkMu.Unlock()
	if ok {
		return cached, nil
	}

	sdk := oba.NewClient(
		option.WithBaseURL(region.OBABaseURL),
		option.WithAPIKey(key),
		option.WithHTTPClient(c.http),
		option.WithRequestTimeout(perRequestTimeout),
		option.WithMaxRetries(maxRetries),
	)

	c.sdkMu.Lock()
	defer c.sdkMu.Unlock()
	// Re-check under the lock: two goroutines racing the same first lookup
	// must end up sharing one client, so the loser adopts the winner's
	// rather than overwriting it.
	if existing, ok := c.sdks[cacheKey]; ok {
		return existing, nil
	}
	c.sdks[cacheKey] = sdk
	return sdk, nil
}

func (c *client) Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error) {
	sdk, err := c.sdkFor(region)
	if err != nil {
		return nil, err
	}

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
			// The 200-null-body shape guarded in ArrivalAndDeparture (a
			// nil response with a nil error) would panic here too; treat
			// it as an empty feed for this agency.
			if list == nil {
				return nil
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
	sdk, err := c.sdkFor(region)
	if err != nil {
		return Departure{}, err
	}

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

	// The live Puget Sound server answers an unknown trip/service-date with
	// HTTP 200 and the literal body `null`; encoding/json unmarshals that
	// into a nil response pointer with a nil error, so this check must come
	// before any field access or the lookup panics.
	if resp == nil {
		return Departure{}, ErrNotFound
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

// jsonField is the subset of the SDK's internal apijson.Field that this
// package needs to tell "absent/null" apart from "present but zero". The SDK
// keeps apijson.Field in an internal package that cannot be imported, but
// every generated entry struct exposes one JSON metadata field per data
// field with these two methods, so a local interface reaches the same
// information without the import.
type jsonField interface {
	IsNull() bool
	IsInvalid() bool
}

func (c *client) ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q StopArrivalsQuery) ([]StopArrival, error) {
	sdk, err := c.sdkFor(region)
	if err != nil {
		return nil, err
	}
	params := oba.ArrivalAndDepartureListParams{}
	if q.MinutesBefore != 0 {
		params.MinutesBefore = oba.F(q.MinutesBefore)
	}
	if q.MinutesAfter != 0 {
		params.MinutesAfter = oba.F(q.MinutesAfter)
	}
	resp, err := sdk.ArrivalAndDeparture.List(ctx, q.StopID, params)
	if err != nil {
		if statusOf(err) == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("obaapi: arrivals-and-departures-for-stop in region %d: %w", region.ID, redact(err))
	}
	// Same `null` body hazard as ArrivalAndDeparture: a nil response with a
	// nil error, so this check precedes any field access.
	if resp == nil {
		return nil, ErrNotFound
	}
	routes := resp.Data.References.Routes
	trips := resp.Data.References.Trips
	entries := resp.Data.Entry.ArrivalsAndDepartures
	out := make([]StopArrival, 0, len(entries))
	for _, e := range entries {
		j := e.JSON
		present := func(f jsonField) bool { return !f.IsNull() && !f.IsInvalid() }
		shortName := e.RouteShortName
		if shortName == "" {
			for _, r := range routes {
				if r.ID == e.RouteID {
					shortName = r.ShortName
					if shortName == "" {
						shortName = r.LongName
					}
					break
				}
			}
		}
		headsign := e.TripHeadsign
		if headsign == "" {
			for _, tr := range trips {
				if tr.ID == e.TripID {
					headsign = tr.TripHeadsign
					break
				}
			}
		}
		out = append(out, StopArrival{
			StopID: e.StopID, TripID: e.TripID, RouteID: e.RouteID,
			ServiceDate: e.ServiceDate, StopSequence: e.StopSequence,
			HasIdentity: present(j.StopID) && present(j.TripID) && present(j.RouteID) &&
				present(j.ServiceDate) && present(j.StopSequence),
			LastUpdateTime: e.LastUpdateTime,
			RouteShortName: shortName, TripHeadsign: headsign,
			Predicted:              e.Predicted,
			ScheduledArrivalTime:   e.ScheduledArrivalTime,
			PredictedArrivalTime:   e.PredictedArrivalTime,
			ScheduledDepartureTime: e.ScheduledDepartureTime,
			PredictedDepartureTime: e.PredictedDepartureTime,
		})
	}
	return out, nil
}

// tripStatusKeys is OBACloud's STATUS_KEYS allow-list, verbatim: the
// forensic subset of a trip-details status block worth storing per report.
var tripStatusKeys = []string{
	"predicted", "vehicleId", "lastUpdateTime", "lastLocationUpdateTime",
	"scheduleDeviation", "phase", "serviceDate", "closestStop",
	"closestStopTimeOffset", "distanceAlongTrip", "totalDistanceAlongTrip",
	"lastKnownLocation", "position", "orientation", "activeTripId",
}

func (c *client) TripDetails(ctx context.Context, region regions.Region, q TripDetailsQuery) (json.RawMessage, error) {
	sdk, err := c.sdkFor(region)
	if err != nil {
		return nil, err // ErrNotConfigured
	}
	// includeSchedule stays at the server default (true) deliberately: the
	// schedule block is what pulls the queried stop into references.stops;
	// disabling it silently blanks the CSV's stop columns (design §2.7).
	resp, err := sdk.TripDetails.Get(ctx, q.TripID, oba.TripDetailGetParams{
		ServiceDate: oba.F(q.ServiceDate),
	})
	if err != nil {
		// Mirrors ArrivalAndDeparture: only a 404 is the upstream's
		// definitive "no such trip" signal. isClientError's broader 4xx
		// tolerance is Fleet's "agency permanently has no feed" concept and
		// would misclassify e.g. a 400 as a trip aging out.
		if statusOf(err) == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("obaapi: trip-details in region %d: %w", region.ID, redact(err))
	}
	// See ArrivalAndDeparture: the live server answers an unknown
	// trip/service-date with HTTP 200 and the literal body `null`, which
	// decodes to a nil response with a nil error. resp.JSON below would
	// panic on a nil resp, so this must come first.
	if resp == nil {
		return nil, ErrNotFound
	}
	return pruneTripSnapshot([]byte(resp.JSON.RawJSON()), q)
}

// pruneTripSnapshot builds the spec-§2.6 snapshot from the RAW response
// JSON. Raw, not the SDK's typed structs, deliberately: every typed status
// field is value-typed, so absence and zero are indistinguishable there --
// and a fabricated zero lastKnownLocation puts the vehicle at Null Island,
// which the CSV would render as kilometers of plausible garbage.
func pruneTripSnapshot(raw []byte, q TripDetailsQuery) (json.RawMessage, error) {
	var doc struct {
		CurrentTime int64 `json:"currentTime"`
		Data        struct {
			Entry      map[string]any `json:"entry"`
			References struct {
				Trips  []map[string]any `json:"trips"`
				Routes []map[string]any `json:"routes"`
				Stops  []map[string]any `json:"stops"`
			} `json:"references"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, ErrNotFound // unparseable body: definitive, not transient
	}
	if len(doc.Data.Entry) == 0 {
		return nil, ErrNotFound // 200-with-null-entry, same as ArrivalAndDeparture
	}
	status := map[string]any{}
	if entryStatus, ok := doc.Data.Entry["status"].(map[string]any); ok {
		for _, k := range tripStatusKeys {
			if v, present := entryStatus[k]; present {
				status[k] = v
			}
		}
	}
	snap := map[string]any{"current_time": doc.CurrentTime, "status": status}
	if display := tripDisplayBlock(doc.Data.References.Trips, doc.Data.References.Routes, doc.Data.References.Stops, q); len(display) > 0 {
		snap["display"] = display
	}
	return json.Marshal(snap)
}

// findReference returns the entry in list whose "id" equals id, or nil.
// An empty id never matches -- absent identifiers stay absent.
func findReference(list []map[string]any, id string) map[string]any {
	if id == "" {
		return nil
	}
	for _, m := range list {
		if m["id"] == id {
			return m
		}
	}
	return nil
}

// resolveRouteID prefers the report's own route identifier, falling back
// to the trip reference's routeId (OBACloud's display_block precedence).
func resolveRouteID(reported string, trip map[string]any) string {
	if reported != "" || trip == nil {
		return reported
	}
	v, ok := trip["routeId"].(string)
	if !ok {
		return ""
	}
	return v
}

// putDisplay copies src[srcKey] into display under key when it is present
// and non-empty -- absent stays absent (OBACloud's .compact).
func putDisplay(display, src map[string]any, key, srcKey string) {
	if src == nil {
		return
	}
	if v, ok := src[srcKey]; ok && v != nil && v != "" {
		display[key] = v
	}
}

// tripDisplayBlock resolves human-readable names out of the references:
// best-effort, absent keys simply missing (OBACloud's .compact). The DB
// stores only raw identifiers; without this the CSV could never show route
// names, headsigns, or stop coordinates.
func tripDisplayBlock(trips, routes, stops []map[string]any, q TripDetailsQuery) map[string]any {
	display := map[string]any{}
	trip := findReference(trips, q.TripID)
	putDisplay(display, trip, "headsign", "tripHeadsign")
	route := findReference(routes, resolveRouteID(q.RouteID, trip))
	putDisplay(display, route, "route_short_name", "shortName")
	putDisplay(display, route, "route_long_name", "longName")
	putDisplay(display, route, "route_type", "type")
	stop := findReference(stops, q.StopID)
	putDisplay(display, stop, "stop_name", "name")
	putDisplay(display, stop, "stop_lat", "lat")
	putDisplay(display, stop, "stop_lon", "lon")
	return display
}

// agencies fetches the region's agencies. Names live in the response's
// references block rather than on the list entries (which carry only ids and
// bounding boxes), so one call yields both.
func (c *client) agencies(ctx context.Context, sdk *oba.Client) ([]Agency, error) {
	resp, err := sdk.AgenciesWithCoverage.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("obaapi: agencies with coverage: %w", redact(err))
	}
	// See ArrivalAndDeparture: a 200 with a literal null body decodes to a
	// nil response with a nil error. No agencies is the honest reading.
	if resp == nil {
		return nil, nil
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
