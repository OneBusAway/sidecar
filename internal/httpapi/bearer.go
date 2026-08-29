package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

const (
	// invalidAPIKeyBody is the single message every bearer failure returns.
	// Unknown, revoked, malformed and mismatched all look identical on the
	// wire: telling them apart is a probing oracle, and the operator's log
	// carries the distinction instead.
	invalidAPIKeyBody = "invalid api key"
	// forbiddenBody is a principal kind a route does not allow. It is
	// deliberately distinct from crossSiteGuard's message so tests and Rails
	// can tell a policy refusal from a browser-origin refusal.
	forbiddenBody = "forbidden"
	// bearerFailuresPerMinute bounds failed bearer attempts per peer. Only
	// failures are charged, so a busy consumer's steady traffic is unmetered;
	// this is the repo's one unauthenticated code path that reaches the key
	// tables and it keeps the throttle-everything-unauthenticated posture.
	bearerFailuresPerMinute = 60
	// touchInterval is how stale last_used_at is allowed to get. A write on
	// every request would put an UPDATE in front of every read.
	touchInterval = time.Hour
	// bearerScheme is matched case-insensitively, followed by exactly one
	// space.
	bearerScheme = "Bearer"
)

// authenticateBearer resolves an Authorization header into a principal. It
// writes the response itself on every failure and reports ok=false.
//
// Cookies are not consulted: if the header is present the request either
// authenticates by bearer or fails. Falling back would let a browser session
// silently rescue a call that was meant to be a bearer call, which is exactly
// how a revoked key stays invisible to whoever revoked it.
func (h *authMiddleware) authenticateBearer(w http.ResponseWriter, r *http.Request, values []string) (principal, bool) {
	if len(values) != 1 {
		// Two Authorization headers is ambiguous. Picking one is how a
		// proxy-injected header quietly wins over the client's.
		return h.rejectBearer(w, r, "duplicate authorization header", "count", len(values))
	}
	if h.deps.APIKeys == nil {
		return h.rejectBearer(w, r, "bearer auth not configured")
	}
	raw := values[0]
	if len(raw) > len(bearerScheme)+1+apikey.MaxRawLen {
		// Checked BEFORE hashing: an unauthenticated caller must not be able
		// to make the server SHA-256 an arbitrary body.
		return h.rejectBearer(w, r, "authorization header too long", "length", len(raw))
	}
	scheme, credential, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) || strings.HasPrefix(credential, " ") {
		return h.rejectBearer(w, r, "not a bearer credential", "length", len(raw))
	}

	kind, prefixRegion, ok := apikey.ParsePrefix(credential)
	if !ok {
		return h.rejectBearer(w, r, "unparseable key prefix", "length", len(credential))
	}
	switch kind {
	case apikey.KindRegion:
		return h.authenticateRegionKey(w, r, credential, prefixRegion)
	case apikey.KindPrincipal:
		return h.authenticatePrincipalKey(w, r, credential)
	default:
		// Unreachable: ParsePrefix reports ok only for the two kinds above.
		// Kept so a third kind added there cannot silently authenticate.
		return h.rejectBearer(w, r, "unknown key kind", "kind", string(kind))
	}
}

// authenticateRegionKey resolves an obask_ credential against the stored
// hash. The prefix's region id never decides anything on its own; see the
// mismatch branch.
func (h *authMiddleware) authenticateRegionKey(w http.ResponseWriter, r *http.Request, credential string, prefixRegion int64) (principal, bool) {
	key, err := h.deps.APIKeys.GetRegionKeyByHash(r.Context(), apikey.Hash(credential))
	switch {
	case errors.Is(err, apikey.ErrRevoked):
		return h.rejectBearer(w, r, "revoked key replayed",
			"reason", "revoked", "kind", string(apikey.KindRegion),
			"prefix_region_id", prefixRegion, "key_id", key.ID)
	case errors.Is(err, apikey.ErrNotFound):
		return h.rejectBearer(w, r, "unknown key",
			"kind", string(apikey.KindRegion), "prefix_region_id", prefixRegion)
	case err != nil:
		serverErrorJSON(w, h.deps.Logger, "get region api key", err)
		return principal{}, false
	}
	if key.RegionID != prefixRegion {
		// The plaintext's region id is a debugging aid and the hash lookup
		// decides -- but if the two disagree the row is not trustworthy for
		// either region.
		return h.rejectBearer(w, r, "key prefix and row disagree on region",
			"prefix_region_id", prefixRegion, "row_region_id", key.RegionID, "key_id", key.ID)
	}

	h.touch(r, key.LastUsedAt, func(now time.Time) error {
		return h.deps.APIKeys.TouchRegionKey(r.Context(), key.ID, now)
	})
	return principal{kind: principalRegionKey, regionID: key.RegionID, keyID: key.ID, scopes: key.Scopes}, true
}

// authenticatePrincipalKey resolves an obasp_ credential. There is no region
// in the plaintext to cross-check: a service principal is deployment-wide.
func (h *authMiddleware) authenticatePrincipalKey(w http.ResponseWriter, r *http.Request, credential string) (principal, bool) {
	p, err := h.deps.APIKeys.GetPrincipalByHash(r.Context(), apikey.Hash(credential))
	switch {
	case errors.Is(err, apikey.ErrRevoked):
		return h.rejectBearer(w, r, "revoked principal replayed",
			"reason", "revoked", "kind", string(apikey.KindPrincipal), "key_id", p.ID)
	case errors.Is(err, apikey.ErrNotFound):
		return h.rejectBearer(w, r, "unknown principal", "kind", string(apikey.KindPrincipal))
	case err != nil:
		serverErrorJSON(w, h.deps.Logger, "get service principal", err)
		return principal{}, false
	}
	h.touch(r, p.LastUsedAt, func(now time.Time) error {
		return h.deps.APIKeys.TouchPrincipal(r.Context(), p.ID, now)
	})
	return principal{kind: principalService, keyID: p.ID}, true
}

// touch records use at most once per touchInterval. It is best effort: a
// failed write is logged, never surfaced, because last_used_at is an
// operator convenience and losing it must not cost a consumer a request.
func (h *authMiddleware) touch(r *http.Request, lastUsed *time.Time, write func(time.Time) error) {
	now := h.deps.Now()
	if lastUsed != nil && now.Sub(*lastUsed) < touchInterval {
		return
	}
	if err := write(now); err != nil {
		h.deps.Logger.Warn("httpapi: touch api key", "err", err, "path", r.URL.Path)
	}
}

// rejectBearer charges the failure bucket and writes the response.
//
// The bucket is charged HERE rather than by wrapping the route in
// throttleByIP, because throttleByIP charges every call: a wrapper would
// meter the successful traffic, which is the hot path this must not touch.
// There is deliberately no FailDelay -- a 256-bit random key is not
// guessable, so a delay would defend nothing while pinning a goroutine per
// garbage request (design spec section 4.2).
//
// Nothing logged here may contain any part of the credential's random
// segment; only its length, its parsed kind, and the prefix's region id.
//
// The bucket is consulted BEFORE the Warn line is written, so a flood of
// garbage headers cannot amplify itself into the log: this is the one
// unauthenticated entry point that reaches the key tables, and one Warn per
// request would let an attacker write to the operator's disk for free. The
// throttled path still says so, at Debug -- the same level every other
// "reason the client is not told" in this package uses, so the capped Warn
// stream stays the production signal while an operator debugging a 429 can
// see the brake engage.
//
// A nil limiter fails OPEN: the throttle is an abuse brake, not the
// authentication decision, and refusing to answer 401 because no bucket was
// wired would turn a missing dependency into an outage. NewRouter always
// supplies one; a hand-built authMiddleware (tests) need not.
func (h *authMiddleware) rejectBearer(w http.ResponseWriter, r *http.Request, reason string, fields ...any) (principal, bool) {
	if h.deps.BearerFailLimiter != nil && !h.deps.BearerFailLimiter.Allow(h.deps.clientIP(r), h.deps.Now()) {
		h.deps.Logger.Debug("httpapi: bearer failures throttled",
			"remote", h.deps.clientIP(r), "path", r.URL.Path)
		w.WriteHeader(http.StatusTooManyRequests)
		return principal{}, false
	}
	attrs := append([]any{"reason_text", reason, "remote", h.deps.clientIP(r), "path", r.URL.Path}, fields...)
	h.deps.Logger.Warn("httpapi: bearer authentication failed", attrs...)
	writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, invalidAPIKeyBody)
	return principal{}, false
}
