package apikey_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

func TestParseScopes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      []string
		want    apikey.Scopes
		wantErr error
	}{
		{"nil is empty, never nil", nil, apikey.Scopes{}, nil},
		{"empty is empty", []string{}, apikey.Scopes{}, nil},
		{"push", []string{"push"}, apikey.Scopes{apikey.ScopePush}, nil},
		{"duplicates collapse", []string{"push", "push"}, apikey.Scopes{apikey.ScopePush}, nil},
		{"surrounding whitespace is trimmed", []string{" push "}, apikey.Scopes{apikey.ScopePush}, nil},
		{"unknown", []string{"admin"}, nil, apikey.ErrUnknownScope},
		{"case matters", []string{"Push"}, nil, apikey.ErrUnknownScope},
		{"blank", []string{""}, nil, apikey.ErrUnknownScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := apikey.ParseScopes(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (got == nil || !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("scopes = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestScopes_HasAndStrings(t *testing.T) {
	t.Parallel()
	var none apikey.Scopes
	if none.Has(apikey.ScopePush) {
		t.Error("nil Scopes must not have push")
	}
	if got := none.Strings(); got == nil || len(got) != 0 {
		t.Errorf("nil Scopes.Strings() = %#v, want an empty non-nil slice", got)
	}
	s := apikey.Scopes{apikey.ScopePush}
	if !s.Has(apikey.ScopePush) {
		t.Error("Has(push) = false")
	}
	if got := s.Strings(); !reflect.DeepEqual(got, []string{"push"}) {
		t.Errorf("Strings() = %#v", got)
	}
}
