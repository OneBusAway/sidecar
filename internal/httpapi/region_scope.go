package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// routeScope is which region middleware an admin route carries.
type routeScope int

const (
	// scopeNone is a route with no {regionId} segment.
	scopeNone routeScope = iota
	// scopeRegion is the ordinary tenancy fence: the principal must be able
	// to access the path's region.
	scopeRegion
	// scopeKeyAdmin is the key-management family only. It grants operators
	// and service principals access without consulting canAccessRegion, and
	// is a separate scope so a service principal's reach is visible in one
	// place and assertable by the route-table test.
	scopeKeyAdmin
)

// regionNotFoundBody is the ONE body every region-scoping failure returns.
// Malformed, unknown, and "not yours" must be indistinguishable, or the
// status code becomes an oracle a region key can use to enumerate regions
// (design spec section 2.5).
const regionNotFoundBody = "region not found"

// adminRegionSegment is the exact grammar of {regionId}: no leading zeros,
// no sign, no whitespace. It deliberately differs from the rider feed's
// lenient ParseRegionSegment, which the admin API does not reuse -- a feed
// that shrugs at "01" costs a rider nothing, while an admin API that does
// hands a caller two spellings of the same tenant.
var adminRegionSegment = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// requireRegion is the tenancy fence for every region-scoped route except
// the key-management family. It parses {regionId}, checks the principal,
// loads the region, and stores it in the context. Handlers read it from
// there and never fetch a region themselves -- which is what makes tenancy
// one check rather than one per handler.
func (h *authMiddleware) requireRegion(next http.Handler) http.Handler {
	return h.scopedRegion(next, false)
}

// requireKeyAdminRegion is requireRegion for the .../api_keys family. It
// parses and loads the region the same way but grants access to operators
// and service principals without consulting canAccessRegion. It is a
// separate function so the service principal's reach is confined to one
// visible place; the route-table test asserts it is applied to exactly the
// patterns ending in /api_keys or /api_keys/{keyId}.
func (h *authMiddleware) requireKeyAdminRegion(next http.Handler) http.Handler {
	return h.scopedRegion(next, true)
}

// scopedRegion is the shared body of the two scopes. keyAdmin swaps the
// access rule and nothing else, so the two can never drift on how a region
// id is parsed, refused, or reported.
func (h *authMiddleware) scopedRegion(next http.Handler, keyAdmin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok {
			// Unreachable through the router: requirePrincipal always runs
			// first. Reaching it means a route lost its middleware, and
			// serving the request would be worse than failing loudly.
			serverErrorJSON(w, h.deps.Logger, "region scope reached without a principal",
				errors.New("no principal on request context"))
			return
		}

		raw := r.PathValue("regionId")
		if !adminRegionSegment.MatchString(raw) {
			h.regionNotFound(w, r, "malformed region segment")
			return
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// Only reachable for a digit string too long for int64.
			h.regionNotFound(w, r, "region segment out of range")
			return
		}

		allowed := p.canAccessRegion(id)
		if keyAdmin {
			allowed = p.kind == principalOperator || p.kind == principalService
		}
		if !allowed {
			h.regionNotFound(w, r, "principal may not access this region")
			return
		}

		region, err := h.deps.Regions.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, regions.ErrNotFound) {
				h.regionNotFound(w, r, "no such region")
				return
			}
			serverErrorJSON(w, h.deps.Logger, "get region", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), regionContextKey, region)))
	})
}

// regionNotFound writes the single 404 and logs why. The client never sees
// the reason; that asymmetry is the point.
func (h *authMiddleware) regionNotFound(w http.ResponseWriter, r *http.Request, reason string) {
	h.deps.Logger.Debug("httpapi: region scope refused",
		"reason", reason, "segment", r.PathValue("regionId"), "path", r.URL.Path)
	writeJSONError(w, h.deps.Logger, http.StatusNotFound, regionNotFoundBody)
}

// regionFrom returns the region requireRegion loaded for this request. The
// boolean is false for any context that did not pass through a region scope,
// so a handler can never mistake a zero value for region 0 -- which is a real
// region (Tampa Bay).
func regionFrom(ctx context.Context) (regions.Region, bool) {
	region, ok := ctx.Value(regionContextKey).(regions.Region)
	return region, ok
}

// mustRegion is regionFrom for handlers: a missing context region means a
// route lost its scope middleware, which is a 500 rather than a silent fall
// back to region 0.
func mustRegion(w http.ResponseWriter, r *http.Request, deps Deps) (regions.Region, bool) {
	region, ok := regionFrom(r.Context())
	if !ok {
		serverErrorJSON(w, deps.Logger, "handler reached without a scoped region",
			errors.New("no region on request context"))
		return regions.Region{}, false
	}
	return region, true
}

// pathInt64 parses an integer path wildcard, returning a caller-safe message
// the HTTP layer maps to 400. Resource ids keep that 400: only {regionId}
// trades it for a 404, and only because its status code would otherwise
// enumerate regions.
func pathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be an integer", name, raw)
	}
	return v, nil
}

// loadAlert resolves {id} within the request's region. An alert in another
// region is the same 404 as one that does not exist: alert ids are globally
// unique, so this loader is what makes the {regionId} segment a fence rather
// than decoration.
func loadAlert(w http.ResponseWriter, r *http.Request, deps Deps) (alerts.Alert, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return alerts.Alert{}, false
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeJSONError(w, deps.Logger, http.StatusBadRequest, err.Error())
		return alerts.Alert{}, false
	}
	a, err := deps.Alerts.Get(r.Context(), id)
	if err != nil {
		writeStoreError(w, deps.Logger, "get alert", err)
		return alerts.Alert{}, false
	}
	if a.RegionID != region.ID {
		writeJSONError(w, deps.Logger, http.StatusNotFound, "alert not found")
		return alerts.Alert{}, false
	}
	return a, true
}

// loadStudy resolves {id} within the request's region. A study id that
// exists but belongs to another region gets exactly the same "study not
// found" body as one that does not exist at all, matching loadAlert's
// reasoning: the {regionId} segment is a fence, not a filter, and the two
// cases must be indistinguishable to a caller.
func loadStudy(w http.ResponseWriter, r *http.Request, deps Deps) (surveys.Study, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return surveys.Study{}, false
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeJSONError(w, deps.Logger, http.StatusBadRequest, err.Error())
		return surveys.Study{}, false
	}
	st, err := deps.Surveys.GetStudy(r.Context(), id)
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, deps.Logger, http.StatusNotFound, "study not found")
			return surveys.Study{}, false
		}
		serverErrorJSON(w, deps.Logger, "get study", err)
		return surveys.Study{}, false
	}
	if st.RegionID != region.ID {
		writeJSONError(w, deps.Logger, http.StatusNotFound, "study not found")
		return surveys.Study{}, false
	}
	return st, true
}

// loadSurvey resolves {id} within the request's region THROUGH ITS STUDY:
// surveys carry no region of their own, so the study is the only place the
// tenancy answer lives. GetSurvey populates Study on every read, so this
// needs no second query.
func loadSurvey(w http.ResponseWriter, r *http.Request, deps Deps) (surveys.Survey, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return surveys.Survey{}, false
	}
	id, err := pathInt64(r, "id")
	if err != nil {
		writeJSONError(w, deps.Logger, http.StatusBadRequest, err.Error())
		return surveys.Survey{}, false
	}
	s, err := deps.Surveys.GetSurvey(r.Context(), id)
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, deps.Logger, http.StatusNotFound, "survey not found")
			return surveys.Survey{}, false
		}
		serverErrorJSON(w, deps.Logger, "get survey", err)
		return surveys.Survey{}, false
	}
	if s.Study.RegionID != region.ID {
		writeJSONError(w, deps.Logger, http.StatusNotFound, "survey not found")
		return surveys.Survey{}, false
	}
	return s, true
}

// loadResponse resolves {publicId} within the request's region THROUGH ITS
// SURVEY'S STUDY, in the single query GetResponseInRegion joins
// (survey_responses -> surveys -> studies). Unlike loadAlert, loadStudy and
// loadSurvey, this is not a load-then-compare: the region condition lives in
// the SQL itself, so there is no Go-level "does this row belong to me" check
// a later refactor could accidentally drop.
func loadResponse(w http.ResponseWriter, r *http.Request, deps Deps) (surveys.Response, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return surveys.Response{}, false
	}
	publicID := r.PathValue("publicId")
	resp, err := deps.Surveys.GetResponseInRegion(r.Context(), region.ID, publicID)
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, deps.Logger, http.StatusNotFound, "response not found")
			return surveys.Response{}, false
		}
		serverErrorJSON(w, deps.Logger, "get survey response", err)
		return surveys.Response{}, false
	}
	return resp, true
}

// loadReport resolves {publicId} within the request's region in the single
// query GetByPublicID's own region_id condition performs. Like loadResponse
// (and unlike loadAlert, loadStudy and loadSurvey), this is not a
// load-then-compare: the region fence lives in the repository's SQL, so
// there is no Go-level "does this row belong to me" check a later refactor
// could accidentally drop.
func loadReport(w http.ResponseWriter, r *http.Request, deps Deps) (ghostbus.Report, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return ghostbus.Report{}, false
	}
	publicID := r.PathValue("publicId")
	rep, err := deps.GhostBus.GetByPublicID(r.Context(), region.ID, publicID)
	if err != nil {
		if errors.Is(err, ghostbus.ErrNotFound) {
			writeJSONError(w, deps.Logger, http.StatusNotFound, "ghost bus report not found")
			return ghostbus.Report{}, false
		}
		serverErrorJSON(w, deps.Logger, "get ghost bus report", err)
		return ghostbus.Report{}, false
	}
	return rep, true
}
