package httpapi

import (
	"context"
	"log/slog"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
)

// principalKind is which credential authenticated a request. The zero value
// is deliberately not a kind, so a principal that never went through
// requirePrincipal cannot pass an allow-list check by accident.
type principalKind int

const (
	principalOperator  principalKind = iota + 1 // session cookie
	principalRegionKey                          // obask_
	principalService                            // obasp_
	// principalPushKey is never what a request authenticates AS -- a
	// push-scoped key authenticates as principalRegionKey. It is the
	// allow-list marker for "a region key carrying apikey.ScopePush", so the
	// route table can say operatorOrPushKey and a test can ask has(). See
	// principalSet.admits.
	principalPushKey
)

// String is for log lines and test failure messages only; it is never a wire
// value.
func (k principalKind) String() string {
	switch k {
	case principalOperator:
		return "operator"
	case principalRegionKey:
		return "region_key"
	case principalService:
		return "service_principal"
	case principalPushKey:
		return "push_scoped_region_key"
	default:
		return "unknown"
	}
}

// principalSet is a route's allow-list. A nil set means "no principal
// required", which is true of exactly two routes (login and logout).
type principalSet []principalKind

// has reports whether a principal of kind k may use a route guarded by s.
func (s principalSet) has(k principalKind) bool {
	for _, want := range s {
		if want == k {
			return true
		}
	}
	return false
}

// admits reports whether p may use a route guarded by s. It is has() on
// p's kind, plus the one derived admission: a region key carrying the push
// scope satisfies principalPushKey. Callers deciding access use this, not
// has(), so the scope check has exactly one home.
func (s principalSet) admits(p principal) bool {
	if s.has(p.kind) {
		return true
	}
	return p.kind == principalRegionKey && p.scopes.Has(apikey.ScopePush) && s.has(principalPushKey)
}

// The four allow-lists every admin route draws from (design spec section
// 4.5). They are named values rather than inline literals so the route table
// reads as policy and a fifth combination has to be introduced deliberately.
var (
	// operatorOnly is for routes no key of any kind may reach: the
	// cross-region region list, and the session's whoami.
	operatorOnly = principalSet{principalOperator}
	// operatorOrKey is the ordinary region-scoped authoring surface.
	operatorOrKey = principalSet{principalOperator, principalRegionKey}
	// operatorOrService is the key-management family, and the only place
	// principalService appears at all.
	operatorOrService = principalSet{principalOperator, principalService}
	// operatorOrPushKey is the two push writes (send, cancel): an operator,
	// or a region key that was minted with the push scope. An unscoped
	// region key is refused, so a leaked ordinary key still cannot page
	// every device in its region (keys design spec section 2.1, amended by
	// migration design spec section 0.2).
	operatorOrPushKey = principalSet{principalOperator, principalPushKey}
)

// principal is who is making an admin request.
type principal struct {
	kind principalKind
	// user is populated for operators only; whoami needs the username.
	user auth.User
	// regionID is populated for region keys only.
	regionID int64
	// keyID is populated for region keys and service principals.
	keyID int64
	// scopes is populated for region keys only; see apikey.Scopes.
	scopes apikey.Scopes
}

// canAccessRegion is the tenancy fence for every region-scoped route EXCEPT
// the key-management family. A service principal is never granted region
// access here: its reach comes only from the key-management routes, which
// check it separately precisely so that reach is visible in one place.
func (p principal) canAccessRegion(id int64) bool {
	return p.kind == principalOperator || (p.kind == principalRegionKey && p.regionID == id)
}

// actor renders the principal for a key's created_by / revoked_by columns.
func (p principal) actor() apikey.Actor {
	switch p.kind {
	case principalOperator:
		return apikey.Actor{Kind: apikey.ActorOperator, ID: p.user.ID}
	case principalService:
		return apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.keyID}
	default:
		// A region key can never mint or revoke a key (design spec section
		// 2.2), so this is unreachable through the router. Returning the CLI
		// actor rather than a zero Actor keeps the CHECK constraints
		// satisfiable if it ever is reached, instead of failing the write
		// with a constraint error nobody can read.
		return apikey.Actor{Kind: apikey.ActorCLI}
	}
}

// LogValue implements slog.LogValuer. principal carries an auth.User, and
// that carries the argon2 password hash: without this method a future
// slog.Any("principal", p) would print it. Emitting only kind, username,
// region id and key id makes that leak unrepresentable rather than merely
// unwritten -- the same argument as regions.Region.LogValue.
func (p principal) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", p.kind.String()),
		slog.String("username", p.user.Username),
		slog.Int64("region_id", p.regionID),
		slog.Int64("key_id", p.keyID),
		slog.Any("scopes", p.scopes.Strings()),
	)
}

// principalFrom returns the principal requirePrincipal authenticated for this
// request. The boolean is false for any context that did not pass through
// requirePrincipal, so a handler can never mistake a zero value for an
// authenticated caller.
func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalContextKey).(principal)
	return p, ok
}
