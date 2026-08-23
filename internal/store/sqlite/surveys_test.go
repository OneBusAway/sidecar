package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestOpenUsesImmediateWriteTransactions pins the DSN's _txlock=immediate
// flag (design spec surveys 2.6, 3.2) directly, since the shared
// conformance suite's ConcurrentAmendsBothLand only observes it
// probabilistically -- on a fast, many-core machine, a fresh *sql.DB opened
// once per attempt tends to serialize its two racing writers through the
// pool's connection-warmup asymmetry before their read snapshots can ever
// diverge, so removing the flag rarely fails that test in a low iteration
// count. This test instead asserts the locking behavior deterministically:
// with a deferred (non-immediate) BEGIN, the second BeginTx below returns
// right away instead of blocking, so this test fails on its very first
// assertion the moment _txlock=immediate is removed from the DSN.
func TestOpenUsesImmediateWriteTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.db.Close() })
	if err = s.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	ctx := context.Background()

	tx1, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx tx1: %v", err)
	}

	unblocked := make(chan struct{})
	var tx2Err error
	go func() {
		tx2, err := s.db.BeginTx(ctx, nil)
		tx2Err = err
		if tx2 != nil {
			_ = tx2.Rollback()
		}
		close(unblocked)
	}()

	select {
	case <-unblocked:
		t.Fatal("second BEGIN returned before the first write transaction released its lock")
	case <-time.After(200 * time.Millisecond):
	}

	// ReadOnly transactions are unaffected by _txlock=immediate (modernc
	// tx.go skips immediate mode for ReadOnly): with tx1 still holding the
	// write lock, a ReadOnly BEGIN must return immediately rather than
	// queue behind it.
	roDone := make(chan error, 1)
	go func() {
		tx3, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if tx3 != nil {
			_ = tx3.Rollback()
		}
		roDone <- err
	}()
	select {
	case err := <-roDone:
		if err != nil {
			t.Fatalf("ReadOnly BeginTx: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("ReadOnly BeginTx blocked behind the open write transaction")
	}

	if err := tx1.Rollback(); err != nil {
		t.Fatalf("tx1.Rollback: %v", err)
	}

	select {
	case <-unblocked:
	case <-time.After(5 * time.Second):
		t.Fatal("second BEGIN never unblocked after the first transaction released its lock")
	}
	if tx2Err != nil {
		t.Fatalf("second BeginTx: %v", tx2Err)
	}
}

// TestListToNull pins the design spec 2.11 invariant that the shared
// conformance suite cannot observe directly: an empty or nil targeting
// list stores NULL, never the literal string "[]" (which the Android
// client would otherwise read as "targets zero stops/routes" instead of
// "everywhere").
func TestListToNull(t *testing.T) {
	for _, in := range [][]string{nil, {}} {
		got, err := listToNull(in)
		if err != nil {
			t.Fatalf("listToNull(%#v): %v", in, err)
		}
		if got.Valid {
			t.Errorf("listToNull(%#v) = %+v, want Valid == false", in, got)
		}
	}

	got, err := listToNull([]string{"1_570"})
	if err != nil {
		t.Fatalf("listToNull: %v", err)
	}
	if !got.Valid || got.String != `["1_570"]` {
		t.Errorf(`listToNull([1_570]) = %+v, want Valid == true, String == ["1_570"]`, got)
	}

	if out, err := nullToList(sql.NullString{}); err != nil || out != nil {
		t.Errorf("nullToList(invalid) = %#v, %v, want nil, nil", out, err)
	}
	if out, err := nullToList(sql.NullString{String: "[]", Valid: true}); err != nil || out != nil {
		t.Errorf(`nullToList("[]") = %#v, %v, want nil, nil`, out, err)
	}
	if out, err := nullToList(sql.NullString{String: `["a"]`, Valid: true}); err != nil || !reflect.DeepEqual(out, []string{"a"}) {
		t.Errorf(`nullToList(["a"]) = %#v, %v, want []string{"a"}, nil`, out, err)
	}
}
