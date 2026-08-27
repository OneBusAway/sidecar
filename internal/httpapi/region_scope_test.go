package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// requireRegion
// ---------------------------------------------------------------------------

// TestRequireRegion_MalformedSegmentIs404. Deliberately NOT pathInt64's 400:
// an unparseable region is "no such region", and the response must not differ
// between malformed, not-yours, and does-not-exist -- otherwise the status
// code alone tells a region key which region ids exist.
func TestRequireRegion_MalformedSegmentIs404(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	// %20 rather than a literal space: a space in the request line is not a
	// URL this package could ever receive, and httptest cannot synthesize one.
	for _, segment := range []string{"abc", "01", "-1", "+1", "1.0", "1%20", "%201", "999999999999999999999"} {
		rec := f.do(http.MethodGet, "/api/admin/v1/regions/"+segment+"/alerts", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("region segment %q: status = %d, want 404; body = %s", segment, rec.Code, rec.Body.String())
			continue
		}
		if got, want := bodyText(rec), `{"error":"region not found"}`; got != want {
			t.Errorf("region segment %q: body = %q, want %q", segment, got, want)
		}
	}
}

// TestRequireRegion_UnknownAndForeignAreIndistinguishable is the probing
// defence: a region key must not be able to learn which region ids exist by
// comparing status codes or bodies.
//
// The operator's unknown-region request is walked too, because it is the only
// way to reach the Regions.Get / ErrNotFound branch: a region key is refused
// by canAccessRegion before the store is ever consulted, so a key-only test
// would compare one branch against itself.
func TestRequireRegion_UnknownAndForeignAreIndistinguishable(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	foreign := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer "+raw)
	unknown := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/9999/alerts", "", "Bearer "+raw)
	malformed := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/abc/alerts", "", "Bearer "+raw)
	// The operator reaches the store lookup, which is the branch a region key
	// never gets to.
	operatorUnknown := f.do(http.MethodGet, "/api/admin/v1/regions/9999/alerts", "")

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"foreign": foreign, "unknown": unknown, "malformed": malformed,
		"operator unknown": operatorUnknown,
	} {
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404; body = %s", name, rec.Code, rec.Body.String())
		}
		if got, want := bodyText(rec), `{"error":"region not found"}`; got != want {
			t.Errorf("%s: body = %q, want %q", name, got, want)
		}
	}
}

// TestRequireRegion_OperatorReachesEveryRegion. The cookie is not
// region-scoped; only keys are.
func TestRequireRegion_OperatorReachesEveryRegion(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, id := range []int{regionTampa, regionPuget, regionBare} {
		rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts", id), "")
		if rec.Code != http.StatusOK {
			t.Errorf("region %d: status = %d, want 200; body = %s", id, rec.Code, rec.Body.String())
		}
	}
}

// TestRequireRegion_InactiveRegionStaysAuthorable. regions.Region.Active is
// the directory's flag and is deliberately not consulted for admin access;
// regionBare is seeded inactive.
func TestRequireRegion_InactiveRegionStaysAuthorable(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionBare)
	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/2/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestRequireKeyAdminRegion_GrantsOperatorsAndServicePrincipals pins the
// second scope's one difference from requireRegion: it does not consult
// canAccessRegion, so a service principal -- which can access no region at all
// -- reaches every region, and a region key reaches none.
//
// It is driven through the middleware directly because no route carries
// scopeKeyAdmin until the key-management family lands; the route table test
// there pins that the scope is applied to exactly that family.
func TestRequireKeyAdminRegion_GrantsOperatorsAndServicePrincipals(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mw := &authMiddleware{deps: f.deps}

	tests := []struct {
		name       string
		p          principal
		wantStatus int
	}{
		{"operator", principal{kind: principalOperator}, http.StatusOK},
		{"service principal", principal{kind: principalService, keyID: 3}, http.StatusOK},
		{"region key, its own region", principal{kind: principalRegionKey, regionID: regionPuget}, http.StatusNotFound},
	}
	for _, tt := range tests {
		var reached bool
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			region, ok := regionFrom(r.Context())
			if !ok {
				t.Errorf("%s: the handler saw no region on the context", tt.name)
			}
			if region.ID != regionPuget {
				t.Errorf("%s: context region = %d, want %d", tt.name, region.ID, regionPuget)
			}
			w.WriteHeader(http.StatusOK)
		})

		req := httptest.NewRequestWithContext(
			context.WithValue(context.Background(), principalContextKey, tt.p),
			http.MethodGet, "/api/admin/v1/regions/1/api_keys", nil)
		req.SetPathValue("regionId", strconv.Itoa(regionPuget))
		rec := httptest.NewRecorder()
		mw.requireKeyAdminRegion(next).ServeHTTP(rec, req)

		if rec.Code != tt.wantStatus {
			t.Errorf("%s: status = %d, want %d; body = %s", tt.name, rec.Code, tt.wantStatus, rec.Body.String())
		}
		if reached != (tt.wantStatus == http.StatusOK) {
			t.Errorf("%s: next handler ran = %v, want %v", tt.name, reached, tt.wantStatus == http.StatusOK)
		}
	}
}

// TestRequireRegion_WithoutAPrincipalIs500 pins the unreachable branch: a
// scoped route that lost requirePrincipal must fail loudly rather than serve
// the request with no tenancy decision behind it.
func TestRequireRegion_WithoutAPrincipalIs500(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mw := &authMiddleware{deps: f.deps}

	var reached bool
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/1/alerts", nil)
	req.SetPathValue("regionId", "1")
	rec := httptest.NewRecorder()
	mw.requireRegion(next).ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if reached {
		t.Error("the handler ran without a principal")
	}
}

// TestMustRegion_WithoutAScopedRegionIs500 is the handler-side half of the
// same argument: falling back to the zero region would silently mean region 0,
// which is Tampa Bay.
func TestMustRegion_WithoutAScopedRegionIs500(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	h := &adminAlertsHandler{deps: f.deps}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/0/alerts", nil)
	rec := httptest.NewRecorder()
	h.list(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// loadAlert
// ---------------------------------------------------------------------------

// TestLoadAlert_ForeignAlertIs404 is what stops /regions/A/alerts/{id-of-B}.
// The id is globally unique, so without the loader the {regionId} segment
// would be decoration.
func TestLoadAlert_ForeignAlertIs404(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertIn(t, regionPuget, minimalAlertBody("puget alert"))

	rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionTampa, id), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"alert not found"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	// And it must not have been mutated either.
	rec = f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionTampa, id), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-region DELETE: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionPuget, id), ""); rec.Code != http.StatusOK {
		t.Errorf("the alert was deleted through the wrong region: status = %d", rec.Code)
	}
}

// TestLoadAlert_MalformedIDIs400 keeps the moved routes' existing code: a
// non-integer {id} is a malformed request, not a missing alert. Only the
// region segment trades 400 for 404, and only because it is an enumeration
// oracle.
func TestLoadAlert_MalformedIDIs400(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/alerts/abc", "")
	assertContains(t, "error", errorText(t, rec, http.StatusBadRequest), "invalid id", "abc")
}

// ---------------------------------------------------------------------------
// the moved alert routes
// ---------------------------------------------------------------------------

// TestAdminAlerts_ListIsRegionScoped: the ?region= query filter is gone; the
// path segment is the only region source, and it is not a filter.
func TestAdminAlerts_ListIsRegionScoped(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.createAlertIn(t, regionPuget, minimalAlertBody("puget"))
	f.createAlertIn(t, regionTampa, minimalAlertBody("tampa"))

	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/alerts?region=0", "")
	list := array(t, rec, http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d alerts, want 1 (the query parameter must be ignored)", len(list))
	}
	if got := list[0]["header"]; got != "puget" {
		t.Errorf("header = %v, want puget", got)
	}
}

// TestAdminAlerts_CreateRejectsRegionID. A stale client that still sends
// region_id must not believe it targeted a region; the field is rejected
// rather than ignored (design spec section 5.1).
func TestAdminAlerts_CreateRejectsRegionID(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	before := f.countAlerts(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", `{
		"region_id": 0, "header": "x", "start_time": "2026-08-15T14:00:00-07:00"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(bodyText(rec), "region_id") {
		t.Errorf("body = %q, want it to name region_id", bodyText(rec))
	}
	if after := f.countAlerts(t); after != before {
		t.Errorf("the rejected create wrote %d alert(s)", after-before)
	}
}

// TestAdminAlerts_CreateLocationCarriesTheRegion pins the Location header's
// new shape, which the SPA follows.
func TestAdminAlerts_CreateLocationCarriesTheRegion(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", minimalAlertBody("x"))
	got := object(t, rec, http.StatusCreated)
	want := fmt.Sprintf("/api/admin/v1/regions/1/alerts/%d", jsonID(t, got))
	if rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
	if got["region_id"] != float64(regionPuget) {
		t.Errorf("region_id = %v, want %d (the path's region)", got["region_id"], regionPuget)
	}
}

// ---------------------------------------------------------------------------
// GET /regions/{regionId}
// ---------------------------------------------------------------------------

// TestGetRegion_ReportsFeatures lets a consumer tell "family not enabled
// here" from a 404 (design spec section 5.1, 5.7).
func TestGetRegion_ReportsFeatures(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t) // alerts, pushes, api_keys, push_registrations wired
	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1", "")
	got := object(t, rec, http.StatusOK)
	if got["name"] != "Puget Sound" {
		t.Errorf("name = %v, want Puget Sound", got["name"])
	}
	// The key is never echoed: oba_api_key is a status word, as on the list.
	if got["oba_api_key"] == nil {
		t.Error("oba_api_key status word missing")
	}
	assertKeys(t, "region detail", got, append(append([]string(nil), regionJSONFields...), "features"))
	features := stringSet(t, got["features"])
	for _, want := range []string{"alerts", "pushes", "push_registrations", "api_keys"} {
		if !features[want] {
			t.Errorf("features %v missing %q", got["features"], want)
		}
	}
	for _, absent := range []string{"surveys", "ghost_bus_reports", "alarms"} {
		if features[absent] {
			t.Errorf("features %v contains %q, which is not wired in this fixture", got["features"], absent)
		}
	}
}

// TestGetRegion_FeaturesFollowTheWiring is the other half: a deployment with
// no push transport must not advertise "pushes", or the SPA hides a card the
// server would have served (and vice versa).
func TestGetRegion_FeaturesFollowTheWiring(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.AlertPushWaker = nil
		d.APIKeys = nil
		d.PushRegs = nil
	})
	got := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1", ""), http.StatusOK)
	features := stringSet(t, got["features"])
	if !features["alerts"] {
		t.Errorf("features %v missing alerts, which is always registered", got["features"])
	}
	for _, absent := range []string{"pushes", "api_keys", "push_registrations"} {
		if features[absent] {
			t.Errorf("features %v advertises %q with no dependency wired", got["features"], absent)
		}
	}
}

// ---------------------------------------------------------------------------
// the route table's scope column
// ---------------------------------------------------------------------------

// TestRouteTable_ScopeAgreesWithPattern is the invariant that keeps handlers
// from parsing a region by hand: a pattern with {regionId} must be scoped,
// and a scoped route must have the segment.
func TestRouteTable_ScopeAgreesWithPattern(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, rt := range adminRoutes(f.deps) {
		hasSegment := strings.Contains(rt.pattern, "{regionId}")
		if hasSegment != (rt.scope != scopeNone) {
			t.Errorf("route %q: has {regionId} = %v but scope = %v", rt.pattern, hasSegment, rt.scope)
		}
	}
}

// ---------------------------------------------------------------------------
// the tenancy walk
// ---------------------------------------------------------------------------

// tenancySpec says how the tenancy walk should call one route.
//
// The default -- both booleans false -- is a route that addresses a resource
// by a globally unique id: the walk puts region A (the caller's own) in the
// path and region B's id in the resource wildcard, so requireRegion passes and
// the route's loader is the only thing left that can refuse.
type tenancySpec struct {
	// wantEmptyList marks a collection route: it answers 200 with an empty
	// array rather than 404, because the region itself IS the caller's and
	// the proof is that region B's rows are not in it.
	wantEmptyList bool
	// ownRegion marks a route whose only resource is the region in the path.
	// There is nothing foreign to name in such a URL, so the walk puts region
	// B in the path instead and the fence under test is canAccessRegion.
	ownRegion bool
	// bodyFor builds the request body, given the region-B fixture ids. Nil
	// means no body.
	bodyFor func(fx tenancyFixtureIDs) string
}

func (s tenancySpec) body(fx tenancyFixtureIDs) string {
	if s.bodyFor == nil {
		return ""
	}
	return s.bodyFor(fx)
}

// tenancyFixtureIDs are the resources seeded in region B that the walk tries
// to reach through region A. Later route families add their own ids here.
type tenancyFixtureIDs struct {
	alertID int64
	pushID  int64
}

// tenancyFixtures is keyed by route pattern. Every scoped route needs an
// entry; TestRouteTable_TenancyWalk fails on a missing one.
var tenancyFixtures = map[string]tenancySpec{
	"GET /api/admin/v1/regions/{regionId}":   {ownRegion: true},
	"PATCH /api/admin/v1/regions/{regionId}": {ownRegion: true, bodyFor: func(tenancyFixtureIDs) string { return `{"default_agency_id":"x"}` }},

	"GET /api/admin/v1/regions/{regionId}/alerts":  {wantEmptyList: true},
	"POST /api/admin/v1/regions/{regionId}/alerts": {ownRegion: true, bodyFor: func(tenancyFixtureIDs) string { return minimalAlertBody("x") }},

	"GET /api/admin/v1/regions/{regionId}/alerts/{id}":    {},
	"PATCH /api/admin/v1/regions/{regionId}/alerts/{id}":  {bodyFor: func(tenancyFixtureIDs) string { return `{"header":"x"}` }},
	"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}": {},

	"POST /api/admin/v1/regions/{regionId}/alerts/{id}/publish":   {},
	"POST /api/admin/v1/regions/{regionId}/alerts/{id}/unpublish": {},

	"PUT /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang}":    {bodyFor: func(tenancyFixtureIDs) string { return `{"header":"x"}` }},
	"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang}": {},

	"POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes":            {bodyFor: func(tenancyFixtureIDs) string { return `{}` }},
	"GET /api/admin/v1/regions/{regionId}/alerts/{id}/pushes":             {},
	"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}": {},
	"GET /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience":      {},
}

// seedTenancyFixtures creates, in region B, one of every resource the walk
// then tries to reach through region A. It goes through the API as the
// operator, so the fixtures are exactly what the routes under test produce.
func (f *adminFixture) seedTenancyFixtures(t *testing.T) tenancyFixtureIDs {
	t.Helper()

	const regionB = regionTampa
	alertID := f.seedPublishedAlert(t, regionB, false)
	f.seedRegistration(t, regionB, "tenancy-token", false)

	// A translation the cross-region DELETE could actually remove if the
	// loader stopped fencing: without one, that route would answer 404 for
	// "no such translation" and the walk would pass for the wrong reason.
	object(t, f.do(http.MethodPut,
		fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d/translations/es", regionB, alertID),
		`{"header":"Encabezado"}`), http.StatusOK)

	created := object(t, f.do(http.MethodPost,
		fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d/pushes", regionB, alertID), `{}`),
		http.StatusAccepted)

	return tenancyFixtureIDs{alertID: alertID, pushID: jsonID(t, created)}
}

// tenancyTarget turns a scoped route pattern into the one request that must be
// refused: region A in {regionId} (my region) and region B's ids everywhere
// else (your resource) -- except for ownRegion routes, whose only resource is
// the region itself and which therefore carry region B in the path.
func (f *adminFixture) tenancyTarget(t *testing.T, pattern string, spec tenancySpec, fx tenancyFixtureIDs) (method, target string) {
	t.Helper()
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		t.Fatalf("route pattern %q has no method", pattern)
	}
	regionID := int64(regionPuget) // region A: the key's own
	if spec.ownRegion {
		regionID = regionTampa // region B: somebody else's
	}
	path = strings.ReplaceAll(path, "{regionId}", strconv.FormatInt(regionID, 10))
	path = strings.ReplaceAll(path, "{id}", strconv.FormatInt(fx.alertID, 10))
	path = strings.ReplaceAll(path, "{pushId}", strconv.FormatInt(fx.pushID, 10))
	path = strings.ReplaceAll(path, "{lang}", "es")
	if strings.ContainsAny(path, "{}") {
		t.Fatalf("route pattern %q has a wildcard the tenancy walk cannot fill", pattern)
	}
	return method, path
}

// TestRouteTable_TenancyWalk calls every scoped route against fixtures created
// in region B. Reads are 404 (or an empty list); writes are 404 and change
// nothing. A route added to adminRoutes without a fixture entry fails here,
// which is what keeps this walk complete as families are added.
//
// Routes a region key may not use at all are walked as the operator instead:
// the allow-list would refuse the key with a 403 before tenancy was ever
// consulted, and the operator reaches every region, so the loader is again the
// only fence left standing.
func TestRouteTable_TenancyWalk(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	keyForA := f.mintRegionKey(t, regionPuget) // region A = 1
	fx := f.seedTenancyFixtures(t)             // creates everything in region B = 0
	alertsBefore := f.countAlerts(t)

	walked := map[string]bool{}
	for _, rt := range adminRoutes(f.deps) {
		if rt.scope == scopeNone {
			continue
		}
		spec, ok := tenancyFixtures[rt.pattern]
		if !ok {
			t.Errorf("route %q has no tenancyFixtures entry; add one so the walk stays complete", rt.pattern)
			continue
		}
		walked[rt.pattern] = true
		method, target := f.tenancyTarget(t, rt.pattern, spec, fx)

		var rec *httptest.ResponseRecorder
		if rt.allowed.has(principalRegionKey) {
			rec = sendBearer(f.handler, method, target, spec.body(fx), "Bearer "+keyForA)
		} else {
			rec = f.do(method, target, spec.body(fx))
		}

		if spec.wantEmptyList {
			list := array(t, rec, http.StatusOK)
			if len(list) != 0 {
				t.Errorf("%s %s: returned %d region-B rows to a region-A caller", method, target, len(list))
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with a region-A caller against region-B data: status = %d, want 404; body = %s",
				method, target, rec.Code, rec.Body.String())
		}
	}

	// A fixture entry for a route that no longer exists is a rule nobody is
	// checking any more; it must be deleted with the route.
	for pattern := range tenancyFixtures {
		if !walked[pattern] {
			t.Errorf("tenancyFixtures has an entry for %q, which is not a scoped route", pattern)
		}
	}

	// Nothing the walk sent may have written a row either: the refused POST
	// must not have created an alert in anybody's region.
	if after := f.countAlerts(t); after != alertsBefore {
		t.Errorf("the walk created %d alert(s)", after-alertsBefore)
	}

	// Nothing the walk sent may have touched region B's alert.
	stored := f.storedAlert(t, fx.alertID)
	if stored.HeaderText != "Route 40 detour" {
		t.Errorf("region B's alert header = %q; a cross-region write went through", stored.HeaderText)
	}
	if !stored.Published {
		t.Error("region B's alert was unpublished through region A")
	}
	if len(stored.Translations) == 0 {
		t.Error("region B's translation was deleted through region A")
	}
	if _, err := f.store.AlertPushes().Get(context.Background(), fx.pushID); err != nil {
		t.Errorf("region B's push is gone after the walk: %v", err)
	}
}
