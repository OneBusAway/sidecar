package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

// TestAPIKeys_MintReturnsTheRawKeyOnce pins the whole 201 contract. The raw
// key appears here and nowhere else: not in a Location header, not in a URL,
// not in a log line.
func TestAPIKeys_MintReturnsTheRawKeyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"obacloud prod"}`)
	got := object(t, rec, http.StatusCreated)
	assertKeys(t, "api key", got, []string{"id", "name", "key", "scopes", "created_by", "created_at"})

	raw, _ := got["key"].(string)
	if !strings.HasPrefix(raw, "obask_1_") {
		t.Fatalf("key = %q, want an obask_1_ prefix", raw)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("a Location header would put the raw key in a URL")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if got["name"] != "obacloud prod" {
		t.Errorf("name = %v", got["name"])
	}
	createdBy, _ := got["created_by"].(map[string]any)
	if createdBy["kind"] != "operator" {
		t.Errorf("created_by = %v, want kind operator", got["created_by"])
	}
	if strings.Contains(buf.String(), raw) || strings.Contains(buf.String(), strings.TrimPrefix(raw, "obask_1_")) {
		t.Errorf("the raw key reached a log line:\n%s", buf.String())
	}

	// The key it returned actually works, and only for its own region.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Errorf("minted key against its own region: status = %d, want 200", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer "+raw); rec.Code != http.StatusNotFound {
		t.Errorf("minted key against another region: status = %d, want 404", rec.Code)
	}
}

// TestAPIKeys_NameValidation. The name is 1-100 BYTES after stripping
// control characters and trimming; a compromised principal controls this
// string, and `key list` prints it to a terminal.
//
// The multi-byte rows below are what actually pin the BYTE (not rune) cap:
// 33 copies of the three-byte rune "中" are 99 bytes -- under the cap -- and
// 34 copies are 102 bytes -- over it -- even though 34 runes is nowhere near
// 100. A length check written as utf8.RuneCountInString instead of len()
// would pass both rows, so this is the case that actually catches that bug;
// the plain-ASCII 100/101-byte rows above only pin the boundary's numeric
// value.
func TestAPIKeys_NameValidation(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"blank", `{"name":""}`, http.StatusUnprocessableEntity},
		{"whitespace only", `{"name":"   "}`, http.StatusUnprocessableEntity},
		{"control chars only", "{\"name\":\"\\u0007\\u0007\"}", http.StatusUnprocessableEntity},
		{"missing", `{}`, http.StatusUnprocessableEntity},
		{"101 bytes", `{"name":"` + strings.Repeat("a", 101) + `"}`, http.StatusUnprocessableEntity},
		{"100 bytes", `{"name":"` + strings.Repeat("a", 100) + `"}`, http.StatusCreated},
		{"33 three-byte runes (99 bytes)", `{"name":"` + strings.Repeat("中", 33) + `"}`, http.StatusCreated},
		{"34 three-byte runes (102 bytes)", `{"name":"` + strings.Repeat("中", 34) + `"}`, http.StatusUnprocessableEntity},
		{"malformed json", `{`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAPIKeys_NameControlCharsAreStripped.
func TestAPIKeys_NameControlCharsAreStripped(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	// A name carrying an ANSI escape would repaint the terminal of whoever
	// runs `key list` -- which, after a principal compromise, is exactly the
	// operator trying to read the list.
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys",
		"{\"name\":\"ob\\u001b[2Jacloud\"}")
	got := object(t, rec, http.StatusCreated)
	if got["name"] != "ob[2Jacloud" {
		t.Errorf("name = %q, want the escape byte stripped", got["name"])
	}
}

// TestAPIKeys_ListNeverEchoesTheKey.
func TestAPIKeys_ListNeverEchoesTheKey(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mint := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a"}`), http.StatusCreated)
	raw, _ := mint["key"].(string)

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d keys, want 1", len(list))
	}
	assertKeys(t, "api key", list[0],
		[]string{"id", "name", "scopes", "created_by", "created_at", "last_used_at", "revoked_at", "revoked_by"})
	if strings.Contains(bodyText(f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", "")), raw) {
		t.Error("the list echoed the raw key")
	}
}

// TestAPIKeys_ListIsMakeNotNil pins the "empty list marshals as [], not
// null" contract the brief calls for: a region with no keys yet must still
// answer with a JSON array a client can range over without a nil check.
func TestAPIKeys_ListIsMakeNotNil(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", "")
	if got, want := bodyText(rec), "[]"; got != want {
		t.Errorf("body = %q, want %q (an empty array, not null)", got, want)
	}
}

// TestAPIKeys_RevokeIsRegionScopedAndIdempotent.
func TestAPIKeys_RevokeIsRegionScopedAndIdempotent(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mint := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a"}`), http.StatusCreated)
	id := jsonID(t, mint)
	raw, _ := mint["key"].(string)

	// Another region's key id is a 404, not a successful revoke.
	if rec := f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/0/api_keys/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Errorf("cross-region revoke: status = %d, want 404", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Fatalf("the key must still be live: status = %d", rec.Code)
	}

	for i := 0; i < 2; i++ {
		if rec := f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/1/api_keys/%d", id), ""); rec.Code != http.StatusNoContent {
			t.Fatalf("revoke %d: status = %d, want 204", i, rec.Code)
		}
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/admin/v1/regions/1/api_keys/99999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key id: status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/admin/v1/regions/1/api_keys/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("unparseable key id: status = %d, want 400", rec.Code)
	}
}

// TestAPIKeys_ServicePrincipalCanMintAnywhereButReadNothing is the whole
// point of the separate scope: the principal's reach is key management in
// every region, and nothing else anywhere.
func TestAPIKeys_ServicePrincipalCanMintAnywhereButReadNothing(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	sp := f.mintPrincipal(t)

	for _, region := range []string{"0", "1", "2"} {
		rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/"+region+"/api_keys",
			`{"name":"obacloud"}`, "Bearer "+sp)
		got := object(t, rec, http.StatusCreated)
		createdBy, _ := got["created_by"].(map[string]any)
		if createdBy["kind"] != "principal" {
			t.Errorf("region %s: created_by = %v, want kind principal", region, got["created_by"])
		}
	}
	// A region that is not in the directory cannot be provisioned: regions
	// come from OBACloud's own export, so a principal can only mint keys for
	// regions OBACloud has published (design spec section 5.6).
	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/9999/api_keys",
		`{"name":"x"}`, "Bearer "+sp); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished region: status = %d, want 404", rec.Code)
	}
	// And it can read no tenant data.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+sp); rec.Code != http.StatusForbidden {
		t.Errorf("principal reading alerts: status = %d, want 403", rec.Code)
	}
}

// TestAPIKeys_RegionKeyCannotReachKeyManagement: a leaked region key must
// not be able to propagate (design spec section 2.2).
func TestAPIKeys_RegionKeyCannotReachKeyManagement(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"x"}`},
		{http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""},
		{http.MethodDelete, "/api/admin/v1/regions/1/api_keys/1", ""},
	} {
		rec := sendBearer(f.handler, tc.method, tc.target, tc.body, "Bearer "+raw)
		if rec.Code != http.StatusForbidden || bodyText(rec) != `{"error":"forbidden"}` {
			t.Errorf("%s %s: status = %d body = %s, want 403 forbidden", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}

// TestRouteTable_KeyAdminScopeIsExactlyTheAPIKeyFamily. scopeKeyAdmin is the
// one middleware that lets a service principal past a region, so the set of
// routes carrying it must be pinned rather than trusted.
func TestRouteTable_KeyAdminScopeIsExactlyTheAPIKeyFamily(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, rt := range adminRoutes(f.deps) {
		_, path, _ := strings.Cut(rt.pattern, " ")
		isKeyFamily := strings.HasSuffix(path, "/api_keys") || strings.HasSuffix(path, "/api_keys/{keyId}")
		if (rt.scope == scopeKeyAdmin) != isKeyFamily {
			t.Errorf("route %q: scope = %v, api_keys family = %v", rt.pattern, rt.scope, isKeyFamily)
		}
		if rt.allowed.has(principalService) && rt.scope != scopeKeyAdmin {
			t.Errorf("route %q allows a service principal outside scopeKeyAdmin", rt.pattern)
		}
	}
}

// TestAPIKeys_ScopesTakeEffect is the "field actually took effect" test
// the migration design spec section 2.2 demands: decodeJSON is lenient, so
// a misspelled or ignored scopes field would mint a key without push and
// fail later at send time. The assertion chain is request -> 201 body ->
// stored row -> list body -> the key can reach the push route.
func TestAPIKeys_ScopesTakeEffect(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"obacloud","scopes":["push"]}`)
	got := object(t, rec, http.StatusCreated)
	assertKeys(t, "api key", got, []string{"id", "name", "key", "scopes", "created_by", "created_at"})
	if scopes, _ := got["scopes"].([]any); len(scopes) != 1 || scopes[0] != "push" {
		t.Fatalf("minted scopes = %v, want [push]", got["scopes"])
	}
	raw, _ := got["key"].(string)

	keys, err := f.store.APIKeys().ListRegionKeys(context.Background(), regionPuget)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].Scopes.Has(apikey.ScopePush) {
		t.Fatalf("stored scopes = %+v, want push", keys)
	}

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""), http.StatusOK)
	assertKeys(t, "api key", list[0],
		[]string{"id", "name", "scopes", "created_by", "created_at", "last_used_at", "revoked_at", "revoked_by"})
	if scopes, _ := list[0]["scopes"].([]any); len(scopes) != 1 || scopes[0] != "push" {
		t.Errorf("listed scopes = %v, want [push]", list[0]["scopes"])
	}

	// The scope is honoured by the router, not just echoed: a push-scoped
	// key is not refused with 403 on the push route. (404 here: alert 1
	// does not exist in this fixture; what matters is that the allow-list
	// let the request through to the loader.)
	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/1/alerts/1/pushes", `{}`, "Bearer "+raw); rec.Code == http.StatusForbidden {
		t.Errorf("push-scoped key was refused on POST pushes: %s", rec.Body.String())
	}
}

// TestAPIKeys_ScopesValidation: unknown names are 400, and an absent or
// empty scopes field mints an unscoped key whose scopes marshal as [].
func TestAPIKeys_ScopesValidation(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"unknown scope", `{"name":"a","scopes":["admin"]}`, http.StatusBadRequest},
		{"blank scope", `{"name":"a","scopes":[""]}`, http.StatusBadRequest},
		{"wrong type", `{"name":"a","scopes":"push"}`, http.StatusBadRequest},
		{"absent", `{"name":"a"}`, http.StatusCreated},
		{"empty", `{"name":"a","scopes":[]}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusCreated {
				got := object(t, rec, http.StatusCreated)
				if scopes, ok := got["scopes"].([]any); !ok || len(scopes) != 0 {
					t.Errorf("scopes = %v (%T), want []", got["scopes"], got["scopes"])
				}
			}
		})
	}
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a","scopes":["admin"]}`); bodyText(rec) != `{"error":"unknown scope \"admin\""}` {
		t.Errorf("400 body = %s", bodyText(rec))
	}
}
