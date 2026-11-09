package sqlite

// White-box test (package sqlite): it opens the store through a private,
// gating driver registration via openWithDriver (upsert_atomic_test.go).

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	modernc "modernc.org/sqlite"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// gateDriver wraps the real driver so a test can stop one call at a chosen
// statement and learn when another call has started its transaction. Both
// hooks are one-shot and disarmed until the test arms them, so migrations
// and setup writes pass straight through.
type gateDriver struct {
	real *modernc.Driver

	// gateDelete, once armed, holds the next "DELETE FROM users" prepare:
	// it closes deleteReached and waits for releaseDelete.
	gateDelete    atomic.Bool
	deleteReached chan struct{}
	releaseDelete chan struct{}

	// signalBegin, once armed, closes beginEntered when the next BeginTx
	// starts, before the driver takes any lock.
	signalBegin  atomic.Bool
	beginEntered chan struct{}
}

func newGateDriver() *gateDriver {
	return &gateDriver{
		real:          &modernc.Driver{},
		deleteReached: make(chan struct{}),
		releaseDelete: make(chan struct{}),
		beginEntered:  make(chan struct{}),
	}
}

func (d *gateDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.real.Open(name)
	if err != nil {
		return nil, err
	}
	return &gateConn{Conn: conn, d: d}, nil
}

type gateConn struct {
	driver.Conn
	d *gateDriver
}

func (c *gateConn) Prepare(query string) (driver.Stmt, error) {
	if strings.Contains(query, "DELETE FROM users") && c.d.gateDelete.CompareAndSwap(true, false) {
		close(c.d.deleteReached)
		<-c.d.releaseDelete
	}
	return c.Conn.Prepare(query)
}

func (c *gateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.d.signalBegin.CompareAndSwap(true, false) {
		close(c.d.beginEntered)
	}
	beginner, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("gateConn: wrapped connection does not implement driver.ConnBeginTx")
	}
	return beginner.BeginTx(ctx, opts)
}

// TestDeleteUserUnlessLast_SerializesConcurrentDeletes forces the
// interleaving that empties the users table under a check-then-delete: A
// has checked the count and is about to delete, and B starts while A is
// still there. A is held at its DELETE prepare until B has entered BeginTx,
// so anything B reads before its transaction is guaranteed to predate A's
// delete. With the lookup, count, and delete all inside one immediate
// transaction, B's BEGIN waits for A to commit and its count then sees one
// account. The ordering comes from the gates alone; nothing here sleeps.
func TestDeleteUserUnlessLast_SerializesConcurrentDeletes(t *testing.T) {
	t.Parallel()

	d := newGateDriver()
	store := openWithDriver(t, "gate", d)
	repo := store.Auth()
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, name := range []string{"alice", "bob"} {
		if _, err := repo.CreateUser(ctx, name, "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", now); err != nil {
			t.Fatalf("CreateUser(%s): %v", name, err)
		}
	}

	// Never leave A parked on the gate if the test fails early.
	var release sync.Once
	releaseA := func() { release.Do(func() { close(d.releaseDelete) }) }
	t.Cleanup(releaseA)

	d.gateDelete.Store(true)
	aDone := make(chan error, 1)
	go func() { aDone <- repo.DeleteUserUnlessLast(ctx, "alice") }()

	select {
	case <-d.deleteReached:
	case err := <-aDone:
		t.Fatalf("DeleteUserUnlessLast(alice) returned %v without reaching its DELETE", err)
	}

	d.signalBegin.Store(true)
	bDone := make(chan error, 1)
	go func() { bDone <- repo.DeleteUserUnlessLast(ctx, "bob") }()

	// B either enters its transaction, or (in an implementation with no
	// transaction) finishes without one; either way A is released next.
	var errB error
	bFinished := false
	select {
	case <-d.beginEntered:
	case errB = <-bDone:
		bFinished = true
	}

	releaseA()
	errA := <-aDone
	if !bFinished {
		errB = <-bDone
	}

	if errA != nil {
		t.Errorf("DeleteUserUnlessLast(alice) = %v, want nil (it held the lock first)", errA)
	}
	if !errors.Is(errB, auth.ErrLastUser) {
		t.Errorf("DeleteUserUnlessLast(bob) = %v, want auth.ErrLastUser (alice's delete committed first)", errB)
	}

	users, err := repo.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 || users[0].Username != "bob" {
		t.Errorf("ListUsers after both deletes = %+v, want only bob", users)
	}
}
