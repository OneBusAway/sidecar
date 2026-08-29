package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// newAPIKeyStoreFunc returns a fresh pair of repositories over one store.
// The regions repository is required because the cascade case needs a region
// to delete.
type newAPIKeyStoreFunc func(*testing.T) (apikey.Repository, regions.Repository)

// RunAPIKeyRepository exercises an apikey.Repository against the behavioral
// contract every engine must satisfy (design spec section 8).
func RunAPIKeyRepository(t *testing.T, newStore newAPIKeyStoreFunc) {
	t.Helper()

	t.Run("CreateGetRoundTrip", func(t *testing.T) { testAPIKeyRoundTrip(t, newStore) })
	t.Run("ScopesRoundTrip", func(t *testing.T) { testAPIKeyScopes(t, newStore) })
	t.Run("RevokedHashIsDistinctFromUnknown", func(t *testing.T) { testAPIKeyRevokedHash(t, newStore) })
	t.Run("RevokeIsRegionScoped", func(t *testing.T) { testAPIKeyRevokeRegionScoped(t, newStore) })
	t.Run("RevokeTwiceSucceeds", func(t *testing.T) { testAPIKeyRevokeIdempotent(t, newStore) })
	t.Run("ListByCreatorCoversAllThreeKinds", func(t *testing.T) { testAPIKeyListByCreator(t, newStore) })
	t.Run("RevokeByCreatorIsAtomic", func(t *testing.T) { testAPIKeyRevokeByCreator(t, newStore) })
	t.Run("TouchRecordsUse", func(t *testing.T) { testAPIKeyTouch(t, newStore) })
	t.Run("ListIsNewestFirst", func(t *testing.T) { testAPIKeyListOrder(t, newStore) })
	t.Run("KeysCascadeOnRegionDelete", func(t *testing.T) { testAPIKeyCascade(t, newStore) })
	t.Run("PrincipalLifecycle", func(t *testing.T) { testPrincipalLifecycle(t, newStore) })
}

// seedAPIKeyRegions upserts the two regions every subtest below uses. Region
// 0 is deliberately one of them: it is a real region, so a repository that
// treats 0 as "no region" fails here.
func seedAPIKeyRegions(t *testing.T, repo regions.Repository) {
	t.Helper()
	err := repo.UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 0, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", Language: "en", Active: true},
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Language: "en", Active: true},
	}, base)
	if err != nil {
		t.Fatalf("seed regions: %v", err)
	}
}

func testAPIKeyRoundTrip(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	by := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}
	created, err := keys.CreateRegionKey(ctx, 0, "obacloud", "hash-a", nil, by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	if created.ID == 0 {
		t.Error("CreateRegionKey returned id 0")
	}
	if created.RegionID != 0 || created.Name != "obacloud" || created.KeyHash != "hash-a" {
		t.Errorf("created = %+v, want region 0 / obacloud / hash-a", created)
	}
	if created.CreatedBy != by {
		t.Errorf("CreatedBy = %+v, want %+v", created.CreatedBy, by)
	}
	if !created.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, base)
	}
	if created.LastUsedAt != nil || created.RevokedAt != nil || created.RevokedBy != nil {
		t.Errorf("a fresh key must have no last-used or revocation: %+v", created)
	}

	got, err := keys.GetRegionKeyByHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("round trip id = %d, want %d", got.ID, created.ID)
	}

	if _, err := keys.GetRegionKeyByHash(ctx, "nope"); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}
}

func testAPIKeyRevokedHash(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", nil, apikey.Actor{Kind: apikey.ActorCLI}, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	revoker := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	later := base.Add(time.Hour)
	if revokeErr := keys.RevokeRegionKey(ctx, 1, k.ID, revoker, later); revokeErr != nil {
		t.Fatalf("RevokeRegionKey: %v", revokeErr)
	}

	// ErrRevoked, not ErrNotFound: a revoked key being replayed is the
	// clearest signal a credential leaked, and the middleware logs it
	// distinctly (design spec section 4.2).
	got, err := keys.GetRegionKeyByHash(ctx, "hash-a")
	if !errors.Is(err, apikey.ErrRevoked) {
		t.Fatalf("revoked hash: err = %v, want ErrRevoked", err)
	}
	if got.ID != k.ID {
		t.Errorf("ErrRevoked must still carry the row: id = %d, want %d", got.ID, k.ID)
	}

	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRegionKeys returned %d rows, want 1 (revoked rows are kept)", len(list))
	}
	if list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(later) {
		t.Errorf("RevokedAt = %v, want %v", list[0].RevokedAt, later)
	}
	if list[0].RevokedBy == nil || *list[0].RevokedBy != revoker {
		t.Errorf("RevokedBy = %+v, want %+v", list[0].RevokedBy, revoker)
	}
}

func testAPIKeyRevokeRegionScoped(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", nil, cli, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// The wrong region is ErrNotFound, never a successful revoke: this is
	// the fence that makes the {regionId} path segment real for the key
	// family (design spec section 3.2).
	if err := keys.RevokeRegionKey(ctx, 0, k.ID, cli, base); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("cross-region revoke: err = %v, want ErrNotFound", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "hash-a"); err != nil {
		t.Errorf("the key must still be live: %v", err)
	}
	if err := keys.RevokeRegionKey(ctx, 1, 99999, cli, base); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func testAPIKeyRevokeIdempotent(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", nil, cli, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	first := base.Add(time.Hour)
	if revokeErr := keys.RevokeRegionKey(ctx, 1, k.ID, cli, first); revokeErr != nil {
		t.Fatalf("first revoke: %v", revokeErr)
	}
	// A second revoke is a no-op SUCCESS: DELETE .../api_keys/{id} answers
	// 204 for an already-revoked key, and it must not move the timestamp
	// that records when the credential actually died.
	if revokeErr := keys.RevokeRegionKey(ctx, 1, k.ID, cli, base.Add(2*time.Hour)); revokeErr != nil {
		t.Fatalf("second revoke: err = %v, want nil", revokeErr)
	}
	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(first) {
		t.Errorf("RevokedAt = %v, want the first revocation %v", list[0].RevokedAt, first)
	}
}

func testAPIKeyListByCreator(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	operator := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}
	principal := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	other := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 5}

	for i, spec := range []struct {
		region int64
		by     apikey.Actor
		hash   string
	}{
		{0, cli, "h-cli"},
		{1, operator, "h-op"},
		{0, principal, "h-p4-a"},
		{1, principal, "h-p4-b"},
		{1, other, "h-p5"},
	} {
		if _, err := keys.CreateRegionKey(ctx, spec.region, "k", spec.hash, nil, spec.by, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("CreateRegionKey %s: %v", spec.hash, err)
		}
	}

	for _, tc := range []struct {
		name string
		by   apikey.Actor
		want []string
	}{
		// The CLI case is the one a bare "created_by_id = ?" silently
		// returns nothing for.
		{"cli", cli, []string{"h-cli"}},
		{"operator", operator, []string{"h-op"}},
		{"principal 4 across regions", principal, []string{"h-p4-b", "h-p4-a"}},
		{"principal 5", other, []string{"h-p5"}},
		{"unknown principal", apikey.Actor{Kind: apikey.ActorPrincipal, ID: 99}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keys.ListRegionKeysByCreator(ctx, tc.by)
			if err != nil {
				t.Fatalf("ListRegionKeysByCreator: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d keys, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, hash := range tc.want {
				if got[i].KeyHash != hash {
					t.Errorf("key %d hash = %q, want %q", i, got[i].KeyHash, hash)
				}
			}
		})
	}
}

func testAPIKeyRevokeByCreator(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	principal := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	cli := apikey.Actor{Kind: apikey.ActorCLI}
	a, err := keys.CreateRegionKey(ctx, 0, "a", "h-a", nil, principal, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	b, err := keys.CreateRegionKey(ctx, 1, "b", "h-b", nil, principal, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	survivor, err := keys.CreateRegionKey(ctx, 1, "c", "h-c", nil, cli, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// Already revoked: it must not appear in the returned ids, so the
	// operator's "these are the keys I just killed" list is accurate.
	if revokeErr := keys.RevokeRegionKey(ctx, 0, a.ID, cli, base.Add(time.Hour)); revokeErr != nil {
		t.Fatalf("pre-revoke: %v", revokeErr)
	}

	at := base.Add(2 * time.Hour)
	ids, err := keys.RevokeRegionKeysByCreator(ctx, principal, cli, at)
	if err != nil {
		t.Fatalf("RevokeRegionKeysByCreator: %v", err)
	}
	if len(ids) != 1 || ids[0] != b.ID {
		t.Fatalf("ids = %v, want [%d]", ids, b.ID)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "h-b"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("h-b: err = %v, want ErrRevoked", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, survivor.KeyHash); err != nil {
		t.Errorf("a key minted by a different actor must survive: %v", err)
	}
}

func testAPIKeyTouch(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	k, err := keys.CreateRegionKey(ctx, 1, "k", "h", nil, apikey.Actor{Kind: apikey.ActorCLI}, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	at := base.Add(90 * time.Minute)
	if touchErr := keys.TouchRegionKey(ctx, k.ID, at); touchErr != nil {
		t.Fatalf("TouchRegionKey: %v", touchErr)
	}
	got, err := keys.GetRegionKeyByHash(ctx, "h")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, at)
	}
	// Touching a row that is gone must not be an error: the touch is
	// best-effort and races a concurrent revoke.
	if err := keys.TouchRegionKey(ctx, 99999, at); err != nil {
		t.Errorf("touch of an unknown id: err = %v, want nil", err)
	}
}

func testAPIKeyListOrder(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	for i, hash := range []string{"h1", "h2", "h3"} {
		if _, err := keys.CreateRegionKey(ctx, 1, hash, hash, nil, cli, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("CreateRegionKey: %v", err)
		}
	}
	if _, err := keys.CreateRegionKey(ctx, 0, "other", "h-other", nil, cli, base); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	got, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	want := []string{"h3", "h2", "h1"}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d (another region's key must not appear)", len(got), len(want))
	}
	for i := range want {
		if got[i].KeyHash != want[i] {
			t.Errorf("key %d = %q, want %q (newest first)", i, got[i].KeyHash, want[i])
		}
	}
}

func testAPIKeyCascade(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	if _, err := keys.CreateRegionKey(ctx, 1, "k", "h", nil, apikey.Actor{Kind: apikey.ActorCLI}, base); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// regions.Repository has no Delete -- the sidecar never removes a region
	// -- so the cascade is asserted through the raw deleter the adapter
	// exposes for exactly this test. See RegionDeleter below.
	deleter, ok := regionRepo.(RegionDeleter)
	if !ok {
		t.Skip("this adapter does not expose DeleteRegionForTest")
	}
	if err := deleter.DeleteRegionForTest(ctx, 1); err != nil {
		t.Fatalf("DeleteRegionForTest: %v", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "h"); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("after the region is deleted: err = %v, want ErrNotFound", err)
	}
}

func testPrincipalLifecycle(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, _ := newStore(t)
	ctx := context.Background()

	p, err := keys.CreatePrincipal(ctx, "rails", "ph", base)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if p.ID == 0 || p.Name != "rails" || !p.CreatedAt.Equal(base) {
		t.Errorf("created = %+v", p)
	}
	if _, getErr := keys.GetPrincipalByHash(ctx, "ph"); getErr != nil {
		t.Fatalf("GetPrincipalByHash: %v", getErr)
	}
	if _, getErr := keys.GetPrincipalByHash(ctx, "nope"); !errors.Is(getErr, apikey.ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", getErr)
	}

	at := base.Add(time.Hour)
	if touchErr := keys.TouchPrincipal(ctx, p.ID, at); touchErr != nil {
		t.Fatalf("TouchPrincipal: %v", touchErr)
	}
	list, err := keys.ListPrincipals(ctx)
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(list) != 1 || list[0].LastUsedAt == nil || !list[0].LastUsedAt.Equal(at) {
		t.Fatalf("ListPrincipals = %+v", list)
	}

	if err := keys.RevokePrincipal(ctx, p.ID, at); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := keys.GetPrincipalByHash(ctx, "ph"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("revoked principal: err = %v, want ErrRevoked", err)
	}
	if err := keys.RevokePrincipal(ctx, p.ID, at.Add(time.Hour)); err != nil {
		t.Errorf("second revoke: err = %v, want nil (no-op success)", err)
	}
	if err := keys.RevokePrincipal(ctx, 99999, at); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown principal: err = %v, want ErrNotFound", err)
	}
}

// RegionDeleter is the escape hatch testAPIKeyCascade needs: regions are
// never deleted through regions.Repository (a directory sync only upserts),
// so an adapter opts into the cascade assertion by implementing this.
type RegionDeleter interface {
	DeleteRegionForTest(ctx context.Context, id int64) error
}

// testAPIKeyScopes pins that scopes survive every read path -- Create's
// return, GetByHash, ListRegionKeys, ListRegionKeysByCreator -- and that a
// key minted with nil scopes reads back as an empty, non-nil set. A key
// that silently lost its push scope would 403 at send time in another
// process, which is the failure the migration design spec section 2.2
// calls out.
func testAPIKeyScopes(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()
	by := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}

	push, err := keys.CreateRegionKey(ctx, 1, "push", "hash-push", apikey.Scopes{apikey.ScopePush}, by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey(push): %v", err)
	}
	plain, err := keys.CreateRegionKey(ctx, 1, "plain", "hash-plain", nil, by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey(plain): %v", err)
	}
	if !push.Scopes.Has(apikey.ScopePush) {
		t.Errorf("created push key scopes = %v, want push", push.Scopes)
	}
	if plain.Scopes == nil || len(plain.Scopes) != 0 {
		t.Errorf("created plain key scopes = %#v, want empty non-nil", plain.Scopes)
	}

	got, err := keys.GetRegionKeyByHash(ctx, "hash-push")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if !got.Scopes.Has(apikey.ScopePush) {
		t.Errorf("GetRegionKeyByHash scopes = %v, want push", got.Scopes)
	}

	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	var sawPush, sawPlain bool
	for _, k := range list {
		switch k.ID {
		case push.ID:
			sawPush = k.Scopes.Has(apikey.ScopePush)
		case plain.ID:
			sawPlain = k.Scopes != nil && len(k.Scopes) == 0
		}
	}
	if !sawPush || !sawPlain {
		t.Errorf("ListRegionKeys lost scopes: %+v", list)
	}

	byCreator, err := keys.ListRegionKeysByCreator(ctx, by)
	if err != nil {
		t.Fatalf("ListRegionKeysByCreator: %v", err)
	}
	for _, k := range byCreator {
		if k.ID == push.ID && !k.Scopes.Has(apikey.ScopePush) {
			t.Errorf("ListRegionKeysByCreator lost the push scope: %+v", k)
		}
	}
}
