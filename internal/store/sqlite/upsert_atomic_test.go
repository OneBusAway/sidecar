package sqlite

// This file is a white-box (package sqlite, not sqlite_test) test: it needs
// to build a *Store around a private, fault-injecting driver registration,
// which requires constructing the unexported Store and regionRepo types
// directly rather than going through Open.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	modernc "modernc.org/sqlite"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// errInjected is the error faultyConn.Prepare returns once its allotted
// number of matching statements has been used up.
var errInjected = errors.New("faultyDriver: injected failure")

// faultyDriver wraps the real modernc.org/sqlite driver and, from its
// failAfter'th matching statement onward, fails Prepare instead of
// delegating. This lets a test force a write partway through a batch to
// fail deterministically -- without depending on goroutine-scheduling
// timing -- so UpsertFromDirectory's all-or-nothing transaction can be
// exercised directly rather than merely asserted by reading the code.
type faultyDriver struct {
	real      *modernc.Driver
	match     string
	failAfter int32
	count     int32
}

func (d *faultyDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.real.Open(name)
	if err != nil {
		return nil, err
	}
	return &faultyConn{Conn: conn, d: d}, nil
}

type faultyConn struct {
	driver.Conn
	d *faultyDriver
}

func (c *faultyConn) Prepare(query string) (driver.Stmt, error) {
	if strings.Contains(query, c.d.match) {
		if atomic.AddInt32(&c.d.count, 1) > c.d.failAfter {
			return nil, errInjected
		}
	}
	return c.Conn.Prepare(query)
}

// openFaulty opens a fresh, migrated *Store backed by a private driver
// registration whose failAfter'th (and every later) statement matching
// match fails with errInjected.
func openFaulty(t *testing.T, match string, failAfter int32) *Store {
	t.Helper()

	name := fmt.Sprintf("sqlite_faulty_%s_%d", t.Name(), time.Now().UnixNano())
	sql.Register(name, &faultyDriver{real: &modernc.Driver{}, match: match, failAfter: failAfter})

	path := filepath.Join(t.TempDir(), "test.db")
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open(name, dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := &Store{db: db, q: gen.New(db)}
	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return store
}

// TestUpsertFromDirectory_AtomicOnMidBatchFailure reproduces the finding
// that UpsertFromDirectory looped one upsert per region with no enclosing
// transaction: a mid-loop failure (e.g. SQLITE_BUSY past the busy_timeout)
// left the table half-refreshed while Sync logged the whole refresh as
// failed -- an operator reading that log would believe nothing changed when
// rows were actually a mix of new and stale. The third region's INSERT is
// made to fail here; the first two would otherwise have succeeded on their
// own. With the loop wrapped in one transaction, none of the three may be
// visible afterward.
func TestUpsertFromDirectory_AtomicOnMidBatchFailure(t *testing.T) {
	t.Parallel()

	store := openFaulty(t, "INSERT INTO regions", 2)

	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := []regions.Region{
		{ID: 1, Name: "Region 1", OBABaseURL: "https://example.org/1/", Active: true},
		{ID: 2, Name: "Region 2", OBABaseURL: "https://example.org/2/", Active: true},
		{ID: 3, Name: "Region 3", OBABaseURL: "https://example.org/3/", Active: true},
	}

	err := store.Regions().UpsertFromDirectory(ctx, in, now)
	if err == nil {
		t.Fatal("UpsertFromDirectory: want error from the injected failure on the third region, got nil")
	}
	if !errors.Is(err, errInjected) {
		t.Errorf("error = %v, want it to wrap the injected failure", err)
	}

	for _, id := range []int64{1, 2, 3} {
		if _, getErr := store.Regions().Get(ctx, id); !errors.Is(getErr, regions.ErrNotFound) {
			t.Errorf("Get(%d) after a failed UpsertFromDirectory = %v, want regions.ErrNotFound (the whole batch must roll back, not leave the first two committed)", id, getErr)
		}
	}
}

// TestUpsertFromDirectory_SucceedsWhenNothingFails is the control: the same
// setup with no injected failure must commit every region, guarding against
// an overzealous fix that never commits at all.
func TestUpsertFromDirectory_SucceedsWhenNothingFails(t *testing.T) {
	t.Parallel()

	// failAfter larger than the batch size: nothing is ever injected.
	store := openFaulty(t, "INSERT INTO regions", 100)

	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	in := []regions.Region{
		{ID: 1, Name: "Region 1", OBABaseURL: "https://example.org/1/", Active: true},
		{ID: 2, Name: "Region 2", OBABaseURL: "https://example.org/2/", Active: true},
	}

	if err := store.Regions().UpsertFromDirectory(ctx, in, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	for _, id := range []int64{1, 2} {
		if _, err := store.Regions().Get(ctx, id); err != nil {
			t.Errorf("Get(%d): %v, want no error", id, err)
		}
	}
}
