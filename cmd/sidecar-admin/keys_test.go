package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// keyFixture is a migrated database plus the two seeded regions, and the
// path the CLI's --db flag points at. Region 0 is deliberately included:
// it is a real region (Tampa Bay), and several of these tests exist to
// prove --region 0 works rather than being mistaken for "no region given".
func keyFixture(t *testing.T) (path string, store *sqlite.Store) {
	t.Helper()
	path, store = sqlitetest.OpenAt(t)
	err := store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 0, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", Language: "en", Active: true},
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Language: "en", Active: true},
	}, time.Now())
	if err != nil {
		t.Fatalf("seed regions: %v", err)
	}
	return path, store
}

// findKeyInOutput scans out for the single whitespace-delimited token
// beginning obask_ or obasp_ and fails the test unless there is exactly
// one -- the raw key `key create`/`principal create` print exactly once.
func findKeyInOutput(t *testing.T, out string) string {
	t.Helper()
	var found []string
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, "obask_") || strings.HasPrefix(field, "obasp_") {
			found = append(found, field)
		}
	}
	if len(found) != 1 {
		t.Fatalf("output contains %d obask_/obasp_ tokens, want exactly 1:\n%s", len(found), out)
	}
	return found[0]
}

// TestKeyCreate_PrintsTheRawKeyOnce. The CLI is the only place an operator
// ever sees a key: it is printed here and nowhere else, and created_by is
// cli, which is what `key list --minted-by-principal` later excludes.
func TestKeyCreate_PrintsTheRawKeyOnce(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	var stdout bytes.Buffer
	err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "1", "--name", "obacloud"})
	if err != nil {
		t.Fatalf("key create: %v", err)
	}

	raw := findKeyInOutput(t, stdout.String()) // scans for the obask_ token
	if !strings.HasPrefix(raw, "obask_1_") {
		t.Fatalf("printed key = %q, want an obask_1_ prefix", raw)
	}
	stored, err := store.APIKeys().GetRegionKeyByHash(context.Background(), apikey.Hash(raw))
	if err != nil {
		t.Fatalf("the printed key does not resolve: %v", err)
	}
	if stored.RegionID != 1 || stored.Name != "obacloud" {
		t.Errorf("stored = %+v, want region 1 / obacloud", stored)
	}
	if stored.CreatedBy.Kind != apikey.ActorCLI || stored.CreatedBy.ID != 0 {
		t.Errorf("CreatedBy = %+v, want the cli actor with no id", stored.CreatedBy)
	}
	// The hash is never printed: an operator pasting this output into a
	// ticket must not paste the lookup key along with it.
	if strings.Contains(stdout.String(), stored.KeyHash) {
		t.Errorf("output contains the stored hash:\n%s", stdout.String())
	}
}

// TestKeyCreate_RegionZeroWorks proves --region 0 (Tampa Bay) is accepted:
// region 0 is a real region, so a required-flag check that mistakenly tests
// the flag's zero value instead of visitedFlags would reject it.
func TestKeyCreate_RegionZeroWorks(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	var stdout bytes.Buffer
	err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "0", "--name", "obacloud"})
	if err != nil {
		t.Fatalf("key create --region 0: %v", err)
	}

	raw := findKeyInOutput(t, stdout.String())
	if !strings.HasPrefix(raw, "obask_0_") {
		t.Fatalf("printed key = %q, want an obask_0_ prefix", raw)
	}
	stored, err := store.APIKeys().GetRegionKeyByHash(context.Background(), apikey.Hash(raw))
	if err != nil {
		t.Fatalf("the printed key does not resolve: %v", err)
	}
	if stored.RegionID != 0 {
		t.Errorf("stored.RegionID = %d, want 0", stored.RegionID)
	}
}

// TestKeyCreate_RequiresAKnownRegion. Regions come from OBACloud's directory
// export, so minting for an id the directory has never published must fail
// loudly rather than create a key nothing can ever use.
func TestKeyCreate_RequiresAKnownRegion(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	err := run(strings.NewReader(""), io.Discard, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "99", "--name", "x"})
	if err == nil {
		t.Fatal("key create for an unknown region: err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("err = %v, want it to name the region", err)
	}
	list, listErr := store.APIKeys().ListRegionKeys(context.Background(), 99)
	if listErr != nil {
		t.Fatalf("ListRegionKeys: %v", listErr)
	}
	if len(list) != 0 {
		t.Errorf("a failed create left %d rows behind", len(list))
	}
}

// TestPrincipalRevoke_AlsoRevokesItsKeysByDefault is the recovery path.
// After a principal leaks, the keys it minted cannot be told apart from the
// legitimate ones -- the attacker mints with the same credential Rails uses
// -- so the default is to kill them all and re-provision (design spec
// section 2.2).
func TestPrincipalRevoke_AlsoRevokesItsKeysByDefault(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	now := time.Now()
	p, err := store.APIKeys().CreatePrincipal(ctx, "rails", "ph", now)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	minted := apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.ID}
	byPrincipal, err := store.APIKeys().CreateRegionKey(ctx, 0, "a", "h-a", minted, now)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	byCLI, err := store.APIKeys().CreateRegionKey(ctx, 1, "b", "h-b", apikey.Actor{Kind: apikey.ActorCLI}, now)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "principal", "revoke", "--id", strconv.FormatInt(p.ID, 10)}); err != nil {
		t.Fatalf("principal revoke: %v", err)
	}
	// The ids are printed so the operator can reconcile them against
	// whatever the consumer still holds.
	if !strings.Contains(stdout.String(), strconv.FormatInt(byPrincipal.ID, 10)) {
		t.Errorf("output does not name the revoked key %d:\n%s", byPrincipal.ID, stdout.String())
	}
	if _, err := store.APIKeys().GetRegionKeyByHash(ctx, "h-a"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("the principal's key: err = %v, want ErrRevoked", err)
	}
	if _, err := store.APIKeys().GetRegionKeyByHash(ctx, byCLI.KeyHash); err != nil {
		t.Errorf("a key minted by the CLI must survive: %v", err)
	}
	if _, err := store.APIKeys().GetPrincipalByHash(ctx, "ph"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("the principal: err = %v, want ErrRevoked", err)
	}
}

// TestKeyList_GuardsNamesForTheTerminal. Defence in depth: the API already
// strips control characters on the way in, but a row written before that
// guard existed -- or by a future path -- must not repaint the terminal of
// the operator investigating a compromise. The name goes in through the
// repository, bypassing the API's guard, which is the case this covers.
func TestKeyList_GuardsNamesForTheTerminal(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	if _, err := store.APIKeys().CreateRegionKey(context.Background(), 1,
		"ob\x1b[2Jacloud", "h", apikey.Actor{Kind: apikey.ActorCLI}, time.Now()); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "list", "--region", "1"}); err != nil {
		t.Fatalf("key list: %v", err)
	}
	if strings.ContainsRune(stdout.String(), 0x1b) {
		t.Errorf("output carries a raw escape byte:\n%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ob[2Jacloud") {
		t.Errorf("output = %q, want the name with its escape byte stripped", stdout.String())
	}
}

// TestKeyList_MintedByPrincipalCrossesRegions is the post-compromise triage
// query: one principal's keys, across every region, which --region cannot
// answer.
func TestKeyList_MintedByPrincipalCrossesRegions(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	now := time.Now()

	p1, err := store.APIKeys().CreatePrincipal(ctx, "p1", "p1-hash", now)
	if err != nil {
		t.Fatalf("CreatePrincipal p1: %v", err)
	}
	p2, err := store.APIKeys().CreatePrincipal(ctx, "p2", "p2-hash", now)
	if err != nil {
		t.Fatalf("CreatePrincipal p2: %v", err)
	}
	p1Actor := apikey.Actor{Kind: apikey.ActorPrincipal, ID: p1.ID}
	p2Actor := apikey.Actor{Kind: apikey.ActorPrincipal, ID: p2.ID}

	// p1 mints in BOTH region 0 and region 1.
	inRegion0, err := store.APIKeys().CreateRegionKey(ctx, 0, "p1-in-0", "h-p1-0", p1Actor, now)
	if err != nil {
		t.Fatalf("CreateRegionKey p1/region0: %v", err)
	}
	inRegion1, err := store.APIKeys().CreateRegionKey(ctx, 1, "p1-in-1", "h-p1-1", p1Actor, now)
	if err != nil {
		t.Fatalf("CreateRegionKey p1/region1: %v", err)
	}
	// A CLI-minted key and a key minted by a different principal must NOT
	// appear in p1's listing.
	cliKey, err := store.APIKeys().CreateRegionKey(ctx, 1, "cli-key", "h-cli", apikey.Actor{Kind: apikey.ActorCLI}, now)
	if err != nil {
		t.Fatalf("CreateRegionKey cli: %v", err)
	}
	otherKey, err := store.APIKeys().CreateRegionKey(ctx, 0, "p2-key", "h-p2", p2Actor, now)
	if err != nil {
		t.Fatalf("CreateRegionKey p2: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "list", "--minted-by-principal", strconv.FormatInt(p1.ID, 10)}); err != nil {
		t.Fatalf("key list --minted-by-principal: %v", err)
	}
	out := stdout.String()

	for _, want := range []struct {
		id   int64
		name string
	}{
		{inRegion0.ID, "p1-in-0"},
		{inRegion1.ID, "p1-in-1"},
	} {
		if !strings.Contains(out, want.name) {
			t.Errorf("output missing p1's key %q:\n%s", want.name, out)
		}
	}
	for _, unwanted := range []struct {
		id   int64
		name string
	}{
		{cliKey.ID, "cli-key"},
		{otherKey.ID, "p2-key"},
	} {
		if strings.Contains(out, unwanted.name) {
			t.Errorf("output should not contain %q (minted by someone else):\n%s", unwanted.name, out)
		}
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("key list --minted-by-principal produced %d lines, want 2 (p1's two keys):\n%s", len(lines), out)
	}
}

// TestKeyList_ShowsHistoryAndCreators. Revoked rows are kept, so the list
// shows who minted each key and who revoked it -- the audit trail the
// recovery path depends on.
func TestKeyList_ShowsHistoryAndCreators(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	now := time.Now()

	p, err := store.APIKeys().CreatePrincipal(ctx, "rails", "ph2", now)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if _, createErr := store.APIKeys().CreateRegionKey(ctx, 1, "live-key", "h-live",
		apikey.Actor{Kind: apikey.ActorCLI}, now); createErr != nil {
		t.Fatalf("CreateRegionKey live: %v", createErr)
	}
	toRevoke, err := store.APIKeys().CreateRegionKey(ctx, 1, "revoked-key", "h-revoked",
		apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.ID}, now)
	if err != nil {
		t.Fatalf("CreateRegionKey toRevoke: %v", err)
	}
	revokeTime := now.Add(time.Hour)
	if err := store.APIKeys().RevokeRegionKey(ctx, 1, toRevoke.ID, apikey.Actor{Kind: apikey.ActorCLI}, revokeTime); err != nil {
		t.Fatalf("RevokeRegionKey: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "list", "--region", "1"}); err != nil {
		t.Fatalf("key list: %v", err)
	}
	out := stdout.String()

	if !strings.Contains(out, "live-key") {
		t.Errorf("output missing the live key:\n%s", out)
	}
	if !strings.Contains(out, "revoked-key") {
		t.Errorf("output missing the revoked key:\n%s", out)
	}
	if !strings.Contains(out, "principal:"+strconv.FormatInt(p.ID, 10)) {
		t.Errorf("output missing the revoked key's creator (principal %d):\n%s", p.ID, out)
	}
	if !strings.Contains(out, revokeTime.UTC().Format(time.RFC3339)) {
		t.Errorf("output missing the revocation timestamp %s:\n%s", revokeTime.UTC().Format(time.RFC3339), out)
	}
	// The revoker (cli) must appear on the revoked row specifically, not
	// merely somewhere in the output -- the live row's own creator column
	// would also contain the literal string "cli" and pass a bare
	// strings.Contains(out, "cli") check regardless of whether the revoker
	// column is wired up at all.
	var revokedLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "revoked-key") {
			revokedLine = line
		}
	}
	if revokedLine == "" {
		t.Fatalf("could not find the revoked-key row in output:\n%s", out)
	}
	fields := strings.Split(revokedLine, "\t")
	if len(fields) != 7 {
		t.Fatalf("revoked-key row = %q, want 7 tab-separated fields", revokedLine)
	}
	if fields[6] != apikey.ActorCLI {
		t.Errorf("revoked-key row's revoked-by field = %q, want %q", fields[6], apikey.ActorCLI)
	}
}

// TestKeyRevoke_IsRegionScoped: --region and --id must agree, or it is an
// error rather than a revoke of somebody else's key.
func TestKeyRevoke_IsRegionScoped(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	now := time.Now()
	key, err := store.APIKeys().CreateRegionKey(ctx, 1, "mine", "h-scoped", apikey.Actor{Kind: apikey.ActorCLI}, now)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	// Wrong region: rejected, and the key must remain live.
	_, _, err = cli(t, path, "key", "revoke", "--region", "0", "--id", strconv.FormatInt(key.ID, 10))
	if err == nil {
		t.Fatal("key revoke --region 0 for a key minted in region 1: want error, got nil")
	}
	if !strings.Contains(err.Error(), strconv.FormatInt(key.ID, 10)) || !strings.Contains(err.Error(), "0") {
		t.Errorf("err = %v, want it to name the key id and the (wrong) region", err)
	}
	if _, getErr := store.APIKeys().GetRegionKeyByHash(ctx, "h-scoped"); getErr != nil {
		t.Errorf("key after a region-mismatched revoke attempt: err = %v, want it still live", getErr)
	}

	// Right region: succeeds.
	if _, _, err := cli(t, path, "key", "revoke", "--region", "1", "--id", strconv.FormatInt(key.ID, 10)); err != nil {
		t.Fatalf("key revoke --region 1: %v", err)
	}
	if _, getErr := store.APIKeys().GetRegionKeyByHash(ctx, "h-scoped"); !errors.Is(getErr, apikey.ErrRevoked) {
		t.Errorf("key after a correct revoke: err = %v, want ErrRevoked", getErr)
	}

	// A second revoke of an already-revoked key is a no-op success, not an
	// error.
	if _, _, err := cli(t, path, "key", "revoke", "--region", "1", "--id", strconv.FormatInt(key.ID, 10)); err != nil {
		t.Errorf("second key revoke of an already-revoked key: %v, want a no-op success", err)
	}
}

// TestPrincipalRevoke_KeepKeys opts out, for the planned rotation of a
// principal whose keys are known good.
func TestPrincipalRevoke_KeepKeys(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	now := time.Now()
	p, err := store.APIKeys().CreatePrincipal(ctx, "rails", "ph3", now)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	key, err := store.APIKeys().CreateRegionKey(ctx, 0, "kept", "h-kept",
		apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.ID}, now)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	if _, _, err := cli(t, path, "principal", "revoke", "--id", strconv.FormatInt(p.ID, 10), "--keep-keys"); err != nil {
		t.Fatalf("principal revoke --keep-keys: %v", err)
	}

	if _, getErr := store.APIKeys().GetPrincipalByHash(ctx, "ph3"); !errors.Is(getErr, apikey.ErrRevoked) {
		t.Errorf("the principal: err = %v, want ErrRevoked", getErr)
	}
	if _, getErr := store.APIKeys().GetRegionKeyByHash(ctx, "h-kept"); getErr != nil {
		t.Errorf("--keep-keys: key %d: err = %v, want it still live", key.ID, getErr)
	}
}

// TestKeyAndPrincipal_FlagErrors: missing --region, missing --name, a
// non-integer --id, both --region and --minted-by-principal on `key list`,
// neither of them, and an unknown subcommand each return a non-nil error
// naming the flag, and write nothing to the database.
func TestKeyAndPrincipal_FlagErrors(t *testing.T) {
	t.Parallel()
	path, store := keyFixture(t)

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"key create missing --region", []string{"key", "create", "--name", "x"}, "--region"},
		{"key create missing --name", []string{"key", "create", "--region", "1"}, "--name"},
		{"key revoke missing --region", []string{"key", "revoke", "--id", "1"}, "--region"},
		{"key revoke missing --id", []string{"key", "revoke", "--region", "1"}, "--id"},
		{"key revoke non-integer --id", []string{"key", "revoke", "--region", "1", "--id", "not-a-number"},
			"invalid value \"not-a-number\""},
		{"key list both selectors", []string{"key", "list", "--region", "1", "--minted-by-principal", "1"},
			"only one"},
		{"key list neither selector", []string{"key", "list"}, "--region"},
		{"key unknown subcommand", []string{"key", "bogus"}, "bogus"},
		{"principal create missing --name", []string{"principal", "create"}, "--name"},
		{"principal revoke missing --id", []string{"principal", "revoke"}, "--id"},
		{"principal revoke non-integer --id", []string{"principal", "revoke", "--id", "abc"},
			"invalid value \"abc\""},
		{"principal unknown subcommand", []string{"principal", "bogus"}, "bogus"},
		{"key missing subcommand", []string{"key"}, "subcommand"},
		{"principal missing subcommand", []string{"principal"}, "subcommand"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := cli(t, path, tc.args...)
			if err == nil {
				t.Fatalf("%v: want error, got nil", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%v: err = %q, want it to contain %q", tc.args, err.Error(), tc.wantErr)
			}
		})
	}

	// None of the above should have written a region key or a principal.
	keys, err := store.APIKeys().ListRegionKeys(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("flag-error cases left %d region keys behind, want 0", len(keys))
	}
	principals, err := store.APIKeys().ListPrincipals(context.Background())
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(principals) != 0 {
		t.Errorf("flag-error cases left %d principals behind, want 0", len(principals))
	}
}
