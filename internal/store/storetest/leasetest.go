package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/lease"
)

// newLeaseStoreFunc is shorthand for the callback every lease subtest
// receives: a fresh, migrated lease repository.
type newLeaseStoreFunc func(*testing.T) lease.Repository

// RunLeaseRepository exercises a lease.Repository against the contract the
// background-loop runner depends on (spec section 12: every loop must be
// safe under at-least-once execution, and with a shared database that
// starts with at most one process running each loop at a time). Each
// subtest gets a fresh store from newStore.
func RunLeaseRepository(t *testing.T, newStore newLeaseStoreFunc) {
	t.Helper()

	t.Run("FreshLeaseIsAcquired", func(t *testing.T) { testFreshLeaseIsAcquired(t, newStore) })
	t.Run("HolderRenewsItsOwnLease", func(t *testing.T) { testHolderRenewsItsOwnLease(t, newStore) })
	t.Run("OtherHolderBlockedUntilExpiry", func(t *testing.T) { testOtherHolderBlockedUntilExpiry(t, newStore) })
	t.Run("ReleaseFreesTheLease", func(t *testing.T) { testReleaseFreesTheLease(t, newStore) })
	t.Run("ReleaseByNonHolderIsNoop", func(t *testing.T) { testReleaseByNonHolderIsNoop(t, newStore) })
	t.Run("NamesAreIndependent", func(t *testing.T) { testLeaseNamesAreIndependent(t, newStore) })
}

const leaseTTL = time.Minute

func mustAcquire(t *testing.T, repo lease.Repository, name, holder string, now time.Time, want bool) {
	t.Helper()
	got, err := repo.Acquire(context.Background(), name, holder, now, leaseTTL)
	if err != nil {
		t.Fatalf("Acquire(%q, %q, %v): %v", name, holder, now, err)
	}
	if got != want {
		t.Fatalf("Acquire(%q, %q, %v) = %v, want %v", name, holder, now, got, want)
	}
}

func testFreshLeaseIsAcquired(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
}

// testHolderRenewsItsOwnLease: re-acquiring is how a live holder keeps the
// lease, so it must succeed and push the expiry out -- a second holder is
// still refused after the ORIGINAL expiry has passed.
func testHolderRenewsItsOwnLease(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
	mustAcquire(t, repo, "alarms", "a", base.Add(45*time.Second), true)
	mustAcquire(t, repo, "alarms", "b", base.Add(leaseTTL), false)
}

// testOtherHolderBlockedUntilExpiry: expiry is inclusive -- at exactly
// expires_at the lease is free, so a holder that died is replaced after
// one TTL, not one TTL plus a second.
func testOtherHolderBlockedUntilExpiry(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
	mustAcquire(t, repo, "alarms", "b", base.Add(leaseTTL-time.Second), false)
	mustAcquire(t, repo, "alarms", "b", base.Add(leaseTTL), true)
	// b now holds it: a's later attempt inside b's TTL is refused.
	mustAcquire(t, repo, "alarms", "a", base.Add(leaseTTL+time.Second), false)
}

func testReleaseFreesTheLease(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
	if err := repo.Release(context.Background(), "alarms", "a"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	mustAcquire(t, repo, "alarms", "b", base.Add(time.Second), true)
}

func testReleaseByNonHolderIsNoop(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
	if err := repo.Release(context.Background(), "alarms", "b"); err != nil {
		t.Fatalf("Release(non-holder): %v", err)
	}
	mustAcquire(t, repo, "alarms", "b", base.Add(time.Second), false)
	// Releasing a name nobody holds is equally harmless.
	if err := repo.Release(context.Background(), "nothing", "b"); err != nil {
		t.Fatalf("Release(unknown name): %v", err)
	}
}

func testLeaseNamesAreIndependent(t *testing.T, newStore newLeaseStoreFunc) {
	repo := newStore(t)
	mustAcquire(t, repo, "alarms", "a", base, true)
	mustAcquire(t, repo, "live-activities", "b", base, true)
	mustAcquire(t, repo, "alarms", "b", base, false)
}
