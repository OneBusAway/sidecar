package sqlite

import (
	"database/sql"
	"reflect"
	"testing"
)

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
