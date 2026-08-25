# iOS Live Activities Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement spec §6 Live Activities: register/delete endpoints, a once-a-minute updater that pushes ActivityKit content-state via gorush, and feedback-driven pruning.

**Architecture:** A new `internal/liveactivities` domain package (types, `Repository`, pure content-state builder, `Updater` loop) shaped like `internal/alarms`; a narrow `push.LiveActivitySender` implemented by `push.Gorush`; a new `obaapi.ArrivalsAndDeparturesForStop`; a sqlite migration + sqlc queries + adapter + `storetest` conformance; two `httpapi` routes and a Live Activity branch in the gorush feedback webhook; wiring in `cmd/sidecar/main.go`.

**Tech Stack:** Go (see `mise.toml`), sqlc + goose over modernc SQLite, `github.com/OneBusAway/go-sdk` v1.15.0, gorush 1.22.0, `golang.org/x/sync/errgroup`.

**Spec:** `docs/superpowers/specs/2026-08-24-live-activities-design.md` (this plan argues from it; cite it in code comments as "design spec §N"). Normative behavior: `specification/specification.md` §6.

## Global Constraints

- `time.Now`/`time.Local` are banned outside `cmd/` and `_test.go` (forbidigo). `storetest` is not a test file: derive instants from its fixed `base`.
- Every timestamp column is `INTEGER` epoch **seconds**; `service_date` is epoch **milliseconds** (OBA data), also `INTEGER`.
- sqlc: never mix `sqlc.arg()`/`@name` with bare `?` in one query. Run `make generate` after touching `.sql` and commit `gen/`.
- `revive` requires doc comments on every exported identifier and package. `nolint` needs a linter name and reason.
- Nil `Deps` field ⇒ routes not registered. Store errors that can embed a token are logged only via `sanitizeToken`.
- Never log push tokens or gorush response bodies.
- Every test must be shown to fail under mutation. Timestamp assertions must pass under `make test-tz`.
- Run `make check` before finishing (needs `make web` once for the adminui embed test).
- Constants (design spec §5): `Lifetime = 8h`, `KeepaliveInterval = 55s`, `MaxConsecutiveFailures = 3`, `StaleAfter = 10m`, `DismissAfterEnd = 15m`, `LookbackMinutes = 5`, `LookaheadMinutes = 120`, `MaxArrivals = 3`, `StopCacheTTL = 55s`, `StopFetchBudget = 6s`, `checkConcurrency = 8`.

## File map

| File | Responsibility |
|---|---|
| `internal/push/push.go` | `LiveActivityPush`, `LiveActivitySender` (Task 1) |
| `internal/push/gorush.go` | `(*Gorush).SendLiveActivity`, shared `post` (Task 1) |
| `internal/obaapi/obaapi.go` | `StopArrivalsQuery`, `StopArrival`, `Client.ArrivalsAndDeparturesForStop` (Task 2) |
| `internal/liveactivities/liveactivities.go` | package doc, `LiveActivity`, `NewLiveActivity`, `Repository`, errors, constants (Task 3) |
| `internal/liveactivities/contentstate.go` | `ContentState`, `ArrivalInfo`, `BuildContentState`, `Changed` (Task 3) |
| `internal/liveactivities/testdata/live_activity_content_state.json` | iOS fixture, verbatim (Task 3) |
| `internal/liveactivities/updater.go` | `Updater`, `ArrivalsSource` (Task 5) |
| `internal/store/sqlite/migrations/00008_live_activities.sql`, `queries/liveactivities.sql`, `liveactivities.go`, `store.go` | storage (Task 4) |
| `internal/store/storetest/liveactivitytest.go` | conformance suite (Task 4) |
| `internal/httpapi/liveactivities.go`, `router.go`, `feedback.go` | HTTP + feedback (Tasks 6, 7) |
| `cmd/sidecar/main.go`, `README.md`, `CLAUDE.md` | wiring + docs (Task 8) |

---

### Task 1: `push.LiveActivitySender` and `Gorush.SendLiveActivity`

**Files:**
- Modify: `internal/push/push.go`
- Modify: `internal/push/gorush.go`
- Test: `internal/push/gorush_test.go`

**Interfaces:**
- Produces:
  ```go
  type LiveActivityPush struct {
      Token         string
      Sandbox       bool
      Event         string    // "update" | "end"
      ContentState  any       // marshals to the §6.2 object
      Timestamp     time.Time // required; epoch seconds on the wire
      StaleDate     time.Time // zero = omitted
      DismissalDate time.Time // zero = omitted
  }
  type LiveActivitySender interface { SendLiveActivity(ctx context.Context, p LiveActivityPush) error }
  func (g *Gorush) SendLiveActivity(ctx context.Context, p LiveActivityPush) error
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/push/gorush_test.go`:

```go
func TestGorushSendLiveActivityPostsExpectedJSON(t *testing.T) {
	server, captured := newCaptureServer(t)
	g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())

	ts := time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)
	err := g.SendLiveActivity(context.Background(), LiveActivityPush{
		Token:     "la-tok",
		Sandbox:   true,
		Event:     "update",
		ContentState: map[string]any{"arrivals": []any{}},
		Timestamp: ts,
		StaleDate: ts.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SendLiveActivity: %v", err)
	}
	got := captured()
	want := map[string]any{
		"tokens":        []any{"la-tok"},
		"platform":      float64(1),
		"push_type":     "liveactivity",
		"priority":      "high",
		"topic":         "org.onebusaway.iphone.push-type.liveactivity",
		"development":   true,
		"event":         "update",
		"content-state": map[string]any{"arrivals": []any{}},
		"timestamp":     float64(ts.Unix()),
		"stale-date":    float64(ts.Add(10 * time.Minute).Unix()),
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	for _, absent := range []string{"dismissal-date", "message", "title", "data"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s should be omitted from an update push, got %#v", absent, got[absent])
		}
	}
}

func TestGorushSendLiveActivityEndCarriesDismissalDateOnly(t *testing.T) {
	server, captured := newCaptureServer(t)
	g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())
	ts := time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)
	err := g.SendLiveActivity(context.Background(), LiveActivityPush{
		Token: "la-tok", Event: "end", ContentState: map[string]any{"arrivals": []any{}},
		Timestamp: ts, DismissalDate: ts.Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("SendLiveActivity: %v", err)
	}
	got := captured()
	if got["dismissal-date"] != float64(ts.Add(15*time.Minute).Unix()) {
		t.Errorf("dismissal-date = %#v", got["dismissal-date"])
	}
	if _, ok := got["stale-date"]; ok {
		t.Errorf("stale-date should be omitted on end, got %#v", got["stale-date"])
	}
	if _, ok := got["development"]; ok {
		t.Errorf("development should be omitted when Sandbox is false")
	}
}

func TestGorushSendLiveActivityRejectsEmptyTopicWithoutSending(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	g := NewGorush(server.URL, "", server.Client())
	err := g.SendLiveActivity(context.Background(), LiveActivityPush{
		Token: "la-tok", Event: "update", ContentState: map[string]any{}, Timestamp: time.Unix(1, 0),
	})
	if err == nil {
		t.Fatal("expected error for empty APNs topic")
	}
	if calls.Load() != 0 {
		t.Errorf("gorush was called %d times; an empty topic must not send", calls.Load())
	}
}

func TestGorushSendLiveActivityRejectsZeroTimestamp(t *testing.T) {
	server, _ := newCaptureServer(t)
	g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())
	err := g.SendLiveActivity(context.Background(), LiveActivityPush{
		Token: "la-tok", Event: "update", ContentState: map[string]any{},
	})
	if err == nil {
		t.Fatal("expected error for zero Timestamp")
	}
}

func TestGorushSendLiveActivityNon2xxIsErrorWithoutBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"echo":"la-tok-SECRET"}`))
	}))
	defer server.Close()
	g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())
	err := g.SendLiveActivity(context.Background(), LiveActivityPush{
		Token: "la-tok-SECRET", Event: "update", ContentState: map[string]any{}, Timestamp: time.Unix(1, 0),
	})
	if err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("err = %v, want status 400", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks response body/token: %v", err)
	}
}
```

Add `"reflect"` and `"sync/atomic"` to the test imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/push -run 'TestGorushSendLiveActivity' -v`
Expected: compile error `undefined: LiveActivityPush`.

- [ ] **Step 3: Implement**

In `internal/push/push.go`, append:

```go
// LiveActivityPush is one ActivityKit push to one Live Activity token
// (spec §6.6). ContentState marshals to the §6.2 object; it is typed any
// because the concrete type lives in internal/liveactivities, which imports
// this package (design spec §2.7).
type LiveActivityPush struct {
	Token   string
	Sandbox bool   // APNs sandbox routing (spec §2.7)
	Event   string // "update" | "end"
	// ContentState is the §6.2 content-state object.
	ContentState any
	// Timestamp is required and must advance on every push to one activity;
	// APNs silently drops a push whose timestamp does not.
	Timestamp time.Time
	// StaleDate is sent on updates (~10 minutes out); zero omits it.
	StaleDate time.Time
	// DismissalDate is sent on end (~15 minutes out); zero omits it.
	DismissalDate time.Time
}

// LiveActivitySender delivers Live Activity pushes. Send semantics match
// Sender: nil means the transport accepted the push, not that the device
// received it; terminal failures arrive via the feedback webhook (§6.5).
type LiveActivitySender interface {
	SendLiveActivity(ctx context.Context, p LiveActivityPush) error
}
```

Add `"time"` to push.go's imports.

In `internal/push/gorush.go`:

```go
// liveActivityTopicSuffix is appended to the app bundle id to form the APNs
// topic for Live Activity pushes (spec §6.6). gorush does not derive it.
const liveActivityTopicSuffix = ".push-type.liveactivity"

// gorushLiveActivity is gorush's Live Activity request shape. The date keys
// and content-state are HYPHENATED because they map 1:1 onto APNs aps keys;
// gorush's unmarshaller silently drops snake_case variants and the activity
// then never updates. No title/message: a Live Activity push has no alert.
type gorushLiveActivity struct {
	Tokens        []string `json:"tokens"`
	Platform      int      `json:"platform"`
	PushType      string   `json:"push_type"`
	Priority      string   `json:"priority"`
	Topic         string   `json:"topic"`
	Development   bool     `json:"development,omitempty"`
	Event         string   `json:"event"`
	ContentState  any      `json:"content-state"`
	Timestamp     int64    `json:"timestamp"`
	StaleDate     int64    `json:"stale-date,omitempty"`
	DismissalDate int64    `json:"dismissal-date,omitempty"`
}

// SendLiveActivity posts p as a liveactivity push at APNs priority 10
// (gorush "high"); at priority 5 an idle phone holds every push and the
// Lock Screen freezes (spec §6.6). An empty APNs topic is refused without a
// request: unlike Send, where gorush's own config might supply a topic, a
// bare ".push-type.liveactivity" would bounce BadTopic every minute for
// eight hours (design spec §2.7).
func (g *Gorush) SendLiveActivity(ctx context.Context, p LiveActivityPush) error {
	if g.apnsTopic == "" {
		return errors.New("push: live activity push requires an APNs topic (--apns-topic)")
	}
	if p.Timestamp.IsZero() {
		return errors.New("push: live activity push requires a timestamp")
	}
	n := gorushLiveActivity{
		Tokens:       []string{p.Token},
		Platform:     int(PlatformIOS),
		PushType:     "liveactivity",
		Priority:     "high",
		Topic:        g.apnsTopic + liveActivityTopicSuffix,
		Development:  p.Sandbox,
		Event:        p.Event,
		ContentState: p.ContentState,
		Timestamp:    p.Timestamp.Unix(),
	}
	if !p.StaleDate.IsZero() {
		n.StaleDate = p.StaleDate.Unix()
	}
	if !p.DismissalDate.IsZero() {
		n.DismissalDate = p.DismissalDate.Unix()
	}
	return g.post(ctx, map[string]any{"notifications": []gorushLiveActivity{n}})
}
```

Refactor `Send` so its tail (from `body, err := json.Marshal(...)` through the status check) becomes a shared method:

```go
// post submits one /api/push batch. Never include the response body in the
// error: gorush error bodies echo the notification, tokens included.
func (g *Gorush) post(ctx context.Context, batch any) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("push: marshal notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.http.Do(req)
	if err != nil {
		return fmt.Errorf("push: gorush request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain so the connection is reused
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("push: gorush returned status %d", resp.StatusCode)
	}
	return nil
}
```

and `Send` ends with `return g.post(ctx, map[string]any{"notifications": []gorushNotification{gn}})`. Keep the existing comments on `Send`. Add `"errors"` to gorush.go imports. Update the package doc comment to mention Live Activity pushes.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/push -v`
Expected: all PASS, including the pre-existing `Send` tests.

- [ ] **Step 5: Mutation check**

Temporarily change `Topic: g.apnsTopic + liveActivityTopicSuffix` to `Topic: g.apnsTopic`; `TestGorushSendLiveActivityPostsExpectedJSON` must fail on `topic`. Revert.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/push
git commit -m "feat(push): Live Activity sender on gorush (push_type liveactivity, derived topic)"
```

---

### Task 2: `obaapi.ArrivalsAndDeparturesForStop`

**Files:**
- Modify: `internal/obaapi/obaapi.go`
- Modify (stub the new method): `internal/httpapi/alarms_api_test.go:26-51`, `internal/httpapi/vehicles_test.go:23-50`, `internal/vehicles/service_test.go` (`fleetByRegion`), `internal/ghostbus/snapshot_test.go` (`fakeTripDetailsSource` only if it declares itself an `obaapi.Client` — check with `go vet ./...`)
- Test: `internal/obaapi/obaapi_test.go`

**Interfaces:**
- Produces:
  ```go
  type StopArrivalsQuery struct { StopID string; MinutesBefore, MinutesAfter int64 }
  type StopArrival struct {
      StopID, TripID, RouteID string
      ServiceDate, StopSequence int64
      HasIdentity bool
      LastUpdateTime int64
      RouteShortName, TripHeadsign string
      Predicted bool
      ScheduledArrivalTime, PredictedArrivalTime, ScheduledDepartureTime, PredictedDepartureTime int64
  }
  // on Client:
  ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q StopArrivalsQuery) ([]StopArrival, error)
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/obaapi/obaapi_test.go`:

```go
// stopArrivalsServer stands in for arrivals-and-departures-for-stop. entries
// is served verbatim so tests can omit or mistype fields; nullBody serves a
// literal `null` 200.
type stopArrivalsServer struct {
	*httptest.Server
	calls     atomic.Int64
	lastQuery atomic.Value // url.Values
	status    int
	nullBody  bool
	entries   []map[string]any
	routes    []map[string]any
	trips     []map[string]any
}

func newStopArrivalsServer(t *testing.T, entries []map[string]any) *stopArrivalsServer {
	t.Helper()
	s := &stopArrivalsServer{entries: entries}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/where/arrivals-and-departures-for-stop/", func(w http.ResponseWriter, r *http.Request) {
		s.calls.Add(1)
		s.lastQuery.Store(r.URL.Query())
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		if s.nullBody {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("null"))
			return
		}
		routes, trips := s.routes, s.trips
		if routes == nil {
			routes = []map[string]any{}
		}
		if trips == nil {
			trips = []map[string]any{}
		}
		entries := s.entries
		if entries == nil {
			entries = []map[string]any{}
		}
		writeOBA(w, map[string]any{
			"entry": map[string]any{"stopId": "1_570", "arrivalsAndDepartures": entries, "nearbyStopIds": []any{}, "situationIds": []any{}},
			"references": map[string]any{"agencies": []any{}, "routes": routes, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": trips},
		})
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func fullStopEntry() map[string]any {
	return map[string]any{
		"stopId": "1_570", "tripId": "1_604370", "routeId": "1_100044", "serviceDate": 1754809200000,
		"stopSequence": 3, "lastUpdateTime": 1754812000000, "routeShortName": "44", "tripHeadsign": "Ballard",
		"predicted": true, "scheduledArrivalTime": 1754812800000, "predictedArrivalTime": 1754812860000,
		"scheduledDepartureTime": 1754812800000, "predictedDepartureTime": 1754812860000,
		"arrivalEnabled": true, "departureEnabled": true, "blockTripSequence": 0, "numberOfStopsAway": 2,
		"totalStopsInTrip": 30, "vehicleId": "1_4361",
	}
}

func TestArrivalsAndDeparturesForStop_MapsEntries(t *testing.T) {
	srv := newStopArrivalsServer(t, []map[string]any{fullStopEntry()})
	got, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570", MinutesBefore: 5, MinutesAfter: 120})
	if err != nil {
		t.Fatalf("ArrivalsAndDeparturesForStop: %v", err)
	}
	want := []StopArrival{{
		StopID: "1_570", TripID: "1_604370", RouteID: "1_100044", ServiceDate: 1754809200000, StopSequence: 3,
		HasIdentity: true, LastUpdateTime: 1754812000000, RouteShortName: "44", TripHeadsign: "Ballard",
		Predicted: true, ScheduledArrivalTime: 1754812800000, PredictedArrivalTime: 1754812860000,
		ScheduledDepartureTime: 1754812800000, PredictedDepartureTime: 1754812860000,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	q := srv.lastQuery.Load().(url.Values)
	if q.Get("minutesBefore") != "5" || q.Get("minutesAfter") != "120" {
		t.Errorf("query = %v, want minutesBefore=5 minutesAfter=120", q)
	}
	if q.Get("key") != sentinelKey {
		t.Errorf("key = %q", q.Get("key"))
	}
}

func TestArrivalsAndDeparturesForStop_ReferencesFallbackForNameAndHeadsign(t *testing.T) {
	e := fullStopEntry()
	delete(e, "routeShortName")
	delete(e, "tripHeadsign")
	srv := newStopArrivalsServer(t, []map[string]any{e})
	srv.routes = []map[string]any{{"id": "1_100044", "shortName": "44", "agencyId": "1"}}
	srv.trips = []map[string]any{{"id": "1_604370", "tripHeadsign": "Ballard", "routeId": "1_100044"}}
	got, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got[0].RouteShortName != "44" || got[0].TripHeadsign != "Ballard" {
		t.Errorf("fallback: shortName=%q headsign=%q", got[0].RouteShortName, got[0].TripHeadsign)
	}
}

func TestArrivalsAndDeparturesForStop_HasIdentityFalseWhenComponentMissingOrInvalid(t *testing.T) {
	missing := fullStopEntry()
	delete(missing, "stopSequence")
	invalid := fullStopEntry()
	invalid["serviceDate"] = "not-a-number"
	zero := fullStopEntry()
	zero["stopSequence"] = 0
	srv := newStopArrivalsServer(t, []map[string]any{missing, invalid, zero})
	got, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got[0].HasIdentity {
		t.Error("missing stopSequence: HasIdentity should be false")
	}
	if got[1].HasIdentity {
		t.Error("non-numeric serviceDate: HasIdentity should be false")
	}
	if !got[2].HasIdentity || got[2].StopSequence != 0 {
		t.Errorf("stopSequence 0 is a real value: HasIdentity=%v seq=%d", got[2].HasIdentity, got[2].StopSequence)
	}
}

func TestArrivalsAndDeparturesForStop_EmptyListIsNotAnError(t *testing.T) {
	srv := newStopArrivalsServer(t, nil)
	got, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty, nil", got, err)
	}
}

func TestArrivalsAndDeparturesForStop_NullBodyAnd404AreErrNotFound(t *testing.T) {
	srv := newStopArrivalsServer(t, nil)
	srv.nullBody = true
	_, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("null body: err = %v, want ErrNotFound", err)
	}
	srv.nullBody = false
	srv.status = http.StatusNotFound
	_, err = New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("404: err = %v, want ErrNotFound", err)
	}
	srv.status = http.StatusBadGateway
	_, err = New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, sentinelKey), StopArrivalsQuery{StopID: "1_570"})
	if err == nil || errors.Is(err, ErrNotFound) || errChainContains(err, sentinelKey) {
		t.Errorf("502: err = %v, want redacted transient error", err)
	}
}

func TestArrivalsAndDeparturesForStop_NoKeyIsErrNotConfigured(t *testing.T) {
	srv := newStopArrivalsServer(t, nil)
	_, err := New("", srv.Client(), nil).ArrivalsAndDeparturesForStop(context.Background(),
		testRegion(srv.URL, ""), StopArrivalsQuery{StopID: "1_570"})
	if !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
	if srv.calls.Load() != 0 {
		t.Error("no request must be made without a key")
	}
}
```

Add `"reflect"` to the imports if absent.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/obaapi -run ArrivalsAndDeparturesForStop`
Expected: compile error `undefined: StopArrivalsQuery`.

- [ ] **Step 3: Implement**

In `internal/obaapi/obaapi.go`, after `Departure`:

```go
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
```

Add to the `Client` interface:

```go
	// ArrivalsAndDeparturesForStop lists every arrival/departure at a stop
	// inside the query window. ErrNotFound on a 404 or a null/absent body;
	// an empty list is not an error. ErrNotConfigured when the region has
	// no API key.
	ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q StopArrivalsQuery) ([]StopArrival, error)
```

Implementation, after `ArrivalAndDeparture`:

```go
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
		present := func(f apijson.Field) bool { return !f.IsNull() && !f.IsInvalid() }
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
```

Import `"github.com/OneBusAway/go-sdk/internal/apijson"` is **not importable** (internal). Instead declare a local interface: `type jsonField interface{ IsNull() bool; IsInvalid() bool }` and make `present` take `jsonField`; `apijson.Field` satisfies it. Verify `ReferencesRoute` has `LongName` and `ReferencesTrip` has `ID`/`TripHeadsign` in `$(go list -m -f '{{.Dir}}' github.com/OneBusAway/go-sdk)/shared/shared.go`; adjust names if they differ.

Stub the new method on every `obaapi.Client` fake (`go vet ./...` lists the ones that fail):

```go
func (f *fakeAlarmsOBA) ArrivalsAndDeparturesForStop(context.Context, regions.Region, obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
	panic("fakeAlarmsOBA.ArrivalsAndDeparturesForStop: unused by alarm tests")
}
```

(same shape for `fakeOBA` in `vehicles_test.go` and `fleetByRegion` in `internal/vehicles/service_test.go`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/obaapi ./internal/httpapi ./internal/vehicles ./internal/ghostbus`
Expected: PASS.

- [ ] **Step 5: Mutation check**

Change `present(j.StopSequence)` to `true`; `..._HasIdentityFalseWhenComponentMissingOrInvalid` must fail on "missing stopSequence". Revert.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/obaapi internal/httpapi internal/vehicles internal/ghostbus
git commit -m "feat(obaapi): ArrivalsAndDeparturesForStop with identity presence and references fallback"
```

---

### Task 3: `internal/liveactivities` domain types and content-state builder

**Files:**
- Create: `internal/liveactivities/liveactivities.go`
- Create: `internal/liveactivities/contentstate.go`
- Create: `internal/liveactivities/testdata/live_activity_content_state.json` (copy of `../ios/OBAKitTests/fixtures/live_activity_content_state.json`, byte-for-byte)
- Test: `internal/liveactivities/contentstate_test.go`

**Interfaces:**
- Consumes: `obaapi.StopArrival` (Task 2).
- Produces (used by Tasks 4–8):
  ```go
  var ErrNotFound, ErrDuplicate error
  const Lifetime, KeepaliveInterval, StaleAfter, DismissAfterEnd, StopCacheTTL, StopFetchBudget time.Duration
  const MaxConsecutiveFailures, MaxArrivals, LookbackMinutes, LookaheadMinutes int64
  type LiveActivity struct { ID, RegionID int64; Token, ActivityID, PushToken string; APNSSandbox bool; StopID, RouteShortName, TripHeadsign, TripID string; ServiceDate int64; VehicleID string; StopSequence *int64; LastContentState ContentState; LastPushedAt *time.Time; ConsecutiveFailures int64; ExpiresAt, CreatedAt time.Time }
  type NewLiveActivity struct { RegionID int64; Token string; ExpiresAt time.Time; ActivityID, PushToken string; APNSSandbox bool; StopID, RouteShortName, TripHeadsign, TripID string; ServiceDate int64; VehicleID string; StopSequence *int64 }
  type Repository interface { Upsert(ctx, in NewLiveActivity, now time.Time) (LiveActivity, error); Delete(ctx, regionID int64, token string) error; DeleteByID(ctx, id int64) error; DeleteByPushToken(ctx, pushToken string) (int64, error); List(ctx) ([]LiveActivity, error); RecordFailure(ctx, id int64) (int64, error); ResetFailures(ctx, id int64) error; RecordPush(ctx, id int64, state ContentState, pushedAt time.Time) error }
  type ContentState struct { Arrivals []ArrivalInfo `json:"arrivals"` }
  type ArrivalInfo struct { DepartureTime int64 `json:"departure_time"`; ScheduleStatus string `json:"schedule_status"`; ScheduleDeviation int64 `json:"schedule_deviation"`; IsArrival bool `json:"is_arrival"` }
  func EmptyContentState() ContentState
  func BuildContentState(entries []obaapi.StopArrival, routeShortName, tripHeadsign string, now time.Time) ContentState
  func Changed(prev, next ContentState) bool
  ```

- [ ] **Step 1: Copy the fixture**

```bash
mkdir -p internal/liveactivities/testdata
cp ../ios/OBAKitTests/fixtures/live_activity_content_state.json internal/liveactivities/testdata/
```

- [ ] **Step 2: Write the failing tests**

`internal/liveactivities/contentstate_test.go`:

```go
package liveactivities_test

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
)

// now is the fixed instant every builder test measures "past" against.
var now = time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)

func ms(t time.Time) int64 { return t.UnixMilli() }

// entry builds a fully-identified, predicted arrival at now+offset with the
// given deviation, at stopSequence 3 (an "arrival" per §6.2).
func entry(offset, deviation time.Duration) obaapi.StopArrival {
	sched := now.Add(offset)
	pred := sched.Add(deviation)
	return obaapi.StopArrival{
		StopID: "1_570", TripID: "1_" + sched.Format("150405"), RouteID: "1_100044",
		ServiceDate: 1754809200000, StopSequence: 3, HasIdentity: true,
		LastUpdateTime: ms(now), RouteShortName: "44", TripHeadsign: "Ballard", Predicted: true,
		ScheduledArrivalTime: ms(sched), PredictedArrivalTime: ms(pred),
		ScheduledDepartureTime: ms(sched.Add(time.Second)), PredictedDepartureTime: ms(pred.Add(time.Second)),
	}
}

func build(entries ...obaapi.StopArrival) liveactivities.ContentState {
	return liveactivities.BuildContentState(entries, "44", "Ballard", now)
}

func TestFixtureRoundTripsWithDefaultDecoder(t *testing.T) {
	raw, err := os.ReadFile("testdata/live_activity_content_state.json")
	if err != nil {
		t.Fatal(err)
	}
	var state liveactivities.ContentState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(state.Arrivals) != 3 || state.Arrivals[0].DepartureTime != 1767980460 ||
		state.Arrivals[0].ScheduleStatus != "on_time" || state.Arrivals[0].ScheduleDeviation != 60 ||
		state.Arrivals[0].IsArrival || state.Arrivals[1].ScheduleStatus != "delayed" ||
		state.Arrivals[2].ScheduleStatus != "unknown" {
		t.Fatalf("decoded fixture = %+v", state)
	}
	// Canonical form of the fixture vs canonical form of our encoding: any
	// key rename or type change diverges here.
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	wantCanon, _ := json.Marshal(generic)
	ours, _ := json.Marshal(state)
	var oursGeneric any
	_ = json.Unmarshal(ours, &oursGeneric)
	gotCanon, _ := json.Marshal(oursGeneric)
	if !bytes.Equal(wantCanon, gotCanon) {
		t.Errorf("encoding drifted from fixture:\n got %s\nwant %s", gotCanon, wantCanon)
	}
}

func TestEmptyStateMarshalsArrivalsAsEmptyArray(t *testing.T) {
	b, _ := json.Marshal(liveactivities.EmptyContentState())
	if string(b) != `{"arrivals":[]}` {
		t.Errorf("got %s", b)
	}
	b, _ = json.Marshal(build())
	if string(b) != `{"arrivals":[]}` {
		t.Errorf("BuildContentState with no entries: got %s", b)
	}
}

func TestBuildFiltersByRouteAndHeadsign(t *testing.T) {
	other := entry(5*time.Minute, 0)
	other.TripHeadsign = "Downtown"
	wrongRoute := entry(6*time.Minute, 0)
	wrongRoute.RouteShortName = "45"
	got := build(other, wrongRoute, entry(7*time.Minute, 0))
	if len(got.Arrivals) != 1 || got.Arrivals[0].DepartureTime != now.Add(7*time.Minute).Unix() {
		t.Errorf("got %+v", got.Arrivals)
	}
}

func TestBuildCollapsesDuplicateVehicleReports(t *testing.T) {
	a := entry(5*time.Minute, 0)
	b := a
	b.LastUpdateTime = a.LastUpdateTime + 1
	b.PredictedArrivalTime += 60_000 // newer report, different minute
	got := build(a, b)
	if len(got.Arrivals) != 1 {
		t.Fatalf("want 1 arrival, got %+v", got.Arrivals)
	}
	if got.Arrivals[0].DepartureTime != b.PredictedArrivalTime/1000 {
		t.Errorf("survivor should be the newest report; got %d want %d", got.Arrivals[0].DepartureTime, b.PredictedArrivalTime/1000)
	}
}

func TestBuildTieKeepsFirstInResponseOrder(t *testing.T) {
	a := entry(5*time.Minute, 0)
	b := a
	b.PredictedArrivalTime += 60_000
	got := build(a, b)
	if len(got.Arrivals) != 1 || got.Arrivals[0].DepartureTime != a.PredictedArrivalTime/1000 {
		t.Errorf("tie must keep first in order; got %+v", got.Arrivals)
	}
}

func TestBuildNeverCollapsesLoopRouteOrCrossServiceDateOrUnidentified(t *testing.T) {
	loopA := entry(5*time.Minute, 0)
	loopB := loopA
	loopB.StopSequence = 9
	loopB.PredictedArrivalTime += 600_000
	crossDate := loopA
	crossDate.ServiceDate += 86_400_000
	crossDate.PredictedArrivalTime += 1_200_000
	unidentA := loopA
	unidentA.HasIdentity = false
	unidentB := unidentA
	got := build(loopA, loopB, crossDate, unidentA, unidentB)
	if len(got.Arrivals) != 3 {
		t.Errorf("want cap of 3 distinct arrivals, got %d: %+v", len(got.Arrivals), got.Arrivals)
	}
	got = build(unidentA, unidentB)
	if len(got.Arrivals) != 2 {
		t.Errorf("unidentified entries must never collapse; got %+v", got.Arrivals)
	}
}

func TestBuildArrivalVsDepartureSelection(t *testing.T) {
	first := entry(5*time.Minute, 0)
	first.StopSequence = 0
	got := build(first)
	if got.Arrivals[0].IsArrival {
		t.Error("stopSequence 0 is a departure")
	}
	if got.Arrivals[0].DepartureTime != first.PredictedDepartureTime/1000 {
		t.Errorf("departure pair expected; got %d", got.Arrivals[0].DepartureTime)
	}
	noArr := entry(5*time.Minute, 0)
	noArr.ScheduledArrivalTime, noArr.PredictedArrivalTime = 0, 0
	got = build(noArr)
	if !got.Arrivals[0].IsArrival || got.Arrivals[0].DepartureTime != noArr.PredictedDepartureTime/1000 {
		t.Errorf("omitted arrival times must fall back to departure pair; got %+v", got.Arrivals[0])
	}
}

func TestBuildPredictedOnlyWhenFlaggedAndPositive(t *testing.T) {
	sched := entry(5*time.Minute, 2*time.Minute)
	sched.Predicted = false
	got := build(sched)
	a := got.Arrivals[0]
	if a.DepartureTime != sched.ScheduledArrivalTime/1000 || a.ScheduleStatus != "unknown" || a.ScheduleDeviation != 0 {
		t.Errorf("unpredicted: %+v", a)
	}
	zeroPred := entry(5*time.Minute, 2*time.Minute)
	zeroPred.PredictedArrivalTime = 0
	got = build(zeroPred)
	if got.Arrivals[0].ScheduleStatus != "unknown" {
		t.Errorf("predicted with 0 time must be unknown: %+v", got.Arrivals[0])
	}
}

func TestBuildDropsPastSortsAndCaps(t *testing.T) {
	past := entry(-2*time.Minute, 0)
	atNow := entry(0, 0)
	got := build(entry(30*time.Minute, 0), past, entry(10*time.Minute, 0), atNow, entry(20*time.Minute, 0), entry(40*time.Minute, 0))
	if len(got.Arrivals) != 3 {
		t.Fatalf("cap: got %d", len(got.Arrivals))
	}
	if got.Arrivals[0].DepartureTime != now.Unix() {
		t.Errorf("an entry at exactly now must survive; first = %d", got.Arrivals[0].DepartureTime)
	}
	for i := 1; i < len(got.Arrivals); i++ {
		if got.Arrivals[i].DepartureTime < got.Arrivals[i-1].DepartureTime {
			t.Errorf("not sorted: %+v", got.Arrivals)
		}
	}
	if got.Arrivals[2].DepartureTime != now.Add(20*time.Minute).Unix() {
		t.Errorf("third should be +20m, got %d", got.Arrivals[2].DepartureTime)
	}
}

func TestScheduleStatusThresholds(t *testing.T) {
	cases := []struct {
		dev  time.Duration
		want string
	}{
		{-91 * time.Second, "early"}, {-90 * time.Second, "on_time"},
		{89 * time.Second, "on_time"}, {90 * time.Second, "delayed"},
	}
	for _, c := range cases {
		got := build(entry(5*time.Minute, c.dev)).Arrivals[0]
		if got.ScheduleStatus != c.want || got.ScheduleDeviation != int64(c.dev/time.Second) {
			t.Errorf("dev %v: got %+v, want %s", c.dev, got, c.want)
		}
	}
}

func TestChangedComparesArrivalsOnly(t *testing.T) {
	a := build(entry(5*time.Minute, 0))
	b := build(entry(5*time.Minute, 0))
	if liveactivities.Changed(a, b) {
		t.Error("identical states must not be changed")
	}
	c := build(entry(6*time.Minute, 0))
	if !liveactivities.Changed(a, c) {
		t.Error("different departure time must be changed")
	}
	if !liveactivities.Changed(liveactivities.EmptyContentState(), a) {
		t.Error("empty vs non-empty must be changed")
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/liveactivities`
Expected: compile error (package does not exist).

- [ ] **Step 4: Implement `liveactivities.go`**

```go
// Package liveactivities implements the OneBusAway sidecar spec §6 (iOS Live
// Activities): a stateful Lock Screen subscription the sidecar updates once
// a minute via ActivityKit pushes until it expires, runs dry, or its token
// dies. Design: docs/superpowers/specs/2026-08-24-live-activities-design.md.
package liveactivities

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound reports that no live activity matches the given region and token.
	ErrNotFound = errors.New("live activity not found")
	// ErrDuplicate reports an Upsert that lost the concurrent first-registration
	// race on (region, activity_id); callers retry once (design spec §2.1).
	ErrDuplicate = errors.New("duplicate live activity registration")
)

// Lifecycle constants (design spec §5). Durations are absolute-instant
// arithmetic; no zone is ever consulted.
const (
	// Lifetime is the hard expiry set at registration (Apple's 8h HIG ceiling).
	Lifetime = 8 * time.Hour
	// KeepaliveInterval is deliberately under the 1-minute cadence: the
	// last-push timestamp is stamped after the push round-trip, so at exactly
	// 60s the next cycle misses by milliseconds and the widget updates every
	// other minute (spec §6.3).
	KeepaliveInterval = 55 * time.Second
	// StaleAfter is the update push's stale-date offset.
	StaleAfter = 10 * time.Minute
	// DismissAfterEnd is the end push's dismissal-date offset.
	DismissAfterEnd = 15 * time.Minute
	// StopCacheTTL bounds how long one stop's arrivals are shared across
	// subscriptions (spec §6.3 cost control).
	StopCacheTTL = 55 * time.Second
	// StopFetchBudget bounds one shared upstream fetch; above obaapi's 4s
	// per-request timeout with no retries.
	StopFetchBudget = 6 * time.Second

	// MaxConsecutiveFailures ends an activity after this many empty/error
	// cycles in a row (spec §6.3).
	MaxConsecutiveFailures int64 = 3
	// MaxArrivals caps the content state (spec §6.2).
	MaxArrivals = 3
	// LookbackMinutes and LookaheadMinutes are the OBA query window.
	LookbackMinutes  int64 = 5
	LookaheadMinutes int64 = 120
)

// LiveActivity is one subscription as stored. Identity is bookmark-scoped
// (stop + route + headsign); the trip fields are display metadata only.
type LiveActivity struct {
	ID          int64
	RegionID    int64
	Token       string // public address (spec §2.4)
	ActivityID  string // ActivityKit activity id; upsert key with RegionID
	PushToken   string // ActivityKit push token (not the device alert token)
	APNSSandbox bool

	StopID         string
	RouteShortName string
	TripHeadsign   string
	TripID         string // "" = omitted
	ServiceDate    int64  // epoch ms; 0 = omitted
	VehicleID      string // "" = omitted
	StopSequence   *int64 // nil = omitted; 0 is a real value

	LastContentState    ContentState
	LastPushedAt        *time.Time // nil = never pushed
	ConsecutiveFailures int64
	ExpiresAt           time.Time
	CreatedAt           time.Time
}

// NewLiveActivity is the input to Repository.Upsert. Token and ExpiresAt
// are used only when the upsert inserts (design spec §2.1).
type NewLiveActivity struct {
	RegionID    int64
	Token       string
	ExpiresAt   time.Time
	ActivityID  string
	PushToken   string
	APNSSandbox bool

	StopID         string
	RouteShortName string
	TripHeadsign   string
	TripID         string
	ServiceDate    int64
	VehicleID      string
	StopSequence   *int64
}

// Repository persists live activities. Implementations must be safe for
// concurrent use: the updater sweep runs List/RecordFailure/RecordPush/
// DeleteByID concurrently with the HTTP handlers' Upsert/Delete and the
// feedback webhook's DeleteByPushToken.
type Repository interface {
	// Upsert inserts on a new (region, activity_id) or rewrites the
	// registration fields of an existing one, preserving token, expiry, and
	// the updater's bookkeeping. ErrDuplicate on the first-registration race.
	Upsert(ctx context.Context, in NewLiveActivity, now time.Time) (LiveActivity, error)
	Delete(ctx context.Context, regionID int64, token string) error // ErrNotFound; 204 contract
	DeleteByID(ctx context.Context, id int64) error                 // missing row is success
	DeleteByPushToken(ctx context.Context, pushToken string) (int64, error)
	List(ctx context.Context) ([]LiveActivity, error)
	RecordFailure(ctx context.Context, id int64) (int64, error) // ++consecutive_failures, returns streak
	ResetFailures(ctx context.Context, id int64) error
	RecordPush(ctx context.Context, id int64, state ContentState, pushedAt time.Time) error
}
```

- [ ] **Step 5: Implement `contentstate.go`**

```go
package liveactivities

import (
	"slices"
	"time"

	"github.com/OneBusAway/sidecar/internal/obaapi"
)

// ContentState is the §6.2 wire contract the iOS widget decodes with a
// default JSONDecoder. Keys and types are fixed; Arrivals marshals as []
// never null. NO timestamp or other always-changing field may ever be
// added: Changed compares consecutive states to decide whether to push.
type ContentState struct {
	Arrivals []ArrivalInfo `json:"arrivals"`
}

// ArrivalInfo is one row of the widget. DepartureTime is epoch SECONDS
// (ActivityKit dates decode from seconds; the rest of OBA is ms).
type ArrivalInfo struct {
	DepartureTime     int64  `json:"departure_time"`
	ScheduleStatus    string `json:"schedule_status"` // early | on_time | delayed | unknown
	ScheduleDeviation int64  `json:"schedule_deviation"` // seconds; 0 when schedule-only
	IsArrival         bool   `json:"is_arrival"`
}

// EmptyContentState is the state with no arrivals, as stored before the
// first push and as sent on an end push with no history.
func EmptyContentState() ContentState { return ContentState{Arrivals: []ArrivalInfo{}} }

// Changed reports whether next differs from prev (arrivals only).
func Changed(prev, next ContentState) bool {
	return !slices.Equal(prev.Arrivals, next.Arrivals)
}

// Thresholds mirror OBAKitCore ArrivalDeparture.scheduleStatus exactly, in
// minutes, half-open on the late side: exactly -1.5 is on_time, exactly
// +1.5 is delayed (spec §6.2).
const (
	earlyThresholdMinutes  = -1.5
	onTimeThresholdMinutes = 1.5
)

// visitIdentity is OBAKitCore ArrivalDeparture.id's components: one trip's
// visit to one stop. Never tripId alone -- a loop route visits a stop twice
// per trip at different sequences, and a tripId recurs across service dates.
type visitIdentity struct {
	stopID, tripID, routeID string
	serviceDate, stopSeq    int64
}

// BuildContentState is a pure port of OBACloud's LiveActivityContentState
// and of the app's own client-side dedupe (design spec §2.3): filter to the
// bookmark, collapse duplicate vehicle reports, choose arrival vs departure
// and predicted vs scheduled times, drop the past, sort, cap. Both sides
// must pick the SAME survivor or a pushed card and a local refresh disagree.
func BuildContentState(entries []obaapi.StopArrival, routeShortName, tripHeadsign string, now time.Time) ContentState {
	matching := make([]obaapi.StopArrival, 0, len(entries))
	for _, e := range entries {
		if e.RouteShortName == routeShortName && e.TripHeadsign == tripHeadsign {
			matching = append(matching, e)
		}
	}
	nowSec := now.Unix()
	arrivals := make([]ArrivalInfo, 0, MaxArrivals)
	for _, e := range collapseDuplicateVehicleReports(matching) {
		if a, ok := arrivalInfo(e, nowSec); ok {
			arrivals = append(arrivals, a)
		}
	}
	slices.SortStableFunc(arrivals, func(a, b ArrivalInfo) int {
		return int(a.DepartureTime - b.DepartureTime)
	})
	if len(arrivals) > MaxArrivals {
		arrivals = arrivals[:MaxArrivals]
	}
	return ContentState{Arrivals: arrivals}
}

// collapseDuplicateVehicleReports keeps, per visit identity, the entry with
// the newest LastUpdateTime (ties: first in response order -- iOS replaces
// only on a strictly newer report). Entries without a complete identity are
// never collapsed: showing a duplicate row is cosmetic, hiding a bus is not.
func collapseDuplicateVehicleReports(entries []obaapi.StopArrival) []obaapi.StopArrival {
	out := make([]obaapi.StopArrival, 0, len(entries))
	survivor := make(map[visitIdentity]int) // identity -> index in out
	for _, e := range entries {
		if !e.HasIdentity {
			out = append(out, e)
			continue
		}
		id := visitIdentity{e.StopID, e.TripID, e.RouteID, e.ServiceDate, e.StopSequence}
		if i, seen := survivor[id]; seen {
			if e.LastUpdateTime > out[i].LastUpdateTime {
				out[i] = e
			}
			continue
		}
		survivor[id] = len(out)
		out = append(out, e)
	}
	return out
}

// arrivalInfo maps one entry per §6.2: any non-first stop is an "arrival"
// showing arrival times (falling back to departure times when a feed omits
// them); predicted times only when flagged and positive. Returns false when
// the chosen time is already past.
func arrivalInfo(e obaapi.StopArrival, nowSec int64) (ArrivalInfo, bool) {
	isArrival := e.StopSequence != 0
	useArrival := isArrival && (e.ScheduledArrivalTime > 0 || e.PredictedArrivalTime > 0)
	predictedMs, scheduledMs := e.PredictedDepartureTime, e.ScheduledDepartureTime
	if useArrival {
		predictedMs, scheduledMs = e.PredictedArrivalTime, e.ScheduledArrivalTime
	}
	predicted := e.Predicted && predictedMs > 0
	timeMs := scheduledMs
	var deviation int64
	if predicted {
		timeMs = predictedMs
		deviation = (predictedMs - scheduledMs) / 1000
	}
	timeSec := timeMs / 1000
	if timeSec < nowSec {
		return ArrivalInfo{}, false
	}
	return ArrivalInfo{
		DepartureTime:     timeSec,
		ScheduleStatus:    scheduleStatus(predicted, deviation),
		ScheduleDeviation: deviation,
		IsArrival:         isArrival,
	}, true
}

func scheduleStatus(predicted bool, deviationSec int64) string {
	if !predicted {
		return "unknown"
	}
	minutes := float64(deviationSec) / 60.0
	switch {
	case minutes < earlyThresholdMinutes:
		return "early"
	case minutes < onTimeThresholdMinutes:
		return "on_time"
	default:
		return "delayed"
	}
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./internal/liveactivities -v`
Expected: all PASS.

- [ ] **Step 7: Mutation checks**

(a) Change `minutes < onTimeThresholdMinutes` to `<=`; `TestScheduleStatusThresholds` must fail at +90s. (b) Change `e.LastUpdateTime > out[i].LastUpdateTime` to `>=`; `TestBuildTieKeepsFirstInResponseOrder` must fail. (c) Change `timeSec < nowSec` to `<=`; `TestBuildDropsPastSortsAndCaps` must fail on "exactly now". Revert each.

- [ ] **Step 8: Lint and commit**

```bash
make lint
git add internal/liveactivities
git commit -m "feat(liveactivities): domain types and §6.2 content-state builder pinned to the iOS fixture"
```

---

### Task 4: SQLite storage — migration, queries, adapter, conformance suite

**Files:**
- Create: `internal/store/sqlite/migrations/00008_live_activities.sql`
- Create: `internal/store/sqlite/queries/liveactivities.sql`
- Generate: `internal/store/sqlite/gen/` (`make generate`)
- Create: `internal/store/sqlite/liveactivities.go`
- Modify: `internal/store/sqlite/store.go` (add `LiveActivities()` accessor next to `Alarms()`)
- Create: `internal/store/storetest/liveactivitytest.go`
- Modify: `internal/store/sqlite/store_test.go` (`TestMigrateDeclaresTimeColumnsAsInteger` map; add conformance hookup)

**Interfaces:**
- Consumes: `liveactivities.Repository`, `NewLiveActivity`, `ContentState`, `EmptyContentState`, `ErrNotFound`, `ErrDuplicate` (Task 3).
- Produces: `func (s *Store) LiveActivities() liveactivities.Repository`; `storetest.RunLiveActivityRepository(t, func(*testing.T) (liveactivities.Repository, regions.Repository))`.

- [ ] **Step 1: Migration**

`internal/store/sqlite/migrations/00008_live_activities.sql`:

```sql
-- +goose Up
CREATE TABLE live_activities (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id            INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  -- The public address (spec section 2.4). Globally unique like alarms.token.
  token                TEXT    NOT NULL UNIQUE,
  -- ActivityKit activity id: the upsert key with region_id (spec section 6.1).
  activity_id          TEXT    NOT NULL,
  -- ActivityKit push token; rotates over the activity's lifetime. Not the
  -- device alert token.
  push_token           TEXT    NOT NULL,
  apns_sandbox         BOOLEAN NOT NULL DEFAULT FALSE,
  stop_id              TEXT    NOT NULL,
  route_short_name     TEXT    NOT NULL,
  trip_headsign        TEXT    NOT NULL,
  -- Optional trip metadata, stored as sent. '' / 0 mean omitted;
  -- stop_sequence needs NULL because 0 is a real value.
  trip_id              TEXT    NOT NULL DEFAULT '',
  service_date         INTEGER NOT NULL DEFAULT 0,
  vehicle_id           TEXT    NOT NULL DEFAULT '',
  stop_sequence        INTEGER,
  -- Canonical JSON of the last pushed content state (design spec section 3).
  last_content_state   TEXT    NOT NULL DEFAULT '{"arrivals":[]}',
  last_pushed_at       INTEGER,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  expires_at           INTEGER NOT NULL,
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

-- UNIQUE (not just an index) so the concurrent first-registration race
-- surfaces as a constraint violation the adapter maps to ErrDuplicate.
CREATE UNIQUE INDEX live_activities_activity_idx ON live_activities (region_id, activity_id);
-- The feedback webhook deletes by push token.
CREATE INDEX live_activities_push_token_idx ON live_activities (push_token);

-- +goose Down
DROP TABLE live_activities;
```

- [ ] **Step 2: Queries**

`internal/store/sqlite/queries/liveactivities.sql`:

```sql
-- name: InsertLiveActivity :one
INSERT INTO live_activities (
  region_id, token, activity_id, push_token, apns_sandbox,
  stop_id, route_short_name, trip_headsign, trip_id, service_date, vehicle_id, stop_sequence,
  expires_at, created_at, updated_at
) VALUES (
  @region_id, @token, @activity_id, @push_token, @apns_sandbox,
  @stop_id, @route_short_name, @trip_headsign, @trip_id, @service_date, @vehicle_id, @stop_sequence,
  @expires_at, @now, @now
)
RETURNING *;

-- UpdateLiveActivityRegistration rewrites only the registration fields
-- (design spec section 2.1): token, expires_at, last_content_state,
-- last_pushed_at and consecutive_failures are deliberately untouched.

-- name: UpdateLiveActivityRegistration :one
UPDATE live_activities SET
  push_token = @push_token, apns_sandbox = @apns_sandbox,
  stop_id = @stop_id, route_short_name = @route_short_name, trip_headsign = @trip_headsign,
  trip_id = @trip_id, service_date = @service_date, vehicle_id = @vehicle_id, stop_sequence = @stop_sequence,
  updated_at = @now
WHERE region_id = @region_id AND activity_id = @activity_id
RETURNING *;

-- name: DeleteLiveActivityByToken :execrows
DELETE FROM live_activities WHERE region_id = @region_id AND token = @token;

-- name: DeleteLiveActivityByID :execrows
DELETE FROM live_activities WHERE id = @id;

-- name: DeleteLiveActivitiesByPushToken :execrows
DELETE FROM live_activities WHERE push_token = @push_token;

-- name: ListLiveActivities :many
SELECT * FROM live_activities ORDER BY id;

-- RecordLiveActivityFailure / ResetLiveActivityFailures stamp updated_at with
-- SQLite's own unixepoch(): the repository methods take no now (same
-- reasoning as queries/alarms.sql).

-- name: RecordLiveActivityFailure :one
UPDATE live_activities SET consecutive_failures = consecutive_failures + 1, updated_at = unixepoch()
WHERE id = @id
RETURNING consecutive_failures;

-- name: ResetLiveActivityFailures :exec
UPDATE live_activities SET consecutive_failures = 0, updated_at = unixepoch()
WHERE id = @id AND consecutive_failures <> 0;

-- name: RecordLiveActivityPush :exec
UPDATE live_activities SET last_content_state = @last_content_state, last_pushed_at = @pushed_at, updated_at = @pushed_at
WHERE id = @id;
```

Run: `make generate && make generate-check`
Expected: `gen/liveactivities.sql.go` and `gen/models.go` (`LiveActivity` struct) appear; diff clean.

- [ ] **Step 3: Write the failing conformance suite**

`internal/store/storetest/liveactivitytest.go`:

```go
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/regions"
)

type newLiveActivityStoreFunc func(*testing.T) (liveactivities.Repository, regions.Repository)

// RunLiveActivityRepository exercises a liveactivities.Repository against
// the behavioral contract every engine must satisfy.
func RunLiveActivityRepository(t *testing.T, newStore newLiveActivityStoreFunc) {
	t.Helper()
	t.Run("UpsertInsertsAndRoundTrips", func(t *testing.T) { testLAUpsertInserts(t, newStore) })
	t.Run("UpsertUpdatesRegistrationAndPreservesBookkeeping", func(t *testing.T) { testLAUpsertUpdates(t, newStore) })
	t.Run("UpsertIsRegionScoped", func(t *testing.T) { testLAUpsertRegionScoped(t, newStore) })
	t.Run("DeleteByTokenReports204Contract", func(t *testing.T) { testLADeleteByToken(t, newStore) })
	t.Run("DeleteByPushTokenCountsRows", func(t *testing.T) { testLADeleteByPushToken(t, newStore) })
	t.Run("FailureCounterIncrementsAndResets", func(t *testing.T) { testLAFailureCounter(t, newStore) })
	t.Run("RecordPushRoundTripsStateAndInstant", func(t *testing.T) { testLARecordPush(t, newStore) })
	t.Run("DeleteByIDTreatsMissingAsSuccess", func(t *testing.T) { testLADeleteByIDMissing(t, newStore) })
}

func fullLAIn(token, activityID string) liveactivities.NewLiveActivity {
	return liveactivities.NewLiveActivity{
		RegionID: 1, Token: token, ExpiresAt: base.Add(8 * time.Hour),
		ActivityID: activityID, PushToken: "push-" + activityID, APNSSandbox: true,
		StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
		TripID: "1_604370", ServiceDate: 1754809200000, VehicleID: "1_4361", StopSequence: ptr(int64(0)),
	}
}

func findLAByToken(t *testing.T, repo liveactivities.Repository, token string) liveactivities.LiveActivity {
	t.Helper()
	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, la := range list {
		if la.Token == token {
			return la
		}
	}
	t.Fatalf("List() missing token %q (got %+v)", token, list)
	return liveactivities.LiveActivity{}
}

func testLAUpsertInserts(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	in := fullLAIn("tok-a", "act-a")
	created, err := repo.Upsert(ctx, in, base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := findLAByToken(t, repo, "tok-a")
	if got.ID != created.ID || got.Token != "tok-a" || got.ActivityID != "act-a" || got.PushToken != "push-act-a" ||
		!got.APNSSandbox || got.StopID != "1_570" || got.RouteShortName != "44" || got.TripHeadsign != "Ballard" ||
		got.TripID != "1_604370" || got.ServiceDate != 1754809200000 || got.VehicleID != "1_4361" ||
		got.StopSequence == nil || *got.StopSequence != 0 {
		t.Errorf("round trip lost a field: %+v", got)
	}
	if !got.ExpiresAt.Equal(base.Add(8*time.Hour)) || !got.CreatedAt.Equal(base) {
		t.Errorf("ExpiresAt=%v CreatedAt=%v", got.ExpiresAt, got.CreatedAt)
	}
	if got.LastPushedAt != nil || got.ConsecutiveFailures != 0 || len(got.LastContentState.Arrivals) != 0 {
		t.Errorf("fresh row bookkeeping: %+v", got)
	}
}

func testLAUpsertUpdates(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	first, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := repo.RecordFailure(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	state := liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{{DepartureTime: 1, ScheduleStatus: "on_time"}}}
	if err := repo.RecordPush(ctx, first.ID, state, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	in := fullLAIn("tok-IGNORED", "act-a") // same activity, rotated token
	in.PushToken = "push-rotated"
	in.APNSSandbox = false
	in.StopSequence = nil
	in.ExpiresAt = base.Add(99 * time.Hour)
	second, err := repo.Upsert(ctx, in, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	if second.ID != first.ID || second.Token != "tok-a" {
		t.Errorf("update must keep id/token: first=%+v second=%+v", first, second)
	}
	got := findLAByToken(t, repo, "tok-a")
	if got.PushToken != "push-rotated" || got.APNSSandbox || got.StopSequence != nil {
		t.Errorf("registration fields not rewritten: %+v", got)
	}
	if !got.ExpiresAt.Equal(base.Add(8*time.Hour)) || got.ConsecutiveFailures != 1 ||
		got.LastPushedAt == nil || !got.LastPushedAt.Equal(base.Add(time.Minute)) ||
		len(got.LastContentState.Arrivals) != 1 {
		t.Errorf("update must preserve expiry and bookkeeping: %+v", got)
	}
	list, _ := repo.List(ctx)
	if len(list) != 1 {
		t.Errorf("update must not insert a second row: %d rows", len(list))
	}
}

func testLAUpsertRegionScoped(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)
	if _, err := repo.Upsert(ctx, fullLAIn("tok-1", "act-a"), base); err != nil {
		t.Fatal(err)
	}
	in := fullLAIn("tok-2", "act-a")
	in.RegionID = 2
	if _, err := repo.Upsert(ctx, in, base); err != nil {
		t.Fatalf("same activity id in another region must insert: %v", err)
	}
	list, _ := repo.List(ctx)
	if len(list) != 2 {
		t.Errorf("want 2 rows, got %d", len(list))
	}
}

func testLADeleteByToken(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)
	if _, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, 2, "tok-a"); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("wrong region: err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, 1, "tok-a"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := repo.Delete(ctx, 1, "tok-a"); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func testLADeleteByPushToken(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	a := fullLAIn("tok-a", "act-a")
	b := fullLAIn("tok-b", "act-b")
	b.PushToken = a.PushToken
	c := fullLAIn("tok-c", "act-c")
	for _, in := range []liveactivities.NewLiveActivity{a, b, c} {
		if _, err := repo.Upsert(ctx, in, base); err != nil {
			t.Fatal(err)
		}
	}
	n, err := repo.DeleteByPushToken(ctx, a.PushToken)
	if err != nil || n != 2 {
		t.Errorf("DeleteByPushToken = %d, %v; want 2, nil", n, err)
	}
	n, err = repo.DeleteByPushToken(ctx, "nope")
	if err != nil || n != 0 {
		t.Errorf("unknown token = %d, %v; want 0, nil", n, err)
	}
	if list, _ := repo.List(ctx); len(list) != 1 || list[0].Token != "tok-c" {
		t.Errorf("survivor: %+v", list)
	}
}

func testLAFailureCounter(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	la, _ := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	for want := int64(1); want <= 3; want++ {
		got, err := repo.RecordFailure(ctx, la.ID)
		if err != nil || got != want {
			t.Fatalf("RecordFailure #%d = %d, %v", want, got, err)
		}
	}
	if err := repo.ResetFailures(ctx, la.ID); err != nil {
		t.Fatal(err)
	}
	if got := findLAByToken(t, repo, "tok-a"); got.ConsecutiveFailures != 0 {
		t.Errorf("after reset: %d", got.ConsecutiveFailures)
	}
	if _, err := repo.RecordFailure(ctx, 999999); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("missing row: err = %v, want ErrNotFound", err)
	}
}

func testLARecordPush(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	la, _ := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	state := liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{
		{DepartureTime: 1767980460, ScheduleStatus: "on_time", ScheduleDeviation: 60, IsArrival: false},
		{DepartureTime: 1767981000, ScheduleStatus: "delayed", ScheduleDeviation: 240, IsArrival: true},
	}}
	at := base.Add(90 * time.Second)
	if err := repo.RecordPush(ctx, la.ID, state, at); err != nil {
		t.Fatal(err)
	}
	got := findLAByToken(t, repo, "tok-a")
	if got.LastPushedAt == nil || !got.LastPushedAt.Equal(at) {
		t.Errorf("LastPushedAt = %v, want %v", got.LastPushedAt, at)
	}
	if liveactivities.Changed(got.LastContentState, state) {
		t.Errorf("state round trip: got %+v want %+v", got.LastContentState, state)
	}
}

func testLADeleteByIDMissing(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, _ := newStore(t)
	if err := repo.DeleteByID(context.Background(), 424242); err != nil {
		t.Errorf("DeleteByID(missing) = %v, want nil", err)
	}
}
```

Hook it up in `internal/store/sqlite/store_test.go` next to `TestAlarmRepositoryConformance`:

```go
// TestLiveActivityRepositoryConformance runs the shared live activity suite
// against the SQLite adapter (design spec §8).
func TestLiveActivityRepositoryConformance(t *testing.T) {
	t.Parallel()
	storetest.RunLiveActivityRepository(t, func(t *testing.T) (liveactivities.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.LiveActivities(), store.Regions()
	})
}
```

and add to `TestMigrateDeclaresTimeColumnsAsInteger`'s map:

```go
		"live_activities":    {"service_date", "last_pushed_at", "expires_at", "created_at", "updated_at"},
```

- [ ] **Step 4: Run to verify failure**

Run: `go test ./internal/store/...`
Expected: compile error `store.LiveActivities undefined`.

- [ ] **Step 5: Adapter**

`internal/store/sqlite/liveactivities.go`:

```go
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// liveActivityRepo implements liveactivities.Repository. Error strings never
// embed tokens (device-addressable secrets, spec §13); callers still wrap
// with sanitizeToken as defense in depth.
type liveActivityRepo struct {
	q      *gen.Queries
	logger *slog.Logger
}

func (r *liveActivityRepo) fromRow(row gen.LiveActivity) liveactivities.LiveActivity {
	state := liveactivities.EmptyContentState()
	if err := json.Unmarshal([]byte(row.LastContentState), &state); err != nil || state.Arrivals == nil {
		// One corrupt cell must not fail List for every subscription; the
		// next successful push overwrites it (design spec §3).
		r.logger.Warn("sqlite: unparseable live activity content state; treating as empty",
			"id", row.ID, "region_id", row.RegionID, "err", err)
		state = liveactivities.EmptyContentState()
	}
	return liveactivities.LiveActivity{
		ID: row.ID, RegionID: row.RegionID, Token: row.Token, ActivityID: row.ActivityID,
		PushToken: row.PushToken, APNSSandbox: row.ApnsSandbox,
		StopID: row.StopID, RouteShortName: row.RouteShortName, TripHeadsign: row.TripHeadsign,
		TripID: row.TripID, ServiceDate: row.ServiceDate, VehicleID: row.VehicleID,
		StopSequence:        nullInt64ToPtr(row.StopSequence),
		LastContentState:    state,
		LastPushedAt:        nullUnixToTime(row.LastPushedAt),
		ConsecutiveFailures: row.ConsecutiveFailures,
		ExpiresAt:           unixToTime(row.ExpiresAt),
		CreatedAt:           unixToTime(row.CreatedAt),
	}
}

// Upsert tries the update first; sql.ErrNoRows means no such registration,
// so it inserts. Two concurrent first registrations both miss the update;
// the loser's insert violates live_activities_activity_idx and surfaces as
// ErrDuplicate for the caller's single retry (design spec §2.1).
func (r *liveActivityRepo) Upsert(ctx context.Context, in liveactivities.NewLiveActivity, now time.Time) (liveactivities.LiveActivity, error) {
	ts := now.Unix()
	row, err := r.q.UpdateLiveActivityRegistration(ctx, gen.UpdateLiveActivityRegistrationParams{
		PushToken: in.PushToken, ApnsSandbox: in.APNSSandbox,
		StopID: in.StopID, RouteShortName: in.RouteShortName, TripHeadsign: in.TripHeadsign,
		TripID: in.TripID, ServiceDate: in.ServiceDate, VehicleID: in.VehicleID,
		StopSequence: int64ToNullInt64(in.StopSequence),
		Now: ts, RegionID: in.RegionID, ActivityID: in.ActivityID,
	})
	switch {
	case err == nil:
		return r.fromRow(row), nil
	case !errors.Is(err, sql.ErrNoRows):
		return liveactivities.LiveActivity{}, fmt.Errorf("sqlite: update live activity (region %d): %w", in.RegionID, err)
	}
	row, err = r.q.InsertLiveActivity(ctx, gen.InsertLiveActivityParams{
		RegionID: in.RegionID, Token: in.Token, ActivityID: in.ActivityID,
		PushToken: in.PushToken, ApnsSandbox: in.APNSSandbox,
		StopID: in.StopID, RouteShortName: in.RouteShortName, TripHeadsign: in.TripHeadsign,
		TripID: in.TripID, ServiceDate: in.ServiceDate, VehicleID: in.VehicleID,
		StopSequence: int64ToNullInt64(in.StopSequence),
		ExpiresAt: in.ExpiresAt.Unix(), Now: ts,
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed: live_activities.region_id") {
			return liveactivities.LiveActivity{}, fmt.Errorf("sqlite: insert live activity (region %d): %w", in.RegionID, liveactivities.ErrDuplicate)
		}
		return liveactivities.LiveActivity{}, fmt.Errorf("sqlite: insert live activity (region %d): %w", in.RegionID, err)
	}
	return r.fromRow(row), nil
}

func (r *liveActivityRepo) Delete(ctx context.Context, regionID int64, token string) error {
	n, err := r.q.DeleteLiveActivityByToken(ctx, gen.DeleteLiveActivityByTokenParams{RegionID: regionID, Token: token})
	if err != nil {
		return fmt.Errorf("sqlite: delete live activity (region %d): %w", regionID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete live activity (region %d): %w", regionID, liveactivities.ErrNotFound)
	}
	return nil
}

// DeleteByID treats zero rows as success: the updater may race a rider's
// own DELETE, and either way the row being gone is the goal.
func (r *liveActivityRepo) DeleteByID(ctx context.Context, id int64) error {
	if _, err := r.q.DeleteLiveActivityByID(ctx, id); err != nil {
		return fmt.Errorf("sqlite: delete live activity %d: %w", id, err)
	}
	return nil
}

func (r *liveActivityRepo) DeleteByPushToken(ctx context.Context, pushToken string) (int64, error) {
	n, err := r.q.DeleteLiveActivitiesByPushToken(ctx, pushToken)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete live activities by push token: %w", err)
	}
	return n, nil
}

func (r *liveActivityRepo) List(ctx context.Context) ([]liveactivities.LiveActivity, error) {
	rows, err := r.q.ListLiveActivities(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list live activities: %w", err)
	}
	out := make([]liveactivities.LiveActivity, len(rows))
	for i, row := range rows {
		out[i] = r.fromRow(row)
	}
	return out, nil
}

func (r *liveActivityRepo) RecordFailure(ctx context.Context, id int64) (int64, error) {
	n, err := r.q.RecordLiveActivityFailure(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sqlite: record failure for live activity %d: %w", id, liveactivities.ErrNotFound)
		}
		return 0, fmt.Errorf("sqlite: record failure for live activity %d: %w", id, err)
	}
	return n, nil
}

func (r *liveActivityRepo) ResetFailures(ctx context.Context, id int64) error {
	if err := r.q.ResetLiveActivityFailures(ctx, id); err != nil {
		return fmt.Errorf("sqlite: reset failures for live activity %d: %w", id, err)
	}
	return nil
}

func (r *liveActivityRepo) RecordPush(ctx context.Context, id int64, state liveactivities.ContentState, pushedAt time.Time) error {
	if state.Arrivals == nil {
		state = liveactivities.EmptyContentState()
	}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("sqlite: marshal content state for live activity %d: %w", id, err)
	}
	err = r.q.RecordLiveActivityPush(ctx, gen.RecordLiveActivityPushParams{
		LastContentState: string(b), PushedAt: sql.NullInt64{Int64: pushedAt.Unix(), Valid: true}, ID: id,
	})
	if err != nil {
		return fmt.Errorf("sqlite: record push for live activity %d: %w", id, err)
	}
	return nil
}
```

If sqlc names `@pushed_at` differently in `gen.RecordLiveActivityPushParams` (it types it from the first column it binds, `last_pushed_at INTEGER` nullable → `sql.NullInt64`; `updated_at` is NOT NULL, so sqlc may instead choose `int64` — read the generated struct and match). If it generates `int64`, pass `pushedAt.Unix()` directly.

In `store.go`, after `Alarms()`:

```go
// LiveActivities returns the liveactivities.Repository backed by this store
// (spec §6).
func (s *Store) LiveActivities() liveactivities.Repository {
	return &liveActivityRepo{q: s.q, logger: slog.Default()}
}
```

`slog.Default()` is acceptable here (the adapter has no logger today); import `log/slog` and `internal/liveactivities`.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/store/...`
Expected: PASS.

- [ ] **Step 7: Mutation check**

In `UpdateLiveActivityRegistration`, add `consecutive_failures = 0,` to the SET list, regenerate; `UpsertUpdatesRegistrationAndPreservesBookkeeping` must fail. Revert and regenerate.

- [ ] **Step 8: Lint, generate-check, commit**

```bash
make lint && make generate-check
git add internal/store
git commit -m "feat(store): live_activities table, queries, adapter, and conformance suite"
```

---

### Task 5: `liveactivities.Updater` — the §6.3 update loop

**Files:**
- Create: `internal/liveactivities/updater.go`
- Test: `internal/liveactivities/updater_test.go`

**Interfaces:**
- Consumes: `Repository`, `ContentState`, `BuildContentState`, `Changed`, constants (Task 3); `obaapi.StopArrivalsQuery`/`StopArrival` (Task 2); `push.LiveActivityPush`/`LiveActivitySender` (Task 1); `regions.Repository`; `cache.New`.
- Produces:
  ```go
  type ArrivalsSource interface { ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) }
  type Updater struct { Repo Repository; Regions regions.Repository; OBA ArrivalsSource; Sender push.LiveActivitySender; Now func() time.Time; Logger *slog.Logger }
  func NewUpdater(repo Repository, regionRepo regions.Repository, oba ArrivalsSource, sender push.LiveActivitySender, now func() time.Time, logger *slog.Logger) *Updater
  func (u *Updater) CheckAll(ctx context.Context)
  func (u *Updater) RunLoop(ctx context.Context, interval time.Duration)
  ```
  (`NewUpdater` exists because the per-stop cache must be constructed with the injected clock; the exported fields stay so tests can read them.)

- [ ] **Step 1: Write the failing tests**

`internal/liveactivities/updater_test.go`:

```go
package liveactivities_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// --- fakes ---------------------------------------------------------------

type fakeRepo struct {
	mu   sync.Mutex
	rows map[int64]liveactivities.LiveActivity
}

func newFakeRepo(rows ...liveactivities.LiveActivity) *fakeRepo {
	r := &fakeRepo{rows: map[int64]liveactivities.LiveActivity{}}
	for _, la := range rows {
		r.rows[la.ID] = la
	}
	return r
}

func (r *fakeRepo) Upsert(context.Context, liveactivities.NewLiveActivity, time.Time) (liveactivities.LiveActivity, error) {
	return liveactivities.LiveActivity{}, errors.New("not implemented")
}
func (r *fakeRepo) Delete(context.Context, int64, string) error { return liveactivities.ErrNotFound }
func (r *fakeRepo) DeleteByID(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, id)
	return nil
}
func (r *fakeRepo) DeleteByPushToken(context.Context, string) (int64, error) { return 0, nil }
func (r *fakeRepo) List(context.Context) ([]liveactivities.LiveActivity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]liveactivities.LiveActivity, 0, len(r.rows))
	for _, la := range r.rows {
		out = append(out, la)
	}
	return out, nil
}
func (r *fakeRepo) RecordFailure(_ context.Context, id int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	la, ok := r.rows[id]
	if !ok {
		return 0, liveactivities.ErrNotFound
	}
	la.ConsecutiveFailures++
	r.rows[id] = la
	return la.ConsecutiveFailures, nil
}
func (r *fakeRepo) ResetFailures(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	la := r.rows[id]
	la.ConsecutiveFailures = 0
	r.rows[id] = la
	return nil
}
func (r *fakeRepo) RecordPush(_ context.Context, id int64, state liveactivities.ContentState, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	la := r.rows[id]
	la.LastContentState = state
	la.LastPushedAt = &at
	r.rows[id] = la
	return nil
}
func (r *fakeRepo) get(id int64) (liveactivities.LiveActivity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	la, ok := r.rows[id]
	return la, ok
}

type fakeRegions struct {
	regions map[int64]regions.Region
	err     error // non-nil: every Get fails with this
}

func (f fakeRegions) Get(_ context.Context, id int64) (regions.Region, error) {
	if f.err != nil {
		return regions.Region{}, f.err
	}
	r, ok := f.regions[id]
	if !ok {
		return regions.Region{}, regions.ErrNotFound
	}
	return r, nil
}
func (f fakeRegions) List(context.Context) ([]regions.Region, error) { return nil, nil }
func (f fakeRegions) UpsertFromDirectory(context.Context, []regions.Region, time.Time) error {
	return nil
}
func (f fakeRegions) SetLocalFields(context.Context, int64, regions.LocalFields, time.Time) error {
	return nil
}

type fakeOBA struct {
	mu    sync.Mutex
	calls int
	fn    func(q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error)
}

func (f *fakeOBA) ArrivalsAndDeparturesForStop(_ context.Context, _ regions.Region, q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fn(q)
}
func (f *fakeOBA) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fakeSender struct {
	mu   sync.Mutex
	sent []push.LiveActivityPush
	err  error
}

func (s *fakeSender) SendLiveActivity(_ context.Context, p push.LiveActivityPush) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, p)
	return nil
}
func (s *fakeSender) pushes() []push.LiveActivityPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]push.LiveActivityPush(nil), s.sent...)
}

// clock is an advancing fake: a fixed Now would never expire the stop cache
// or advance the push timestamp (design spec §8).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var base = time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)

func upcoming(offset time.Duration) obaapi.StopArrival {
	e := entry(offset, 0) // from contentstate_test.go; measured against `now`
	// entry() is relative to contentstate_test's `now`; rebase onto base.
	shift := base.Sub(now)
	e.ScheduledArrivalTime += shift.Milliseconds()
	e.PredictedArrivalTime += shift.Milliseconds()
	e.ScheduledDepartureTime += shift.Milliseconds()
	e.PredictedDepartureTime += shift.Milliseconds()
	return e
}

func activity(id int64, stop string) liveactivities.LiveActivity {
	return liveactivities.LiveActivity{
		ID: id, RegionID: 1, Token: "tok", ActivityID: "act", PushToken: "push-" + stop, APNSSandbox: true,
		StopID: stop, RouteShortName: "44", TripHeadsign: "Ballard",
		LastContentState: liveactivities.EmptyContentState(), ExpiresAt: base.Add(8 * time.Hour),
	}
}

type harness struct {
	repo   *fakeRepo
	oba    *fakeOBA
	sender *fakeSender
	clk    *clock
	u      *liveactivities.Updater
}

func newHarness(t *testing.T, sender *fakeSender, rows ...liveactivities.LiveActivity) *harness {
	t.Helper()
	h := &harness{
		repo:   newFakeRepo(rows...),
		oba:    &fakeOBA{fn: func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return []obaapi.StopArrival{upcoming(5 * time.Minute)}, nil }},
		sender: sender,
		clk:    &clock{t: base},
	}
	var s push.LiveActivitySender
	if sender != nil {
		s = sender
	}
	h.u = liveactivities.NewUpdater(h.repo,
		fakeRegions{regions: map[int64]regions.Region{1: {ID: 1, OBABaseURL: "https://example.org/", OBAAPIKey: "k"}}},
		h.oba, s, h.clk.now, slog.New(slog.DiscardHandler))
	return h
}

// cycle runs one CheckAll after advancing the clock one minute, like the
// production ticker.
func (h *harness) cycle() { h.clk.advance(time.Minute); h.u.CheckAll(context.Background()) }

// --- tests ---------------------------------------------------------------

func TestFirstCyclePushesUpdateWithStaleDateAndRecords(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	p := s.pushes()
	if len(p) != 1 {
		t.Fatalf("pushes = %d, want 1", len(p))
	}
	if p[0].Event != "update" || p[0].Token != "push-1_570" || !p[0].Sandbox {
		t.Errorf("push = %+v", p[0])
	}
	if !p[0].StaleDate.Equal(h.clk.now().Add(liveactivities.StaleAfter)) || !p[0].DismissalDate.IsZero() {
		t.Errorf("stale=%v dismissal=%v", p[0].StaleDate, p[0].DismissalDate)
	}
	if !p[0].Timestamp.Equal(h.clk.now()) {
		t.Errorf("timestamp = %v, want %v", p[0].Timestamp, h.clk.now())
	}
	state, ok := p[0].ContentState.(liveactivities.ContentState)
	if !ok || len(state.Arrivals) != 1 {
		t.Errorf("content state = %#v", p[0].ContentState)
	}
	row, _ := h.repo.get(1)
	if row.LastPushedAt == nil || !row.LastPushedAt.Equal(h.clk.now()) || len(row.LastContentState.Arrivals) != 1 {
		t.Errorf("RecordPush not applied: %+v", row)
	}
}

func TestKeepaliveBoundaryAndUnchangedState(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle() // first push at base+60s
	h.clk.advance(54 * time.Second)
	h.u.CheckAll(context.Background())
	if len(s.pushes()) != 1 {
		t.Fatalf("unchanged state at 54s must not push; got %d", len(s.pushes()))
	}
	h.clk.advance(time.Second) // 55s since last push
	h.u.CheckAll(context.Background())
	if len(s.pushes()) != 2 {
		t.Fatalf("keepalive at 55s must push; got %d", len(s.pushes()))
	}
}

func TestChangedStateInsideKeepaliveWindowPushes(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
		return []obaapi.StopArrival{upcoming(9 * time.Minute)}, nil
	}
	h.clk.advance(liveactivities.StopCacheTTL + time.Second) // past the cache, inside keepalive? no: 56s > 55s
	// Use a fresh stop-cache miss but keep the keepalive from firing by
	// recording a push "just now".
	row, _ := h.repo.get(1)
	_ = h.repo.RecordPush(context.Background(), 1, row.LastContentState, h.clk.now())
	h.u.CheckAll(context.Background())
	if len(s.pushes()) != 2 {
		t.Fatalf("changed state must push even inside the keepalive window; got %d", len(s.pushes()))
	}
}

func TestTimestampAdvancesWhenClockDoesNot(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	row, _ := h.repo.get(1)
	// Force a keepalive without moving the clock: backdate last push.
	old := h.clk.now().Add(-time.Hour)
	_ = h.repo.RecordPush(context.Background(), 1, row.LastContentState, old)
	// Now pretend the previous push was stamped at exactly now.
	_ = h.repo.RecordPush(context.Background(), 1, liveactivities.EmptyContentState(), h.clk.now())
	h.clk.advance(liveactivities.StopCacheTTL + time.Second)
	h.clk.advance(-(liveactivities.StopCacheTTL + time.Second)) // net zero; cache still warm, state changed vs empty
	h.u.CheckAll(context.Background())
	p := s.pushes()
	if len(p) != 2 {
		t.Fatalf("pushes = %d", len(p))
	}
	if !p[1].Timestamp.After(p[0].Timestamp) || !p[1].Timestamp.Equal(h.clk.now().Add(time.Second)) {
		t.Errorf("timestamp must be last_pushed_at+1s when the clock has not moved: got %v (prev %v, now %v)", p[1].Timestamp, p[0].Timestamp, h.clk.now())
	}
}

func TestExpiredEndsWithEndPushThenDelete(t *testing.T) {
	s := &fakeSender{}
	la := activity(1, "1_570")
	la.ExpiresAt = base.Add(30 * time.Second)
	la.LastContentState = liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{{DepartureTime: 1, ScheduleStatus: "on_time"}}}
	h := newHarness(t, s, la)
	h.cycle()
	p := s.pushes()
	if len(p) != 1 || p[0].Event != "end" {
		t.Fatalf("pushes = %+v, want one end", p)
	}
	if !p[0].DismissalDate.Equal(h.clk.now().Add(liveactivities.DismissAfterEnd)) || !p[0].StaleDate.IsZero() {
		t.Errorf("end push dates: %+v", p[0])
	}
	if st := p[0].ContentState.(liveactivities.ContentState); len(st.Arrivals) != 1 {
		t.Errorf("end push must reuse last state: %+v", st)
	}
	if _, ok := h.repo.get(1); ok {
		t.Error("expired row must be deleted")
	}
	if h.oba.count() != 0 {
		t.Error("expiry must not fetch upstream")
	}
}

func TestThreeEmptyCyclesEndTwoDoNot(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, nil }
	h.cycle()
	h.cycle()
	row, ok := h.repo.get(1)
	if !ok || row.ConsecutiveFailures != 2 || len(s.pushes()) != 0 {
		t.Fatalf("after 2 empties: ok=%v row=%+v pushes=%d", ok, row, len(s.pushes()))
	}
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("third empty cycle must end the activity")
	}
	if p := s.pushes(); len(p) != 1 || p[0].Event != "end" {
		t.Errorf("want one end push, got %+v", p)
	}
}

func TestFetchErrorsCountAndSuccessResets(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, errors.New("boom") }
	h.cycle()
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, obaapi.ErrNotFound }
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 2 {
		t.Fatalf("transient and not-found must both count (spec §6.3): %+v", row)
	}
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return []obaapi.StopArrival{upcoming(5 * time.Minute)}, nil }
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 0 {
		t.Errorf("success must reset: %+v", row)
	}
}

func TestSendFailureLeavesRowAndLastPushedAt(t *testing.T) {
	s := &fakeSender{err: errors.New("gorush down")}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	row, ok := h.repo.get(1)
	if !ok || row.LastPushedAt != nil || row.ConsecutiveFailures != 0 {
		t.Errorf("send failure must not record a push or count a failure: ok=%v %+v", ok, row)
	}
}

func TestEndPushFailureStillDeletes(t *testing.T) {
	s := &fakeSender{err: errors.New("gorush down")}
	la := activity(1, "1_570")
	la.ExpiresAt = base
	h := newHarness(t, s, la)
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("row must be deleted even when the end push fails (spec §6.4)")
	}
}

func TestStoreOnlyModeExpiresWithoutSending(t *testing.T) {
	la := activity(1, "1_570")
	la.ExpiresAt = base
	h := newHarness(t, nil, la, activity(2, "1_571"))
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("expired row must be deleted in store-only mode")
	}
	if row, _ := h.repo.get(2); row.LastPushedAt != nil {
		t.Error("store-only mode must not record pushes")
	}
}

func TestRegionErrors(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.u.Regions = fakeRegions{regions: map[int64]regions.Region{}} // region gone
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 1 {
		t.Errorf("ErrNotFound region must count: %+v", row)
	}
	h.u.Regions = fakeRegions{err: errors.New("db locked")}
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 1 {
		t.Errorf("transient region error must not count: %+v", row)
	}
	if h.oba.count() != 0 {
		t.Error("no upstream call without a region")
	}
}

func TestSubscriptionsOnOneStopShareOneFetchPerCycle(t *testing.T) {
	s := &fakeSender{}
	b := activity(2, "1_570")
	b.PushToken = "push-b"
	h := newHarness(t, s, activity(1, "1_570"), b, activity(3, "1_999"))
	h.cycle()
	if h.oba.count() != 2 {
		t.Fatalf("two stops -> two fetches, got %d", h.oba.count())
	}
	if len(s.pushes()) != 3 {
		t.Errorf("every subscription pushes: %d", len(s.pushes()))
	}
	h.cycle() // 60s later: cache (55s) expired
	if h.oba.count() != 4 {
		t.Errorf("cache must expire after 55s: fetches = %d", h.oba.count())
	}
}

func TestQueryWindowIsLookbackAndLookahead(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	var got obaapi.StopArrivalsQuery
	h.oba.fn = func(q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { got = q; return nil, nil }
	h.cycle()
	if got.StopID != "1_570" || got.MinutesBefore != liveactivities.LookbackMinutes || got.MinutesAfter != liveactivities.LookaheadMinutes {
		t.Errorf("query = %+v", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/liveactivities`
Expected: compile error `undefined: liveactivities.NewUpdater`.

- [ ] **Step 3: Implement `updater.go`**

```go
package liveactivities

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// ArrivalsSource is the one obaapi method the updater needs, declared
// consumer-side so tests fake three lines instead of the whole Client.
type ArrivalsSource interface {
	ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error)
}

// checkConcurrency bounds parallel checks per cycle, matching alarms.
const checkConcurrency = 8

// stopCacheEntries bounds the per-stop cache; beyond this many distinct
// stops per 55s the oldest are evicted and refetched.
const stopCacheEntries = 1024

// Updater runs the §6.3 update cycle: once per cycle it lists every
// subscription, builds its content state from a per-stop shared fetch, and
// pushes an update, a keepalive, or an end.
type Updater struct {
	Repo    Repository
	Regions regions.Repository
	OBA     ArrivalsSource
	// Sender may be nil (store-only mode: no push transport configured).
	// Expiry and reaping still run so rows cannot accumulate; nothing is
	// sent or recorded as pushed.
	Sender push.LiveActivitySender
	Now    func() time.Time
	Logger *slog.Logger

	stops *cache.Cache[[]obaapi.StopArrival]
}

// NewUpdater builds an Updater whose per-stop cache reads the injected
// clock (design spec §2.6).
func NewUpdater(repo Repository, regionRepo regions.Repository, oba ArrivalsSource,
	sender push.LiveActivitySender, now func() time.Time, logger *slog.Logger) *Updater {
	return &Updater{
		Repo: repo, Regions: regionRepo, OBA: oba, Sender: sender, Now: now, Logger: logger,
		stops: cache.New[[]obaapi.StopArrival](StopCacheTTL, stopCacheEntries, StopFetchBudget, now),
	}
}

// regionLookup is one cycle's cached resolution of a region (see
// alarms.Scheduler for why the error is kept alongside the region).
type regionLookup struct {
	region *regions.Region
	err    error
}

// CheckAll runs one cycle over every subscription. Exported so tests and
// the loop wiring drive cycles without a ticker.
func (u *Updater) CheckAll(ctx context.Context) {
	rows, err := u.Repo.List(ctx)
	if err != nil {
		u.Logger.Error("liveactivities: list", "err", err)
		return
	}
	regionCache := make(map[int64]regionLookup)
	var mu sync.Mutex
	regionFor := func(id int64) regionLookup {
		mu.Lock()
		r, ok := regionCache[id]
		mu.Unlock()
		if ok {
			return r
		}
		region, err := u.Regions.Get(ctx, id)
		resolved := regionLookup{err: err}
		if err == nil {
			resolved.region = &region
		}
		mu.Lock()
		regionCache[id] = resolved
		mu.Unlock()
		return resolved
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(checkConcurrency)
	for _, la := range rows {
		g.Go(func() error {
			u.check(gctx, la, regionFor(la.RegionID))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		u.Logger.Error("liveactivities: check cycle", "err", err)
	}
}

func (u *Updater) check(ctx context.Context, la LiveActivity, lookup regionLookup) {
	now := u.Now()
	if now.After(la.ExpiresAt) {
		u.end(ctx, la, "expired")
		return
	}
	if lookup.region == nil {
		if !errors.Is(lookup.err, regions.ErrNotFound) {
			// A store hiccup is a fact about the database, not this row.
			u.Logger.Warn("liveactivities: resolve region", "region_id", la.RegionID, "err", lookup.err)
			return
		}
		u.countFailure(ctx, la, "region not found")
		return
	}

	entries, err := u.fetch(ctx, *lookup.region, la.StopID)
	if err != nil {
		// Spec §6.3 step 2: OBA/network errors count, unlike the alarm
		// scheduler -- a Live Activity that cannot be updated is worthless
		// and three minutes is the cutoff. Don't "fix" this to match alarms.
		u.countFailure(ctx, la, "fetch failed")
		return
	}
	state := BuildContentState(entries, la.RouteShortName, la.TripHeadsign, now)
	if len(state.Arrivals) == 0 {
		// Night headways and feed gaps produce valid-but-empty responses on
		// healthy subscriptions; only a streak ends the activity.
		u.countFailure(ctx, la, "no matching upcoming arrivals")
		return
	}
	if la.ConsecutiveFailures > 0 {
		if err := u.Repo.ResetFailures(ctx, la.ID); err != nil {
			u.Logger.Warn("liveactivities: reset failures", "region_id", la.RegionID, "err", err)
		}
	}

	if !Changed(la.LastContentState, state) && !keepaliveDue(la.LastPushedAt, now) {
		return
	}
	if u.Sender == nil {
		return // store-only mode
	}
	err = u.Sender.SendLiveActivity(ctx, push.LiveActivityPush{
		Token: la.PushToken, Sandbox: la.APNSSandbox, Event: "update",
		ContentState: state, Timestamp: pushTimestamp(la.LastPushedAt, now),
		StaleDate: now.Add(StaleAfter),
	})
	if err != nil {
		// Leave the row; next cycle retries. Not a §6.3 failure: the
		// upstream is fine, our transport is not.
		u.Logger.Error("liveactivities: update push failed", "region_id", la.RegionID, "err", err)
		return
	}
	if err := u.Repo.RecordPush(ctx, la.ID, state, now); err != nil {
		u.Logger.Error("liveactivities: record push", "region_id", la.RegionID, "err", err)
	}
}

// fetch shares one upstream call per (region, stop) per StopCacheTTL across
// every subscription on that stop (spec §6.3 cost control).
func (u *Updater) fetch(ctx context.Context, region regions.Region, stopID string) ([]obaapi.StopArrival, error) {
	key := fmt.Sprintf("%d/%s", region.ID, stopID)
	return u.stops.Get(ctx, key, func(ctx context.Context) ([]obaapi.StopArrival, error) {
		return u.OBA.ArrivalsAndDeparturesForStop(ctx, region, obaapi.StopArrivalsQuery{
			StopID: stopID, MinutesBefore: LookbackMinutes, MinutesAfter: LookaheadMinutes,
		})
	})
}

// keepaliveDue reports whether KeepaliveInterval has elapsed (>=, a
// deliberate boundary choice: the intent is to push on every cycle).
func keepaliveDue(lastPushedAt *time.Time, now time.Time) bool {
	return lastPushedAt == nil || now.Sub(*lastPushedAt) >= KeepaliveInterval
}

// pushTimestamp is max(now, last+1s): APNs drops a Live Activity push whose
// timestamp does not advance (design spec §2.5).
func pushTimestamp(lastPushedAt *time.Time, now time.Time) time.Time {
	if lastPushedAt != nil && !now.After(*lastPushedAt) {
		return lastPushedAt.Add(time.Second)
	}
	return now
}

func (u *Updater) countFailure(ctx context.Context, la LiveActivity, reason string) {
	streak, err := u.Repo.RecordFailure(ctx, la.ID)
	if err != nil {
		u.Logger.Warn("liveactivities: record failure", "region_id", la.RegionID, "err", err)
		return
	}
	u.Logger.Warn("liveactivities: "+reason, "region_id", la.RegionID, "stop_id", la.StopID, "streak", streak)
	if streak >= MaxConsecutiveFailures {
		u.end(ctx, la, reason)
	}
}

// end sends a best-effort end push and ALWAYS deletes the row (spec §6.4):
// a dead token must not keep the row being re-checked forever.
func (u *Updater) end(ctx context.Context, la LiveActivity, reason string) {
	now := u.Now()
	u.Logger.Info("liveactivities: ending", "region_id", la.RegionID, "stop_id", la.StopID, "reason", reason)
	if u.Sender != nil {
		state := la.LastContentState
		if state.Arrivals == nil {
			state = EmptyContentState()
		}
		err := u.Sender.SendLiveActivity(ctx, push.LiveActivityPush{
			Token: la.PushToken, Sandbox: la.APNSSandbox, Event: "end",
			ContentState: state, Timestamp: pushTimestamp(la.LastPushedAt, now),
			DismissalDate: now.Add(DismissAfterEnd),
		})
		if err != nil {
			u.Logger.Warn("liveactivities: best-effort end push failed", "region_id", la.RegionID, "err", err)
		}
	}
	if err := u.Repo.DeleteByID(ctx, la.ID); err != nil {
		u.Logger.Error("liveactivities: delete ended activity", "region_id", la.RegionID, "err", err)
	}
}

// RunLoop calls CheckAll every interval until ctx is done (§6.3: once per
// minute). Mirrors alarms.Scheduler.RunLoop.
func (u *Updater) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.CheckAll(ctx)
		}
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/liveactivities -race -v`
Expected: PASS. If `TestTimestampAdvancesWhenClockDoesNot` or `TestChangedStateInsideKeepaliveWindowPushes` prove awkward against the cache, simplify them to: record a push at `now` with the *empty* state (so `Changed` is true, keepalive false), run `CheckAll` without advancing the clock (cache warm from the first cycle is fine — the state differs from the stored empty one), and assert one more push whose `Timestamp == now+1s`. Keep the assertion, not the choreography.

- [ ] **Step 5: Mutation checks**

(a) `>=` → `>` in `keepaliveDue`: `TestKeepaliveBoundaryAndUnchangedState` fails. (b) Remove the `DeleteByID` call from `end`: `TestEndPushFailureStillDeletes` and `TestExpiredEnds…` fail. (c) Make transient fetch errors `return` without counting: `TestFetchErrorsCountAndSuccessResets` fails. Revert each.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/liveactivities
git commit -m "feat(liveactivities): §6.3 updater with per-stop shared fetch, keepalive, and best-effort end"
```

---

### Task 6: HTTP handlers — register (upsert) and delete

**Files:**
- Create: `internal/httpapi/liveactivities.go`
- Modify: `internal/httpapi/router.go` (`Deps` fields + registration block after the `Alarms` block)
- Modify: `internal/httpapi/alarms.go:186-195` (generalize `alarmURL` into `resourceURL`)
- Test: `internal/httpapi/liveactivities_test.go`

**Interfaces:**
- Consumes: `liveactivities.Repository`, `NewLiveActivity`, `Lifetime`, `ErrNotFound`, `ErrDuplicate` (Task 3/4); `parseRequestParams`, `params.str/int64`, `parseAPNSSandbox`, `errorWithMessages`, `resolveRegion`, `writeServerError`, `sanitizeToken`, `throttleByIP`, `maxTokenLen`, `securetoken.New`.
- Produces: `Deps.LiveActivities liveactivities.Repository`, `Deps.LiveActivityLimiter *ratelimit.Limiter`; routes `POST /api/v2/regions/{regionId}/live_activities`, `DELETE /api/v2/regions/{regionId}/live_activities/{liveActivityToken}`; `resourceURL(region, r, path string) string`.

- [ ] **Step 1: Write the failing tests**

`internal/httpapi/liveactivities_test.go`:

```go
package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

func newLiveActivitiesServer(t *testing.T, limiter *ratelimit.Limiter) (http.Handler, liveactivities.Repository, regions.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		LiveActivities:      store.LiveActivities(),
		LiveActivityLimiter: limiter,
		Regions:             store.Regions(),
		Now:                 func() time.Time { return base },
		Logger:              slog.New(slog.DiscardHandler),
	}
	return httpapi.NewRouter(deps), store.LiveActivities(), store.Regions()
}

func laRequest(t *testing.T, h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const laForm = "activity_id=act-1&push_token=ptok-1&stop_id=1_570&route_short_name=44&trip_headsign=Ballard&apns_sandbox=1&trip_id=1_604370&service_date=1754809200000&stop_sequence=0"

var laURLRe = regexp.MustCompile(`^https://sidecar\.example/api/v2/regions/1/live_activities/([A-Za-z0-9_-]{22})$`)

func createURL(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !laURLRe.MatchString(body["url"]) {
		t.Fatalf("url = %q", body["url"])
	}
	return body["url"]
}

func TestLiveActivityCreateFormAndJSON(t *testing.T) {
	h, repo, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")

	url := createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("rows = %d", len(list))
	}
	la := list[0]
	if la.ActivityID != "act-1" || la.PushToken != "ptok-1" || !la.APNSSandbox || la.StopID != "1_570" ||
		la.RouteShortName != "44" || la.TripHeadsign != "Ballard" || la.TripID != "1_604370" ||
		la.ServiceDate != 1754809200000 || la.StopSequence == nil || *la.StopSequence != 0 ||
		!la.ExpiresAt.Equal(base.Add(liveactivities.Lifetime)) || !strings.HasSuffix(url, la.Token) {
		t.Errorf("stored = %+v (url %s)", la, url)
	}

	jsonBody := `{"activity_id":"act-2","push_token":"ptok-2","stop_id":"1_570","route_short_name":"44","trip_headsign":"Ballard","apns_sandbox":"true","stop_sequence":3}`
	createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/json", jsonBody))
	list, _ = repo.List(context.Background())
	if len(list) != 2 {
		t.Errorf("rows = %d", len(list))
	}
}

func TestLiveActivityRePostIsUpsertWithSameURL(t *testing.T) {
	h, repo, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	first := createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	rotated := strings.Replace(laForm, "push_token=ptok-1", "push_token=ptok-rotated", 1)
	rotated = strings.Replace(rotated, "apns_sandbox=1", "apns_sandbox=0", 1)
	second := createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", rotated))
	if first != second {
		t.Errorf("re-registration URL changed: %s -> %s", first, second)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 1 || list[0].PushToken != "ptok-rotated" || list[0].APNSSandbox {
		t.Errorf("upsert did not rewrite token/sandbox: %+v", list)
	}
}

func TestLiveActivityCreateValidation(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	rec := laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/x-www-form-urlencoded", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error    string   `json:"error"`
		Messages []string `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	want := []string{"Activity can't be blank", "Push token can't be blank", "Stop can't be blank",
		"Route short name can't be blank", "Trip headsign can't be blank"}
	if body.Error != "Unable to register live activity" || strings.Join(body.Messages, "|") != strings.Join(want, "|") {
		t.Errorf("body = %+v", body)
	}
	long := strings.Replace(laForm, "push_token=ptok-1", "push_token="+strings.Repeat("x", 4097), 1)
	rec = laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/x-www-form-urlencoded", long)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Push token is too long (maximum is 4096 characters)") {
		t.Errorf("long token: %d %s", rec.Code, rec.Body.String())
	}
	rec = laRequest(t, h, http.MethodPost, "/api/v2/regions/99/live_activities", "application/x-www-form-urlencoded", laForm)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Couldn't find Region") {
		t.Errorf("unknown region: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLiveActivityDelete(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	putRegionWithBaseURL(t, regionRepo, 2, "https://sidecar.example")
	url := createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	path := strings.TrimPrefix(url, "https://sidecar.example")
	token := path[strings.LastIndex(path, "/")+1:]

	if rec := laRequest(t, h, http.MethodDelete, "/api/v2/regions/2/live_activities/"+token, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("wrong region: %d", rec.Code)
	}
	slug := "/api/v2/regions/1-puget-sound/live_activities/" + token
	if rec := laRequest(t, h, http.MethodDelete, slug, "", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("slug delete: %d %q", rec.Code, rec.Body.String())
	}
	if rec := laRequest(t, h, http.MethodDelete, path, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", rec.Code)
	}
}

func TestLiveActivityPostThrottledDeleteNot(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, ratelimit.New(1, time.Minute))
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	url := createURL(t, laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	if rec := laRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second POST: %d, want 429", rec.Code)
	}
	if rec := laRequest(t, h, http.MethodDelete, strings.TrimPrefix(url, "https://sidecar.example"), "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE must not be throttled: %d", rec.Code)
	}
}

func TestLiveActivityURLFallsBackToRequestHost(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://sidecar.local:8080/api/v2/regions/1/live_activities", strings.NewReader(laForm))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"url":"https://sidecar.local:8080/api/v2/regions/1/live_activities/`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestNewRouterPanicsWhenLiveActivitiesLacksRegionsOrNow(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "Deps.Regions") || !strings.Contains(r.(string), "Deps.Now") {
			t.Errorf("recover = %v", r)
		}
	}()
	httpapi.NewRouter(httpapi.Deps{LiveActivities: sqlitetest.Open(t).LiveActivities(), Logger: slog.New(slog.DiscardHandler)})
}
```

`base` and `putRegionWithBaseURL` already exist in the `httpapi_test` package (`alarms_api_test.go`).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/httpapi -run LiveActivity`
Expected: compile error `unknown field LiveActivities in struct literal`.

- [ ] **Step 3: Implement**

In `router.go`, after the `OBA obaapi.Client` field:

```go
	// LiveActivities backs the Live Activity register/delete endpoints
	// (spec §6.1). Nil means those routes are not registered.
	LiveActivities liveactivities.Repository
	// LiveActivityLimiter throttles Live Activity registration POSTs
	// (design spec §4: a sidecar-specific 30/minute per IP, because every
	// distinct stop a registration names costs one upstream call per
	// minute for eight hours). NewRouter defaults it; tests inject tighter.
	LiveActivityLimiter *ratelimit.Limiter
```

After the `Alarms` registration block:

```go
	if deps.LiveActivities != nil {
		if missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.LiveActivities is set")
		}
		if deps.LiveActivityLimiter == nil {
			deps.LiveActivityLimiter = ratelimit.New(liveActivityRegistrationsPerMinute, time.Minute)
		}
		lh := &liveActivitiesHandler{deps: deps}
		mux.HandleFunc("POST /api/v2/regions/{regionId}/live_activities",
			throttleByIP(deps.LiveActivityLimiter, deps, lh.register))
		// DELETE is unthrottled: dismissals are cheap and a throttled
		// dismissal would strand a row until expiry.
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/live_activities/{liveActivityToken}", lh.delete)
	}
```

Import `internal/liveactivities`.

In `alarms.go`, replace `alarmURL` with a general helper and a thin wrapper:

```go
// resourceURL builds a §2.4 creation-response URL for path (which starts
// with "/api/…"). The region's directory sidecarBaseUrl wins; a region
// without one falls back to this request's Host over https (the only scheme
// the apps will talk to us on).
func resourceURL(region regions.Region, r *http.Request, path string) string {
	base := strings.TrimRight(region.SidecarBaseURL, "/")
	if base == "" {
		base = "https://" + r.Host
	}
	return base + path
}

func alarmURL(region regions.Region, r *http.Request, version int, token string) string {
	return resourceURL(region, r, fmt.Sprintf("/api/v%d/regions/%d/alarms/%s", version, region.ID, token))
}
```

`internal/httpapi/liveactivities.go`:

```go
package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/securetoken"
)

// liveActivityRegistrationsPerMinute is the sidecar-specific POST throttle
// (design spec §4).
const liveActivityRegistrationsPerMinute = 30

type liveActivitiesHandler struct{ deps Deps }

// register is the §6.1 upsert on (region, activity_id). Every field is
// re-read on re-registration, including apns_sandbox: a rotated token comes
// from the same build that sent the original. The response is 201 on both
// insert and update; the URL is the same either way so the client's stored
// DELETE URL stays valid across token rotations (design spec §2.1).
func (h *liveActivitiesHandler) register(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, requestBodyLimit)
	if err != nil {
		errorWithMessages(w, h.deps.Logger, "Unable to register live activity", []string{err.Error()})
		return
	}
	var msgs []string
	activityID, _ := p.str("activity_id")
	if activityID == "" {
		msgs = append(msgs, "Activity can't be blank")
	}
	pushToken, _ := p.str("push_token")
	switch {
	case pushToken == "":
		msgs = append(msgs, "Push token can't be blank")
	case len(pushToken) > maxTokenLen:
		// A sidecar addition (the reference only checks presence): a junk
		// token would be stored and pushed to every minute for eight hours.
		msgs = append(msgs, fmt.Sprintf("Push token is too long (maximum is %d characters)", maxTokenLen))
	}
	stopID, _ := p.str("stop_id")
	if stopID == "" {
		msgs = append(msgs, "Stop can't be blank")
	}
	routeShortName, _ := p.str("route_short_name")
	if routeShortName == "" {
		msgs = append(msgs, "Route short name can't be blank")
	}
	tripHeadsign, _ := p.str("trip_headsign")
	if tripHeadsign == "" {
		msgs = append(msgs, "Trip headsign can't be blank")
	}
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to register live activity", msgs)
		return
	}

	tripID, _ := p.str("trip_id")
	vehicleID, _ := p.str("vehicle_id")
	serviceDate, _ := p.int64("service_date") // non-numeric -> 0 = omitted
	var stopSeq *int64
	if v, ok := p.int64("stop_sequence"); ok {
		stopSeq = &v
	}
	token, err := securetoken.New()
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "mint live activity token", err)
		return
	}
	now := h.deps.Now()
	in := liveactivities.NewLiveActivity{
		RegionID: region.ID, Token: token, ExpiresAt: now.Add(liveactivities.Lifetime),
		ActivityID: activityID, PushToken: pushToken, APNSSandbox: parseAPNSSandbox(p, h.deps.Logger),
		StopID: stopID, RouteShortName: routeShortName, TripHeadsign: tripHeadsign,
		TripID: tripID, ServiceDate: serviceDate, VehicleID: vehicleID, StopSequence: stopSeq,
	}
	la, err := h.deps.LiveActivities.Upsert(r.Context(), in, now)
	if errors.Is(err, liveactivities.ErrDuplicate) {
		// Lost the concurrent first-registration race; one row exists now,
		// so a single retry takes the update path (spec §6.1).
		la, err = h.deps.LiveActivities.Upsert(r.Context(), in, now)
	}
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "upsert live activity",
			sanitizeToken(sanitizeToken(err, pushToken), token))
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusCreated, map[string]string{
		"url": resourceURL(region, r, fmt.Sprintf("/api/v2/regions/%d/live_activities/%s", region.ID, la.Token)),
	})
}

// delete is the client-initiated dismissal: 204 only after the row is gone,
// 404 for an unknown token in this region, no end push (spec §6.4).
func (h *liveActivitiesHandler) delete(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	token := r.PathValue("liveActivityToken")
	err := h.deps.LiveActivities.Delete(r.Context(), region.ID, token)
	switch {
	case errors.Is(err, liveactivities.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		writeServerError(w, h.deps.Logger, region.ID, "delete live activity", sanitizeToken(err, token))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/httpapi`
Expected: PASS (including the alarm URL tests, which now go through `resourceURL`).

- [ ] **Step 5: Mutation check**

Remove the `ErrDuplicate` retry; no test fails (the race is not reproducible through HTTP) — that is expected; the storetest `ErrDuplicate` mapping is covered by the adapter's update-first path instead. Change `"Stop can't be blank"` to `"Stop id can't be blank"`; `TestLiveActivityCreateValidation` must fail. Revert.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/httpapi
git commit -m "feat(httpapi): Live Activity register (upsert) and delete endpoints"
```

---

### Task 7: Feedback webhook prunes Live Activities; registered without PushRegs

**Files:**
- Modify: `internal/httpapi/feedback.go:44-58`
- Modify: `internal/httpapi/router.go` (hoist the `/webhooks/gorush` registration out of the `PushRegs` block)
- Test: `internal/httpapi/feedback_test.go`

**Interfaces:**
- Consumes: `Deps.LiveActivities.DeleteByPushToken` (Task 4/6).

- [ ] **Step 1: Write the failing tests**

Append to `internal/httpapi/feedback_test.go`:

```go
func seedLiveActivity(t *testing.T, repo liveactivities.Repository, regionRepo regions.Repository, pushToken string) {
	t.Helper()
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	_, err := repo.Upsert(context.Background(), liveactivities.NewLiveActivity{
		RegionID: 1, Token: "la-" + pushToken, ExpiresAt: base.Add(time.Hour), ActivityID: "act-" + pushToken,
		PushToken: pushToken, StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
	}, base)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedbackTerminalDeletesLiveActivityAndRegistration(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "dead-tok")
	if err := store.PushRegs().Upsert(context.Background(), pushreg.Upsert{RegionID: 1, Token: "dead-tok", OperatingSystem: "ios"}, base); err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs: store.PushRegs(), LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"type":"failed-push","platform":"ios","token":"dead-tok","error":"Unregistered"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 0 {
		t.Errorf("live activity survived terminal feedback: %+v", list)
	}
	if n, _ := store.PushRegs().DeleteByToken(context.Background(), "dead-tok"); n != 0 {
		t.Errorf("registration survived: %d", n)
	}
}

func TestFeedbackRegisteredWithLiveActivitiesOnly(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "dead-tok")
	h := httpapi.NewRouter(httpapi.Deps{
		LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"token":"dead-tok","error":"BadDeviceToken"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (webhook must exist without PushRegs)", rec.Code)
	}
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 0 {
		t.Errorf("live activity survived: %+v", list)
	}
}

func TestFeedbackNonTerminalKeepsLiveActivity(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "ok-tok")
	h := httpapi.NewRouter(httpapi.Deps{
		LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"token":"ok-tok","error":"ExpiredProviderToken"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 1 {
		t.Errorf("non-terminal reason must not delete: %+v", list)
	}
}
```

Add `"github.com/OneBusAway/sidecar/internal/liveactivities"` and `"github.com/OneBusAway/sidecar/internal/regions"` to the imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/httpapi -run 'TestFeedback'`
Expected: `TestFeedbackRegisteredWithLiveActivitiesOnly` fails with status 404; the first fails because the live activity survives.

- [ ] **Step 3: Implement**

In `router.go`, move the feedback registration (from `fh := &feedbackHandler{deps: deps}` through `mux.HandleFunc("POST /webhooks/gorush", feedback)`, with its comment) out of the `PushRegs` block into its own block placed after the `LiveActivities` block:

```go
	if deps.PushRegs != nil || deps.LiveActivities != nil {
		// The feedback webhook prunes whichever token tables are configured
		// (spec §6.5); it must exist for a Live-Activities-only deployment
		// too (design spec §2.8).
		fh := &feedbackHandler{deps: deps}
		feedback := fh.receive
		if deps.FeedbackSecret == "" {
			if deps.FeedbackLimiter == nil {
				deps.FeedbackLimiter = ratelimit.New(feedbackLimitPerMinute, time.Minute)
			}
			feedback = throttleByIP(deps.FeedbackLimiter, deps, feedback)
		}
		mux.HandleFunc("POST /webhooks/gorush", feedback)
	}
```

In `feedback.go`, replace the delete section of `receive`:

```go
	if h.deps.PushRegs != nil {
		n, err := h.deps.PushRegs.DeleteByToken(r.Context(), fb.Token)
		if err != nil {
			h.deps.Logger.Error("httpapi: delete registration from feedback", "err", sanitizeToken(err, fb.Token))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if n > 0 {
			h.deps.Logger.Info("httpapi: pruned dead push token",
				"platform", fb.Platform, "reason", fb.Error, "registrations", n)
		}
	}
	if h.deps.LiveActivities != nil {
		// A terminal ActivityKit token means every future update would
		// bounce: delete the subscription, no end push (spec §6.4/§6.5).
		n, err := h.deps.LiveActivities.DeleteByPushToken(r.Context(), fb.Token)
		if err != nil {
			h.deps.Logger.Error("httpapi: delete live activity from feedback", "err", sanitizeToken(err, fb.Token))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if n > 0 {
			h.deps.Logger.Info("httpapi: retired live activity for dead token",
				"platform", fb.Platform, "reason", fb.Error, "live_activities", n)
		}
	}
	w.WriteHeader(http.StatusOK)
```

Update `receive`'s doc comment to say it prunes both tables.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/httpapi`
Expected: PASS, including the existing feedback tests.

- [ ] **Step 5: Mutation check**

Change `deps.PushRegs != nil || deps.LiveActivities != nil` back to `deps.PushRegs != nil`; `TestFeedbackRegisteredWithLiveActivitiesOnly` must fail. Revert.

- [ ] **Step 6: Lint and commit**

```bash
make lint
git add internal/httpapi
git commit -m "feat(httpapi): gorush feedback retires Live Activities; webhook registered without PushRegs"
```

---

### Task 8: Wiring, docs, full check

**Files:**
- Modify: `cmd/sidecar/main.go` (`buildDeps` return literal; loop start after the alarm scheduler; boot warning)
- Modify: `README.md` (new "Live Activities" section after "Ghost bus reports"; line 7 intro already lists the feature)
- Modify: `CLAUDE.md:41` (package list) and the background-loop list in "Wiring lives only in…"

- [ ] **Step 1: Wire `main.go`**

In `buildDeps`'s returned `httpapi.Deps` add `LiveActivities: store.LiveActivities(),`.

After `go sched.RunLoop(ctx, alarmCheckInterval)`:

```go
	// Live Activities share the alarm cadence (spec §6.3: once per minute)
	// and the same store-only rule: without a sender, rows still expire and
	// reap (design spec §2.5).
	var laSender push.LiveActivitySender
	if sender != nil {
		laSender = sender.(push.LiveActivitySender)
	}
	updater := liveactivities.NewUpdater(store.LiveActivities(), store.Regions(), deps.OBA, laSender, time.Now, logger)
	go updater.RunLoop(ctx, alarmCheckInterval)
```

Rather than the type assertion, change `var sender push.Sender` to `var gorush *push.Gorush` and assign both `Sender: gorush`-style values with an explicit nil check (`if gorush != nil { sender = gorush; laSender = gorush }`), so a nil `*Gorush` never becomes a non-nil interface.

Extend the two boot warnings:
- `"no --gorush-url/SIDECAR_GORUSH_URL set; departure alarms and Live Activities will be stored and reaped but never pushed"`
- `"no --apns-topic/SIDECAR_APNS_TOPIC set; iOS alarm pushes will be rejected by APNs with MissingTopic and Live Activity pushes are refused locally"`

Import `internal/liveactivities`.

Run: `go build ./... && go test ./cmd/...`

- [ ] **Step 2: README**

Insert after the "Ghost bus reports" section (before `## Weather and vehicle search`):

```markdown
## Live Activities

iOS Lock Screen widgets showing the next departures for one bookmarked route +
headsign at one stop (spec §6). The sidecar owns the update cadence: once a
minute per subscription it fetches the stop's arrivals from the region's OBA
server, builds the §6.2 `content-state`, and pushes it through gorush as an
APNs `liveactivity` push (priority 10, topic `<bundle id>.push-type.liveactivity`,
derived from `SIDECAR_APNS_TOPIC`).

### Endpoints

```
POST   /api/v2/regions/{regionId}/live_activities          → 201 {"url": "…/live_activities/<token>"}
DELETE /api/v2/regions/{regionId}/live_activities/{token}  → 204 | 404
```

- Registration is an **upsert on `(region, activity_id)`**: ActivityKit rotates
  push tokens and the app re-POSTs the same activity with each one. The URL is
  the same every time; every field, including `apns_sandbox`, is re-read.
- Required: `activity_id`, `push_token` (≤ 4096 chars — a sidecar addition), `stop_id`,
  `route_short_name`, `trip_headsign`. Optional trip metadata: `trip_id`,
  `service_date` (epoch ms), `vehicle_id`, `stop_sequence`.
- `apns_sandbox` follows the §2.7 allow-list. The stakes are highest here: a
  misrouted push bounces `BadDeviceToken`, which the feedback webhook treats as
  terminal and deletes the subscription.
- POST is throttled at 30/minute per TCP peer (a sidecar-specific addition —
  every distinct stop costs one upstream call per minute for eight hours);
  DELETE is not.
- Subscriptions end at 8 hours, after 3 consecutive empty/error cycles, on
  client DELETE, or on terminal APNs feedback. The first two send a best-effort
  `end` push (dismissal 15 minutes out); the last two do not.
- Without `SIDECAR_GORUSH_URL` the updater runs in store-only mode: rows expire
  and reap but nothing is pushed.

Deviations from OBACloud, all deliberate: route/headsign matching resolves
names through the response `references` like the app does; the push-token
length cap and the POST throttle are new.
```

- [ ] **Step 3: CLAUDE.md**

Add `liveactivities` to the domain package list on line 41 and "Live Activity updater" to the background-loop list in the "Wiring lives only in `cmd/sidecar/main.go`" paragraph.

- [ ] **Step 4: Full check**

```bash
make web    # once, for the adminui embed test
make check  # fmt-check vet lint test test-tz test-race web-check
```

Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add cmd/sidecar/main.go README.md CLAUDE.md
git commit -m "feat: wire Live Activity updater and document the endpoints"
```
