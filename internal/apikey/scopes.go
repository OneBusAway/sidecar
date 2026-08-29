package apikey

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Scope is one named capability a region key carries on top of the
// ordinary region-scoped authoring surface. A key with no scopes is the
// key the keys design spec section 2.1 describes; each scope widens its
// blast radius by exactly one stated thing, so the set is closed and every
// name is spelled here.
type Scope string

// ScopePush lets a region key send and cancel alert pushes for its own
// region (POST/DELETE .../pushes). It exists for OBACloud, which drives the
// sidecar's push routes at send time (migration design spec section 2.2);
// a leaked push-scoped key can deliver notifications to every device in
// its region, and the remedy remains revocation.
const ScopePush Scope = "push"

// ErrUnknownScope is returned by ParseScopes for any name that is not a
// defined Scope. It is a client error (400 at the admin API), never a
// silently dropped value: a key minted without the scope the caller asked
// for would fail later, at send time, in a different process.
var ErrUnknownScope = errors.New("unknown scope")

// Scopes is a key's scope set: sorted, deduplicated, and never nil once it
// has been through ParseScopes, so it marshals as [] rather than null.
type Scopes []Scope

// ParseScopes validates and normalizes a list of scope names. Whitespace
// around a name is trimmed; anything else that is not exactly a defined
// scope is ErrUnknownScope. nil and empty input both yield an empty,
// non-nil set.
func ParseScopes(names []string) (Scopes, error) {
	seen := make(map[Scope]bool, len(names))
	out := Scopes{}
	for _, n := range names {
		s := Scope(strings.TrimSpace(n))
		if s != ScopePush {
			return nil, fmt.Errorf("%w %q", ErrUnknownScope, n)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Has reports whether want is in the set. A nil set has nothing.
func (s Scopes) Has(want Scope) bool {
	for _, got := range s {
		if got == want {
			return true
		}
	}
	return false
}

// Strings renders the set for JSON and the CLI: always a non-nil slice,
// so an empty set is [] on the wire.
func (s Scopes) Strings() []string {
	out := make([]string, 0, len(s))
	for _, sc := range s {
		out = append(out, string(sc))
	}
	return out
}
