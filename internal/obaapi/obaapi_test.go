package obaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

const sentinelKey = "SENTINEL-API-KEY-do-not-log"

// obaServer stands in for a region's OBA REST API server. vehicleStatus lets
// a test make one agency's call fail with a specific code.
type obaServer struct {
	*httptest.Server
	agencyCalls   atomic.Int64
	vehicleStatus map[string]int
}

func newOBAServer(t *testing.T, agencies []struct{ ID, Name string }, vehicles map[string][]string) *obaServer {
	t.Helper()
	s := &obaServer{vehicleStatus: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/where/agencies-with-coverage.json", func(w http.ResponseWriter, r *http.Request) {
		s.agencyCalls.Add(1)
		list := []map[string]any{}
		refs := []map[string]any{}
		for _, a := range agencies {
			list = append(list, map[string]any{
				"agencyId": a.ID, "lat": 47.6, "lon": -122.3, "latSpan": 0.1, "lonSpan": 0.1,
			})
			refs = append(refs, map[string]any{"id": a.ID, "name": a.Name})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": refs, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	mux.HandleFunc("/api/where/vehicles-for-agency/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/where/vehicles-for-agency/"), ".json")
		if code, ok := s.vehicleStatus[id]; ok {
			w.WriteHeader(code)
			return
		}
		list := []map[string]any{}
		for _, v := range vehicles[id] {
			list = append(list, map[string]any{
				"vehicleId": v, "lastUpdateTime": 0, "lastLocationUpdateTime": 0,
			})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func writeOBA(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "version": 2, "currentTime": 0, "text": "OK", "data": data,
	})
}

func testRegion(baseURL, key string) regions.Region {
	return regions.Region{ID: 1, Name: "Test", OBABaseURL: baseURL, OBAAPIKey: key, Active: true}
}

// errChainContains reports whether needle appears in the Error() text of err
// or anything err wraps. A leak fixed at the top frame but reintroduced by a
// %w further down the chain would be invisible to a check of err.Error()
// alone only by the accident of how the outer message happens to be built;
// walking the chain explicitly does not depend on that accident.
func errChainContains(err error, needle string) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), needle) {
			return true
		}
	}
	return false
}

func TestFleetResolvesAgencyNamesFromReferences(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro Transit"}, {"3", "Community Transit"}},
		map[string][]string{"1": {"1_4361", "1_4362"}, "3": {"3_99"}},
	)

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}

	want := []Vehicle{
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4361"},
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4362"},
		{AgencyID: "3", AgencyName: "Community Transit", VehicleID: "3_99"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vehicles, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vehicle %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Parallel completion order is not deterministic, so the result must be
// reassembled by index rather than by arrival. Each run is checked against a
// pinned expected order, not against an earlier run: comparing only to run 1
// would let a deterministic-but-wrong permutation pass, and would only catch
// a completion-order bug on whichever runs happen to race unevenly.
func TestFleetOrderIsDeterministic(t *testing.T) {
	agencies := []struct{ ID, Name string }{}
	vehicles := map[string][]string{}
	var want []Vehicle
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		agencies = append(agencies, struct{ ID, Name string }{id, "Agency " + id})
		vehicles[id] = []string{id + "_a", id + "_b"}
		want = append(want,
			Vehicle{AgencyID: id, AgencyName: "Agency " + id, VehicleID: id + "_a"},
			Vehicle{AgencyID: id, AgencyName: "Agency " + id, VehicleID: id + "_b"},
		)
	}
	srv := newOBAServer(t, agencies, vehicles)
	c := New("", srv.Client(), slog.New(slog.DiscardHandler))

	for i := 0; i < 6; i++ {
		got, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
		if err != nil {
			t.Fatalf("run %d: Fleet: %v", i, err)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d vehicles, want %d: %+v", i, len(got), len(want), got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("run %d vehicle %d = %+v, want %+v", i, j, got[j], want[j])
			}
		}
	}
}

// An agency with no realtime feed answers 4xx forever. Failing the whole
// fetch would brick vehicle search for the region permanently.
func TestFleetTolerates4xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "NoRealtime"}},
		map[string][]string{"1": {"1_1"}},
	)
	srv.vehicleStatus["2"] = http.StatusNotFound

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != "1_1" {
		t.Errorf("Fleet = %+v, want just 1_1", got)
	}
}

// A 5xx is a real failure: caching a fleet with an agency silently missing
// tells every rider on its routes that their bus does not exist.
func TestFleetFailsOn5xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "Broken"}},
		map[string][]string{"1": {"1_1"}, "2": {"2_2"}},
	)
	srv.vehicleStatus["2"] = http.StatusInternalServerError

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 500")
	}
	// This is the one path that actually puts the key on the wire in a
	// non-2xx response: the SDK's error formats as a string containing the
	// full request URL, key included. A redact bug here is invisible to
	// TestErrorsDoNotLeakTheKey, which only exercises a transport failure.
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

// 408 and 429 are transient, not a durable "this agency has no feed" fact,
// so they must fail the fetch like a 5xx rather than being tolerated like a
// 404. Tolerating them would silently drop a rate-limited agency's vehicles
// from the fleet.
func TestFleetFailsOn429FromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "RateLimited"}},
		map[string][]string{"1": {"1_1"}, "2": {"2_2"}},
	)
	srv.vehicleStatus["2"] = http.StatusTooManyRequests

	if _, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey)); err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 429")
	}
}

// Cancellation must survive redact so a caller can tell a shutdown apart
// from a real upstream failure -- but the SDK returns this as a bare
// sentinel, never wrapped in *url.Error, so it needs its own path through
// redact rather than falling out of the *url.Error branch.
func TestFleetPreservesContextCancellation(t *testing.T) {
	srv := newOBAServer(t, []struct{ ID, Name string }{{"1", "Metro"}}, map[string][]string{"1": {"1_1"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(ctx, testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded with an already-cancelled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fleet err = %v, want context.Canceled in the chain", err)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestFleetWithoutKeyMakesNoRequest(t *testing.T) {
	srv := newOBAServer(t, []struct{ ID, Name string }{{"1", "Metro"}}, map[string][]string{"1": {"1_1"}})

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Fleet err = %v, want ErrNotConfigured", err)
	}
	if srv.agencyCalls.Load() != 0 {
		t.Errorf("made %d requests, want 0", srv.agencyCalls.Load())
	}
}

func TestRegionKeyOverridesDefault(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Query().Get("key"))
		writeOBA(w, map[string]any{
			"list":       []any{},
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	}))
	defer srv.Close()

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, "region-key"))
	if got, _ := seen.Load().(string); got != "region-key" {
		t.Errorf("key = %q, want region-key", got)
	}

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if got, _ := seen.Load().(string); got != "process-default" {
		t.Errorf("key = %q, want process-default", got)
	}
}

// The SDK puts the key in the query string, and *url.Error embeds the full
// URL. An error logged verbatim would write the secret to disk.
func TestErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // closed immediately: every request is a transport failure

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	_, err := New("", srv.Client(), logger).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a closed server, want an error")
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
	if strings.Contains(logs.String(), sentinelKey) {
		t.Errorf("log output leaks the API key: %s", logs.String())
	}
}

// The SDK puts the key in the query string, and the default *http.Client sets
// Referer to the previous request's full URL -- query included -- on any
// redirect it follows. New must refuse to follow redirects so a region whose
// OBABaseURL (sourced from the remote regions directory, with no guarantee of
// being well-behaved) points at a redirecting server never hands the key to
// the redirect target.
//
// Mirrors weather's TestPirateWeatherDoesNotFollowRedirectsWithReferer.
func TestFleetDoesNotFollowRedirectsWithReferer(t *testing.T) {
	var targetHit bool
	var gotReferer string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		gotReferer = r.Header.Get("Referer")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	_, err := New("", origin.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(origin.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a redirecting server, want an error (a 302 is not a 200)")
	}
	if targetHit {
		t.Errorf("redirect target was contacted; Referer = %q", gotReferer)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	if strings.Contains(gotReferer, sentinelKey) {
		t.Errorf("Referer header leaked the key: %q", gotReferer)
	}
}

// redact checks *url.Error before the context-cancellation sentinel. net/http's
// client Timeout produces an error that is BOTH wrapped in a key-bearing
// *url.Error AND satisfies errors.Is(err, context.DeadlineExceeded) -- so a
// client whose Timeout elapses mid-request is the reachable case the branch
// order guards against. This pins both halves: the key must never appear, and
// errors.Is(err, context.DeadlineExceeded) must still hold for callers.
func TestFleetTimeoutDoesNotLeakKeyAndPreservesDeadlineExceeded(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	httpClient := &http.Client{Timeout: 20 * time.Millisecond}

	_, err := New("", httpClient, slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a server that never responds, want a timeout error")
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fleet err = %v, want context.DeadlineExceeded in the chain", err)
	}
}

// New must apply usable defaults for a nil http.Client and a nil logger --
// the latter matters because the 4xx-tolerance path calls c.logger.Warn, and
// a nil *slog.Logger there panics rather than merely misbehaving.
func TestNewAppliesDefaultsWithoutPanicking(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "NoRealtime"}},
		map[string][]string{"1": {"1_1"}},
	)
	srv.vehicleStatus["2"] = http.StatusNotFound

	got, err := New("", nil, nil).Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != "1_1" {
		t.Errorf("Fleet = %+v, want just 1_1", got)
	}
}

// TestFleetWarnsWhenEveryAgencyDeclines pins the observability of the case
// where every agency answers 4xx: the fleet is empty and stays cached, so an
// operator's only signal that a region has gone dark is this log line. The
// status code is unchanged (an empty fleet is a legitimate 200), which is why
// the log has to carry the weight.
func TestFleetWarnsWhenEveryAgencyDeclines(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "Other"}},
		map[string][]string{},
	)
	srv.vehicleStatus["1"] = http.StatusNotFound
	srv.vehicleStatus["2"] = http.StatusNotFound

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	fleet, err := New("", srv.Client(), logger).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(fleet) != 0 {
		t.Fatalf("fleet = %+v, want empty", fleet)
	}
	if !strings.Contains(logs.String(), "every agency declined") {
		t.Errorf("no all-declined warning logged; operator would have no signal:\n%s", logs.String())
	}
}

// arrivalServer stands in for a region's arrival-and-departure-for-stop
// endpoint. status, when nonzero, makes every request answer with that bare
// HTTP status instead of a JSON body -- for the 404/5xx tests. Otherwise it
// serves entry/routes as a normal OBA envelope and records the last
// request's query string so tests can assert on exactly which parameters
// were sent.
type arrivalServer struct {
	*httptest.Server
	calls     atomic.Int64
	lastQuery atomic.Value // url.Values
	status    int
	entry     map[string]any
	routes    []map[string]any
	// nullBody serves a 200 whose entire body is the literal JSON `null` --
	// the shape the live Puget Sound server produces for an unknown
	// trip/service-date pair, which the SDK decodes into a nil response
	// with a nil error.
	nullBody bool
}

func newArrivalServer(t *testing.T, status int, entry map[string]any, routes []map[string]any) *arrivalServer {
	t.Helper()
	s := &arrivalServer{status: status, entry: entry, routes: routes}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/where/arrival-and-departure-for-stop/", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastQuery.Store(r.URL.Query())
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		if s.nullBody {
			w.Header().Set("Content-Type", "application/json")
			if _, err := w.Write([]byte("null")); err != nil {
				t.Errorf("write null body: %v", err)
			}
			return
		}
		routes := s.routes
		if routes == nil {
			routes = []map[string]any{}
		}
		writeOBA(w, map[string]any{
			"entry":      s.entry,
			"references": map[string]any{"agencies": []any{}, "routes": routes, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *arrivalServer) query() url.Values {
	v, _ := s.lastQuery.Load().(url.Values)
	return v
}

func TestArrivalAndDeparture_MapsEntry(t *testing.T) {
	entry := map[string]any{
		"tripId":                 "1_12345",
		"routeId":                "1_100224",
		"routeShortName":         "44",
		"tripHeadsign":           "Ballard",
		"scheduledDepartureTime": int64(1700000000000),
		"predictedDepartureTime": int64(1700000060000),
		"predicted":              true,
	}
	srv := newArrivalServer(t, 0, entry, nil)

	seq := int64(5)
	q := DepartureQuery{
		StopID:       "1_100",
		TripID:       "1_12345",
		ServiceDate:  1699999999000,
		VehicleID:    "1_9999",
		StopSequence: &seq,
	}
	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey), q)
	if err != nil {
		t.Fatalf("ArrivalAndDeparture: %v", err)
	}
	want := Departure{
		RouteShortName:         "44",
		TripHeadsign:           "Ballard",
		ScheduledDepartureTime: 1700000000000,
		PredictedDepartureTime: 1700000060000,
		Predicted:              true,
	}
	if got != want {
		t.Errorf("ArrivalAndDeparture = %+v, want %+v", got, want)
	}

	qv := srv.query()
	if got := qv.Get("tripId"); got != "1_12345" {
		t.Errorf("tripId = %q, want 1_12345", got)
	}
	if got := qv.Get("serviceDate"); got != "1699999999000" {
		t.Errorf("serviceDate = %q, want 1699999999000", got)
	}
	if got := qv.Get("vehicleId"); got != "1_9999" {
		t.Errorf("vehicleId = %q, want 1_9999", got)
	}
	if got := qv.Get("stopSequence"); got != "5" {
		t.Errorf("stopSequence = %q, want 5", got)
	}
}

func TestArrivalAndDeparture_RouteShortNameFromReferences(t *testing.T) {
	// The entry carries no routeShortName, so the label has to come out of
	// references -- and a route that has only a longName still needs to
	// produce something a rider can read rather than an empty label.
	for _, tc := range []struct {
		name  string
		route map[string]any
		want  string
	}{
		{"short name from references", map[string]any{"id": "1_100224", "agencyId": "1", "type": 3, "shortName": "44"}, "44"},
		{"falls back to long name", map[string]any{"id": "1_100224", "agencyId": "1", "type": 3, "longName": "Ballard Local"}, "Ballard Local"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{"tripId": "1_1", "routeId": "1_100224", "routeShortName": ""}
			srv := newArrivalServer(t, 0, entry, []map[string]any{tc.route})

			got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
				ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
					DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
			if err != nil {
				t.Fatalf("ArrivalAndDeparture: %v", err)
			}
			if got.RouteShortName != tc.want {
				t.Errorf("RouteShortName = %q, want %q", got.RouteShortName, tc.want)
			}
		})
	}
}

// A 200 whose entry has no tripId is OBA's other way of saying "nothing
// here" (alongside a bare 404) and must count toward the alarm reaper's
// 3-strike streak the same way. The fake produces it simply by omitting
// tripId from the entry map, exactly like a real "no such trip" response.
// TestArrivalAndDeparture_NullBodyIsErrNotFound pins the third "not found"
// shape observed on the live Puget Sound server: HTTP 200 with the literal
// body `null`. encoding/json unmarshals that into a nil response pointer
// with a nil error, so without an explicit nil check the lookup panics --
// which is how this case was found, panicking the real server during
// end-to-end exercise.
func TestArrivalAndDeparture_NullBodyIsErrNotFound(t *testing.T) {
	t.Parallel()
	srv := newArrivalServer(t, 0, nil, nil)
	srv.nullBody = true

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
			DepartureQuery{StopID: "1_570", TripID: "1_604370", ServiceDate: 1755673200000})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestArrivalAndDeparture_EmptyEntryIsErrNotFound(t *testing.T) {
	srv := newArrivalServer(t, 0, map[string]any{}, nil)

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
			DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestArrivalAndDeparture_404IsErrNotFound(t *testing.T) {
	srv := newArrivalServer(t, http.StatusNotFound, nil, nil)

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
			DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A 5xx is a transient upstream failure, not "this trip doesn't exist" -- it
// must not satisfy errors.Is(err, ErrNotFound), or the alarm reaper would
// count an outage as trips aging out and delete alarms that are still live.
func TestArrivalAndDeparture_5xxIsNotErrNotFound(t *testing.T) {
	srv := newArrivalServer(t, http.StatusInternalServerError, nil, nil)

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
			DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
	if err == nil {
		t.Fatal("ArrivalAndDeparture succeeded, want an error on 500")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want NOT ErrNotFound for a 5xx", err)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestArrivalAndDeparture_OmitsOptionalParams(t *testing.T) {
	entry := map[string]any{"tripId": "1_1", "routeShortName": "44"}
	srv := newArrivalServer(t, 0, entry, nil)

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, sentinelKey),
			DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
	if err != nil {
		t.Fatalf("ArrivalAndDeparture: %v", err)
	}

	qv := srv.query()
	if qv.Has("vehicleId") {
		t.Errorf("vehicleId present in query, want omitted: %v", qv)
	}
	if qv.Has("stopSequence") {
		t.Errorf("stopSequence present in query, want omitted: %v", qv)
	}
}

func TestArrivalAndDeparture_WithoutKeyMakesNoRequest(t *testing.T) {
	srv := newArrivalServer(t, 0, map[string]any{"tripId": "1_1"}, nil)

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		ArrivalAndDeparture(context.Background(), testRegion(srv.URL, ""),
			DepartureQuery{StopID: "1_100", TripID: "1_1", ServiceDate: 1})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if srv.calls.Load() != 0 {
		t.Errorf("made %d requests, want 0", srv.calls.Load())
	}
}

// TestSDKClientCacheKeyedOnBaseURLAndKey pins the memoization in sdkFor.
// The cache exists because ArrivalAndDeparture builds a client on every call
// and the alarm scheduler calls it once per pending alarm each minute; but a
// cache keyed too loosely is worse than no cache, since it would route one
// region's request at another region's server or with a rotated-away key.
// Both halves of the key are asserted, and so is the memoization itself --
// without the last check the cache could quietly stop caching.
func TestSDKClientCacheKeyedOnBaseURLAndKey(t *testing.T) {
	t.Parallel()
	c := New("", nil, slog.New(slog.DiscardHandler)).(*client)

	a1, err := c.sdkFor(testRegion("https://a.example", "key-a"))
	if err != nil {
		t.Fatalf("sdkFor(a): %v", err)
	}
	a2, err := c.sdkFor(testRegion("https://a.example", "key-a"))
	if err != nil {
		t.Fatalf("sdkFor(a) again: %v", err)
	}
	if a1 != a2 {
		t.Error("same base URL and key returned two clients; the cache is not caching")
	}

	b, err := c.sdkFor(testRegion("https://b.example", "key-a"))
	if err != nil {
		t.Fatalf("sdkFor(b): %v", err)
	}
	if b == a1 {
		t.Error("different base URLs shared one client; requests would go to the wrong OBA server")
	}

	rotated, err := c.sdkFor(testRegion("https://a.example", "key-a2"))
	if err != nil {
		t.Fatalf("sdkFor(rotated): %v", err)
	}
	if rotated == a1 {
		t.Error("a rotated API key served the client built with the old key")
	}
}

// tripDetailsServer stands in for a region's trip-details endpoint. status,
// when not http.StatusOK, makes every request answer with that bare HTTP
// status (and whatever body was configured, mirroring a real error body);
// otherwise it serves body verbatim as the JSON envelope.
type tripDetailsServer struct {
	*httptest.Server
	calls  atomic.Int64
	status int
	body   string
}

func newTripDetailsFake(t *testing.T, status int, body string) (Client, regions.Region) {
	t.Helper()
	s := &tripDetailsServer{status: status, body: body}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/where/trip-details/", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if s.status != http.StatusOK {
			w.WriteHeader(s.status)
		}
		if _, err := w.Write([]byte(s.body)); err != nil {
			t.Errorf("write body: %v", err)
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)

	client := New("", s.Client(), slog.New(slog.DiscardHandler))
	return client, testRegion(s.URL, sentinelKey)
}

func TestTripDetailsPrunesRawJSON(t *testing.T) {
	// Serve a realistic trip-details document. Crucially the status block
	// OMITS lastKnownLocation and includes an extra key (nextStop) that the
	// allow-list must drop; the references resolve the trip, its route, and
	// the queried stop.
	body := `{
	  "code": 200, "currentTime": 1756000123456,
	  "data": {
	    "entry": {
	      "tripId": "1_604370",
	      "status": {
	        "predicted": true, "vehicleId": "1_4361", "phase": "in_progress",
	        "scheduleDeviation": 60, "nextStop": "1_999",
	        "position": {"lat": 47.61, "lon": -122.34}
	      }
	    },
	    "references": {
	      "trips":  [{"id": "1_604370", "routeId": "1_100044", "tripHeadsign": "Ballard"}],
	      "routes": [{"id": "1_100044", "shortName": "44", "longName": "Ballard - Montlake", "type": 3}],
	      "stops":  [{"id": "1_570", "name": "15th Ave NW & NW Market St", "lat": 47.668, "lon": -122.376}]
	    }
	  }
	}`
	client, region := newTripDetailsFake(t, http.StatusOK, body)

	raw, err := client.TripDetails(context.Background(), region, TripDetailsQuery{
		TripID: "1_604370", ServiceDate: 1754809200000, StopID: "1_570",
	})
	if err != nil {
		t.Fatalf("TripDetails: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	status, _ := snap["status"].(map[string]any)
	if status["phase"] != "in_progress" || status["vehicleId"] != "1_4361" {
		t.Errorf("status block = %v", status)
	}
	// Absent means absent: keys the upstream omitted must not appear as
	// zero values (a defaulted lastKnownLocation puts the bus at Null
	// Island and poisons the CSV distance column).
	if _, ok := status["lastKnownLocation"]; ok {
		t.Error("lastKnownLocation fabricated from a zero value")
	}
	if _, ok := status["nextStop"]; ok {
		t.Error("nextStop survived the STATUS_KEYS allow-list")
	}
	display, _ := snap["display"].(map[string]any)
	if display["route_short_name"] != "44" || display["headsign"] != "Ballard" ||
		display["stop_name"] != "15th Ave NW & NW Market St" {
		t.Errorf("display block = %v", display)
	}
	if display["stop_lat"] != 47.668 {
		t.Errorf("stop_lat = %v", display["stop_lat"])
	}
	if ct, ok := snap["current_time"].(float64); !ok || int64(ct) != 1756000123456 {
		t.Errorf("current_time = %v", snap["current_time"])
	}
}

func TestTripDetailsEmptyEntryIsNotFound(t *testing.T) {
	// Upstream 200 with a null/empty entry is a definitive miss, same
	// contract as ArrivalAndDeparture's 200-with-null handling (0bb74c2).
	client, region := newTripDetailsFake(t, http.StatusOK, `{"code":200,"data":{"entry":null,"references":{}}}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTripDetails404IsNotFound(t *testing.T) {
	client, region := newTripDetailsFake(t, http.StatusNotFound, `{}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTripDetails500IsTransient(t *testing.T) {
	client, region := newTripDetailsFake(t, http.StatusInternalServerError, `{}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want a transient (unclassified) error", err)
	}
}

func TestTripDetailsNoKeyIsNotConfigured(t *testing.T) {
	// Region with no key and no process default: same contract as the
	// existing calls. Reuse the file's existing no-key client fixture.
	client := New("", http.DefaultClient, slog.New(slog.DiscardHandler))
	_, err := client.TripDetails(context.Background(), regions.Region{ID: 1, OBABaseURL: "https://example.com"}, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// TestSDKClientCacheSendsToTheRightServer is the behavioral counterpart: the
// pointer-identity test above cannot catch a client that is distinct but
// still configured for the wrong host.
func TestSDKClientCacheSendsToTheRightServer(t *testing.T) {
	t.Parallel()
	newServer := func(hits *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			writeOBA(w, map[string]any{
				"list":       []any{},
				"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
			})
		}))
	}
	var hitsA, hitsB atomic.Int64
	srvA, srvB := newServer(&hitsA), newServer(&hitsB)
	defer srvA.Close()
	defer srvB.Close()

	c := New("shared-key", srvA.Client(), slog.New(slog.DiscardHandler))
	// Region A first, so a cache keyed only on the key would hand region B
	// the client already built for A.
	_, _ = c.Fleet(context.Background(), testRegion(srvA.URL, "shared-key"))
	beforeB := hitsB.Load()
	_, _ = c.Fleet(context.Background(), testRegion(srvB.URL, "shared-key"))

	if hitsB.Load() == beforeB {
		t.Error("region B's server saw no request; its lookup went to region A's server")
	}
}
