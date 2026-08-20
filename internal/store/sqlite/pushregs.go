package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// Error strings from this repo deliberately never embed token values:
// tokens are device-addressable secrets (spec section 13), errors get
// logged verbatim by callers, and scrubbing at the source is what makes
// httpapi's sanitizeToken defense-in-depth rather than the only line.
type pushRegRepo struct {
	db *sql.DB
	q  *gen.Queries
}

// upsertPushRegistrationSQL is hand-written rather than sqlc-generated: see
// the comment at the top of queries/pushregs.sql for why. It is a single
// atomic statement -- the whole point being that two concurrent first
// registrations for the same (region, token) both hit this one INSERT, one
// wins the row and the other transparently follows the ON CONFLICT branch,
// with no application-level retry.
//
// The three CASE WHEN ? THEN excluded.col ELSE push_registrations.col END
// expressions are the sticky-field semantics (spec section 4): each of
// locale, test_device, and description carries its own independent
// set-flag, so a caller can overwrite one without touching the others.
// operating_system, apns_sandbox, and last_seen_at have no such flag --
// every registration restates its own build's platform and APNs
// environment, and every registration is by definition a sighting.
const upsertPushRegistrationSQL = `
INSERT INTO push_registrations (
  region_id, token, operating_system, apns_sandbox,
  locale, test_device, description,
  last_seen_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (region_id, token) DO UPDATE SET
  operating_system = excluded.operating_system,
  apns_sandbox     = excluded.apns_sandbox,
  locale           = CASE WHEN CAST(? AS BOOLEAN) THEN excluded.locale      ELSE push_registrations.locale      END,
  test_device      = CASE WHEN CAST(? AS BOOLEAN) THEN excluded.test_device ELSE push_registrations.test_device END,
  description      = CASE WHEN CAST(? AS BOOLEAN) THEN excluded.description ELSE push_registrations.description END,
  last_seen_at     = excluded.last_seen_at,
  updated_at       = excluded.updated_at
`

// deref returns *p, or fallback if p is nil.
func deref[T any](p *T, fallback T) T {
	if p == nil {
		return fallback
	}
	return *p
}

func (r *pushRegRepo) Upsert(ctx context.Context, in pushreg.Upsert, now time.Time) error {
	ts := now.Unix()
	_, err := r.db.ExecContext(ctx, upsertPushRegistrationSQL,
		in.RegionID, in.Token, in.OperatingSystem, in.APNSSandbox,
		deref(in.Locale, ""), deref(in.TestDevice, false), deref(in.Description, ""),
		ts, ts, ts,
		in.Locale != nil, in.TestDevice != nil, in.Description != nil,
	)
	if err != nil {
		return fmt.Errorf("sqlite: upsert push registration (region %d): %w", in.RegionID, err)
	}
	return nil
}

func pushRegistrationFromRow(row gen.PushRegistration) pushreg.Registration {
	return pushreg.Registration{
		RegionID:        row.RegionID,
		Token:           row.Token,
		OperatingSystem: row.OperatingSystem,
		Locale:          row.Locale,
		APNSSandbox:     row.ApnsSandbox,
		TestDevice:      row.TestDevice,
		Description:     row.Description,
		LastSeenAt:      unixToTime(row.LastSeenAt),
		CreatedAt:       unixToTime(row.CreatedAt),
	}
}

func (r *pushRegRepo) Get(ctx context.Context, regionID int64, token string) (pushreg.Registration, error) {
	row, err := r.q.GetPushRegistration(ctx, gen.GetPushRegistrationParams{RegionID: regionID, Token: token})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return pushreg.Registration{}, fmt.Errorf("sqlite: get push registration (region %d): %w", regionID, pushreg.ErrNotFound)
		}
		return pushreg.Registration{}, fmt.Errorf("sqlite: get push registration (region %d): %w", regionID, err)
	}
	return pushRegistrationFromRow(row), nil
}

func (r *pushRegRepo) Delete(ctx context.Context, regionID int64, token string) error {
	n, err := r.q.DeletePushRegistration(ctx, gen.DeletePushRegistrationParams{RegionID: regionID, Token: token})
	if err != nil {
		return fmt.Errorf("sqlite: delete push registration (region %d): %w", regionID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete push registration (region %d): %w", regionID, pushreg.ErrNotFound)
	}
	return nil
}

func (r *pushRegRepo) DeleteByToken(ctx context.Context, token string) (int64, error) {
	n, err := r.q.DeletePushRegistrationsByToken(ctx, token)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete push registrations by token: %w", err)
	}
	return n, nil
}

func (r *pushRegRepo) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := r.q.PrunePushRegistrations(ctx, cutoff.Unix())
	if err != nil {
		return 0, fmt.Errorf("sqlite: prune push registrations before %v: %w", cutoff, err)
	}
	return n, nil
}
