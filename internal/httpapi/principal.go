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

// The three allow-lists every admin route draws from (design spec section
// 4.5). They are named values rather than inline literals so the route table
// reads as policy and a fourth combination has to be introduced deliberately.
var (
	// operatorOnly is for routes a leaked region key must not reach:
	// sending or cancelling a push, and the cross-region region list.
	operatorOnly = principalSet{principalOperator}
	// operatorOrKey is the ordinary region-scoped authoring surface.
	operatorOrKey = principalSet{principalOperator, principalRegionKey}
	// operatorOrService is the key-management family, and the only place
	// principalService appears at all.
	operatorOrService = principalSet{principalOperator, principalService}
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
