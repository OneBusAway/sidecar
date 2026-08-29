package apikey

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var (
	// ErrNotFound is returned when no row matches an id or a key hash.
	ErrNotFound = errors.New("api key not found")
	// ErrRevoked is returned by the by-hash lookups when the hash matches a
	// revoked row. It is deliberately distinct from ErrNotFound: a revoked
	// key being replayed is the clearest signal that a credential leaked,
	// and the middleware logs it as reason=revoked (spec section 4.2).
	ErrRevoked = errors.New("api key revoked")
)

// Actor names who minted or revoked a key. It is not a foreign key: a
// deleted operator or a revoked principal must not orphan the audit trail
// (spec section 3).
type Actor struct {
	// Kind is ActorOperator, ActorPrincipal, or ActorCLI.
	Kind string
	// ID is the users.id or service_principals.id, and 0 for the CLI.
	ID int64
}

// The three actor kinds. They are the same strings the CHECK constraints on
// region_api_keys enforce.
const (
	ActorOperator  = "operator"
	ActorPrincipal = "principal"
	ActorCLI       = "cli"
)

// RegionKey is one bearer credential scoped to exactly one region.
type RegionKey struct {
	ID       int64
	RegionID int64
	Name     string
	KeyHash  string
	// Scopes widens what the key may do beyond the region-scoped authoring
	// surface; see Scope. Empty for every key minted before scopes existed.
	Scopes     Scopes
	CreatedBy  Actor
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	RevokedBy  *Actor
}

// LogValue implements slog.LogValuer, omitting KeyHash for the same reason
// regions.Region.LogValue omits its OBA key: the omission makes the leak
// unrepresentable rather than merely unwritten.
func (k RegionKey) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Int64("id", k.ID),
		slog.Int64("region_id", k.RegionID),
		slog.String("name", k.Name),
		slog.Any("scopes", k.Scopes.Strings()),
		slog.String("created_by_kind", k.CreatedBy.Kind),
		slog.Int64("created_by_id", k.CreatedBy.ID),
		slog.Bool("revoked", k.RevokedAt != nil),
	}
	if k.RevokedBy != nil {
		attrs = append(attrs,
			slog.String("revoked_by_kind", k.RevokedBy.Kind),
			slog.Int64("revoked_by_id", k.RevokedBy.ID))
	}
	return slog.GroupValue(attrs...)
}

// ServicePrincipal is the deployment-wide credential that may mint, list,
// and revoke region keys, and nothing else.
type ServicePrincipal struct {
	ID         int64
	Name       string
	KeyHash    string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// LogValue implements slog.LogValuer, omitting KeyHash. See RegionKey.LogValue.
func (p ServicePrincipal) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", p.ID),
		slog.String("name", p.Name),
		slog.Bool("revoked", p.RevokedAt != nil),
	)
}

// Repository stores region keys and service principals. Implementations must
// be safe for concurrent use. Revoked rows are kept rather than deleted, so
// `key list` shows history and a revoked hash can never be re-minted by
// accident.
type Repository interface {
	// CreateRegionKey mints a key. scopes is stored as given (already
	// normalized by ParseScopes); nil is an empty set.
	CreateRegionKey(ctx context.Context, regionID int64, name, keyHash string, scopes Scopes, by Actor, now time.Time) (RegionKey, error)
	// GetRegionKeyByHash returns ErrNotFound for unknown hashes and
	// ErrRevoked for a hash that matches a revoked row, so the caller can
	// log a replay distinctly.
	GetRegionKeyByHash(ctx context.Context, keyHash string) (RegionKey, error)
	// ListRegionKeys returns live and revoked keys, newest first.
	ListRegionKeys(ctx context.Context, regionID int64) ([]RegionKey, error)
	// ListRegionKeysByCreator returns every key an actor minted, across
	// regions, newest first. Actor{Kind: ActorCLI} matches created_by_id IS
	// NULL; an implementation must never spell that as a bare "= ?", which
	// silently matches nothing.
	ListRegionKeysByCreator(ctx context.Context, by Actor) ([]RegionKey, error)
	// RevokeRegionKey is region-scoped: a key in another region is
	// ErrNotFound. An already-revoked key is a no-op success.
	RevokeRegionKey(ctx context.Context, regionID, id int64, by Actor, now time.Time) error
	// RevokeRegionKeysByCreator revokes every live key the actor minted, in
	// one transaction, and returns their ids ascending.
	RevokeRegionKeysByCreator(ctx context.Context, minted, by Actor, now time.Time) ([]int64, error)
	// TouchRegionKey records use. Callers touch at most hourly (spec
	// section 4.2), so last_used_at may be up to an hour stale.
	TouchRegionKey(ctx context.Context, id int64, now time.Time) error

	CreatePrincipal(ctx context.Context, name, keyHash string, now time.Time) (ServicePrincipal, error)
	// GetPrincipalByHash returns ErrNotFound or ErrRevoked, as
	// GetRegionKeyByHash does.
	GetPrincipalByHash(ctx context.Context, keyHash string) (ServicePrincipal, error)
	ListPrincipals(ctx context.Context) ([]ServicePrincipal, error)
	// RevokePrincipal is a no-op success for an already-revoked principal
	// and ErrNotFound for an unknown id.
	RevokePrincipal(ctx context.Context, id int64, now time.Time) error
	TouchPrincipal(ctx context.Context, id int64, now time.Time) error
}
