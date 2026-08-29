package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// apiKeyRepo implements apikey.Repository. Error strings from this repo
// never embed a key hash: RegionKey.LogValue and ServicePrincipal.LogValue
// already omit it, and an fmt.Errorf here must not undo that.
type apiKeyRepo struct {
	db     *sql.DB
	q      *gen.Queries
	logger *slog.Logger
}

// regionKeyFromRow maps a generated row onto the domain type. CreatedBy's ID
// is 0 exactly when CreatedByKind is "cli": the region_api_keys CHECK
// constraint ((created_by_kind = 'cli') = (created_by_id IS NULL))
// guarantees that, so no separate zero-value branch is needed here.
//
// It returns an error for a scopes cell that does not decode or names a
// scope this binary does not know: a key must never quietly read back with
// fewer scopes than it was minted with (migration design spec section 2.2).
func regionKeyFromRow(r gen.RegionApiKey) (apikey.RegionKey, error) {
	scopes, err := decodeScopes(r.Scopes)
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: region api key %d: scopes: %w", r.ID, err)
	}
	out := apikey.RegionKey{
		ID:         r.ID,
		RegionID:   r.RegionID,
		Name:       r.Name,
		KeyHash:    r.KeyHash,
		Scopes:     scopes,
		CreatedBy:  apikey.Actor{Kind: r.CreatedByKind, ID: r.CreatedByID.Int64},
		CreatedAt:  unixToTime(r.CreatedAt),
		LastUsedAt: nullUnixToTime(r.LastUsedAt),
		RevokedAt:  nullUnixToTime(r.RevokedAt),
	}
	if r.RevokedByKind.Valid {
		out.RevokedBy = &apikey.Actor{Kind: r.RevokedByKind.String, ID: r.RevokedByID.Int64}
	}
	return out, nil
}

// encodeScopes renders a scope set for the scopes column: a JSON array of
// names, [] for an empty or nil set.
func encodeScopes(s apikey.Scopes) (string, error) {
	b, err := json.Marshal(s.Strings())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeScopes is the inverse of encodeScopes, re-validated through
// ParseScopes so a hand-edited or downgraded row cannot carry a name the
// running binary does not enforce.
func decodeScopes(cell string) (apikey.Scopes, error) {
	var names []string
	if err := json.Unmarshal([]byte(cell), &names); err != nil {
		return nil, err
	}
	return apikey.ParseScopes(names)
}

// regionKeysFromRows maps a list, stopping at the first undecodable row.
func regionKeysFromRows(rows []gen.RegionApiKey) ([]apikey.RegionKey, error) {
	out := make([]apikey.RegionKey, len(rows))
	for i, row := range rows {
		k, err := regionKeyFromRow(row)
		if err != nil {
			return nil, err
		}
		out[i] = k
	}
	return out, nil
}

// principalFromRow maps a generated row onto the domain type.
func principalFromRow(r gen.ServicePrincipal) apikey.ServicePrincipal {
	return apikey.ServicePrincipal{
		ID:         r.ID,
		Name:       r.Name,
		KeyHash:    r.KeyHash,
		CreatedAt:  unixToTime(r.CreatedAt),
		LastUsedAt: nullUnixToTime(r.LastUsedAt),
		RevokedAt:  nullUnixToTime(r.RevokedAt),
	}
}

// actorToColumns splits an Actor into the two columns that store it. The
// CLI kind stores no id: created_by_id / revoked_by_id is NULL, matching the
// CHECK constraint's (kind = 'cli') = (id IS NULL) pairing.
func actorToColumns(a apikey.Actor) (kind string, id sql.NullInt64) {
	if a.Kind == apikey.ActorCLI {
		return a.Kind, sql.NullInt64{}
	}
	return a.Kind, sql.NullInt64{Int64: a.ID, Valid: true}
}

func (r *apiKeyRepo) CreateRegionKey(ctx context.Context, regionID int64, name, keyHash string, scopes apikey.Scopes, by apikey.Actor, now time.Time) (apikey.RegionKey, error) {
	kind, id := actorToColumns(by)
	encoded, err := encodeScopes(scopes)
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: create region api key for region %d: encode scopes: %w", regionID, err)
	}
	row, err := r.q.CreateRegionAPIKey(ctx, gen.CreateRegionAPIKeyParams{
		RegionID: regionID, Name: name, KeyHash: keyHash, Scopes: encoded,
		CreatedByKind: kind, CreatedByID: id, CreatedAt: now.Unix(),
	})
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: create region api key for region %d: %w", regionID, err)
	}
	return regionKeyFromRow(row)
}

func (r *apiKeyRepo) GetRegionKeyByHash(ctx context.Context, keyHash string) (apikey.RegionKey, error) {
	row, err := r.q.GetRegionAPIKeyByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apikey.RegionKey{}, fmt.Errorf("sqlite: get region api key by hash: %w", apikey.ErrNotFound)
		}
		return apikey.RegionKey{}, fmt.Errorf("sqlite: get region api key by hash: %w", err)
	}
	key, err := regionKeyFromRow(row)
	if err != nil {
		return apikey.RegionKey{}, err
	}
	if key.RevokedAt != nil {
		// The row is still returned: the middleware logs a replay of a
		// revoked key with the key's id, which ErrNotFound alone could not
		// carry (design spec section 4.2).
		return key, apikey.ErrRevoked
	}
	return key, nil
}

func (r *apiKeyRepo) ListRegionKeys(ctx context.Context, regionID int64) ([]apikey.RegionKey, error) {
	rows, err := r.q.ListRegionAPIKeys(ctx, regionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list region api keys for region %d: %w", regionID, err)
	}
	return regionKeysFromRows(rows)
}

func (r *apiKeyRepo) ListRegionKeysByCreator(ctx context.Context, by apikey.Actor) ([]apikey.RegionKey, error) {
	var rows []gen.RegionApiKey
	var err error
	// A bare "created_by_id = ?" bound to NULL matches no row in SQL, so the
	// CLI case -- created_by_id IS NULL -- has its own query rather than
	// sharing this one (apikeys.sql).
	if by.Kind == apikey.ActorCLI {
		rows, err = r.q.ListRegionAPIKeysByCLI(ctx)
	} else {
		rows, err = r.q.ListRegionAPIKeysByCreator(ctx, gen.ListRegionAPIKeysByCreatorParams{
			CreatedByKind: by.Kind,
			CreatedByID:   sql.NullInt64{Int64: by.ID, Valid: true},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: list region api keys by creator: %w", err)
	}
	return regionKeysFromRows(rows)
}

// RevokeRegionKey reads the row and writes the revocation in one immediate
// transaction (the store opens with _txlock=immediate), so a concurrent
// revoker waits on busy_timeout at BEGIN rather than racing this read. A
// single "UPDATE ... WHERE revoked_at IS NULL" cannot by itself distinguish
// "no such row" from "already revoked" -- both return zero rows -- and those
// two cases must map to different results (ErrNotFound vs. a no-op success).
func (r *apiKeyRepo) RevokeRegionKey(ctx context.Context, regionID, id int64, by apikey.Actor, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: revoke region api key %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	row, err := q.GetRegionAPIKey(ctx, gen.GetRegionAPIKeyParams{ID: id, RegionID: regionID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: revoke region api key %d: %w", id, apikey.ErrNotFound)
		}
		return fmt.Errorf("sqlite: revoke region api key %d: %w", id, err)
	}
	if row.RevokedAt.Valid {
		// Already revoked: a no-op success that must not move the original
		// revocation timestamp, so nothing is written.
		return nil
	}

	kind, actorID := actorToColumns(by)
	if _, err := q.RevokeRegionAPIKey(ctx, gen.RevokeRegionAPIKeyParams{
		RevokedAt:     sql.NullInt64{Int64: now.Unix(), Valid: true},
		RevokedByKind: sql.NullString{String: kind, Valid: true},
		RevokedByID:   actorID,
		ID:            id,
		RegionID:      regionID,
	}); err != nil {
		return fmt.Errorf("sqlite: revoke region api key %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: revoke region api key %d: commit: %w", id, err)
	}
	return nil
}

func (r *apiKeyRepo) RevokeRegionKeysByCreator(ctx context.Context, minted, by apikey.Actor, now time.Time) ([]int64, error) {
	kind, actorID := actorToColumns(by)
	revokedAt := sql.NullInt64{Int64: now.Unix(), Valid: true}
	revokedByKind := sql.NullString{String: kind, Valid: true}

	var ids []int64
	var err error
	if minted.Kind == apikey.ActorCLI {
		ids, err = r.q.RevokeRegionAPIKeysByCLI(ctx, gen.RevokeRegionAPIKeysByCLIParams{
			RevokedAt: revokedAt, RevokedByKind: revokedByKind, RevokedByID: actorID,
		})
	} else {
		ids, err = r.q.RevokeRegionAPIKeysByCreator(ctx, gen.RevokeRegionAPIKeysByCreatorParams{
			RevokedAt: revokedAt, RevokedByKind: revokedByKind, RevokedByID: actorID,
			CreatedByKind: minted.Kind,
			CreatedByID:   sql.NullInt64{Int64: minted.ID, Valid: true},
		})
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: revoke region api keys by creator: %w", err)
	}
	// UPDATE ... RETURNING makes no ordering promise; the interface does.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (r *apiKeyRepo) TouchRegionKey(ctx context.Context, id int64, now time.Time) error {
	if err := r.q.TouchRegionAPIKey(ctx, gen.TouchRegionAPIKeyParams{
		LastUsedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		ID:         id,
	}); err != nil {
		return fmt.Errorf("sqlite: touch region api key %d: %w", id, err)
	}
	return nil
}

func (r *apiKeyRepo) CreatePrincipal(ctx context.Context, name, keyHash string, now time.Time) (apikey.ServicePrincipal, error) {
	row, err := r.q.CreateServicePrincipal(ctx, gen.CreateServicePrincipalParams{
		Name: name, KeyHash: keyHash, CreatedAt: now.Unix(),
	})
	if err != nil {
		return apikey.ServicePrincipal{}, fmt.Errorf("sqlite: create service principal %q: %w", name, err)
	}
	return principalFromRow(row), nil
}

func (r *apiKeyRepo) GetPrincipalByHash(ctx context.Context, keyHash string) (apikey.ServicePrincipal, error) {
	row, err := r.q.GetServicePrincipalByHash(ctx, keyHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apikey.ServicePrincipal{}, fmt.Errorf("sqlite: get service principal by hash: %w", apikey.ErrNotFound)
		}
		return apikey.ServicePrincipal{}, fmt.Errorf("sqlite: get service principal by hash: %w", err)
	}
	p := principalFromRow(row)
	if p.RevokedAt != nil {
		return p, apikey.ErrRevoked
	}
	return p, nil
}

func (r *apiKeyRepo) ListPrincipals(ctx context.Context) ([]apikey.ServicePrincipal, error) {
	rows, err := r.q.ListServicePrincipals(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list service principals: %w", err)
	}
	out := make([]apikey.ServicePrincipal, len(rows))
	for i, row := range rows {
		out[i] = principalFromRow(row)
	}
	return out, nil
}

// RevokePrincipal mirrors RevokeRegionKey: read then write in one immediate
// transaction, so "no such row" and "already revoked" map to ErrNotFound and
// a no-op success respectively, and a concurrent revoker cannot race the read.
func (r *apiKeyRepo) RevokePrincipal(ctx context.Context, id int64, now time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: revoke service principal %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	row, err := q.GetServicePrincipal(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: revoke service principal %d: %w", id, apikey.ErrNotFound)
		}
		return fmt.Errorf("sqlite: revoke service principal %d: %w", id, err)
	}
	if row.RevokedAt.Valid {
		return nil
	}

	if _, err := q.RevokeServicePrincipal(ctx, gen.RevokeServicePrincipalParams{
		RevokedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		ID:        id,
	}); err != nil {
		return fmt.Errorf("sqlite: revoke service principal %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: revoke service principal %d: commit: %w", id, err)
	}
	return nil
}

func (r *apiKeyRepo) TouchPrincipal(ctx context.Context, id int64, now time.Time) error {
	if err := r.q.TouchServicePrincipal(ctx, gen.TouchServicePrincipalParams{
		LastUsedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		ID:         id,
	}); err != nil {
		return fmt.Errorf("sqlite: touch service principal %d: %w", id, err)
	}
	return nil
}
