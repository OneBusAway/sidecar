package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/lease"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

type leaseRepo struct {
	q *gen.Queries
}

var _ lease.Repository = (*leaseRepo)(nil)

// Acquire is one atomic upsert (see queries/leases.sql): a refused
// acquisition surfaces as sql.ErrNoRows from the RETURNING clause, which
// is the "not acquired" answer, not an error.
func (r *leaseRepo) Acquire(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	_, err := r.q.AcquireLease(ctx, gen.AcquireLeaseParams{
		Name:      name,
		Holder:    holder,
		ExpiresAt: now.Add(ttl).Unix(),
		Now:       now.Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sqlite: acquire lease %q: %w", name, err)
	}
	return true, nil
}

func (r *leaseRepo) Release(ctx context.Context, name, holder string) error {
	if err := r.q.ReleaseLease(ctx, gen.ReleaseLeaseParams{Name: name, Holder: holder}); err != nil {
		return fmt.Errorf("sqlite: release lease %q: %w", name, err)
	}
	return nil
}
