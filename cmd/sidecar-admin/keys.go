package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// formatKeyInstant renders t as RFC 3339 UTC. Region key and service
// principal timestamps are audit/bookkeeping data read next to server logs
// -- not rider-facing scheduling tied to a single region's zone the way
// alert list's formatInZone is -- and `key list --minted-by-principal` can
// return keys spanning several regions with different zones, so there is no
// single zone to render alongside UTC here.
func formatKeyInstant(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// formatActor renders an apikey.Actor for a table cell: "cli" for the CLI
// actor (which carries no id), or "kind:id" otherwise.
func formatActor(a apikey.Actor) string {
	if a.Kind == apikey.ActorCLI {
		return apikey.ActorCLI
	}
	return fmt.Sprintf("%s:%d", a.Kind, a.ID)
}

// ---------------------------------------------------------------------------
// key
// ---------------------------------------------------------------------------

// runKey dispatches `sidecar-admin key`'s subcommands.
func runKey(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("key requires a subcommand: create, list, revoke")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "create":
		return keyCreate(ctx, stdout, store, now, cmdArgs)
	case "list":
		return keyList(ctx, stdout, store, cmdArgs)
	case "revoke":
		return keyRevoke(ctx, stdout, store, now, cmdArgs)
	default:
		return fmt.Errorf("unknown key subcommand %q; expected create, list, or revoke", cmd)
	}
}

// keyCreate is the operator's own bootstrap path for minting the first
// region key by hand -- distinct from principalCreate's deployment-wide
// credential. The raw key is printed here, on its own line, and stored only
// as its hash: this is the only place an operator ever sees it.
func keyCreate(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("key create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required; 0 is a real region, Tampa Bay)")
	name := fs.String("name", "", "a label for this key (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	var missing []string
	if !seen["region"] {
		missing = append(missing, "--region")
	}
	if !seen["name"] {
		missing = append(missing, "--name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("key create requires %s", strings.Join(missing, ", "))
	}

	// Regions come from OBACloud's directory export (region sync); minting a
	// key for an id the directory has never published would create a
	// credential nothing can ever use. That is rejected here, before any
	// write, rather than left as an orphan row.
	if _, err := store.Regions().Get(ctx, *regionID); err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			return fmt.Errorf("key create: region %d: %w", *regionID, regions.ErrNotFound)
		}
		return fmt.Errorf("key create: %w", err)
	}

	raw, hash, err := apikey.NewRegionKey(*regionID)
	if err != nil {
		return fmt.Errorf("key create: %w", err)
	}
	created, err := store.APIKeys().CreateRegionKey(ctx, *regionID, *name, hash, apikey.Actor{Kind: apikey.ActorCLI}, now)
	if err != nil {
		return fmt.Errorf("key create: %w", err)
	}

	fmt.Fprintln(stdout, raw)
	fmt.Fprintf(stdout, "id: %d\tname: %s\n", created.ID, created.Name)
	return nil
}

// keyList requires exactly one of --region or --minted-by-principal: region
// 0 is a real region (Tampa Bay), so there is no zero value that could
// safely default to "all" the way alert list's --all does; and
// --minted-by-principal is the post-compromise triage query, which
// --region cannot answer because it crosses regions.
func keyList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("key list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "list this region's keys")
	principalID := fs.Int64("minted-by-principal", 0, "list every key this principal minted, across regions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)

	if seen["region"] && seen["minted-by-principal"] {
		return errors.New("key list: specify only one of --region or --minted-by-principal")
	}
	if !seen["region"] && !seen["minted-by-principal"] {
		return errors.New("key list: specify --region N or --minted-by-principal N")
	}

	var keys []apikey.RegionKey
	var err error
	if seen["region"] {
		keys, err = store.APIKeys().ListRegionKeys(ctx, *regionID)
	} else {
		keys, err = store.APIKeys().ListRegionKeysByCreator(ctx,
			apikey.Actor{Kind: apikey.ActorPrincipal, ID: *principalID})
	}
	if err != nil {
		return fmt.Errorf("key list: %w", err)
	}

	for _, k := range keys {
		lastUsed := "—"
		if k.LastUsedAt != nil {
			lastUsed = formatKeyInstant(*k.LastUsedAt)
		}
		revoked := "—"
		if k.RevokedAt != nil {
			revoked = formatKeyInstant(*k.RevokedAt)
		}
		revokedBy := "—"
		if k.RevokedBy != nil {
			revokedBy = formatActor(*k.RevokedBy)
		}
		// Names go through StripControlChars on the way out: the admin API
		// already strips on the way in (§5.6), but a row written before that
		// guard existed -- or by a future path that writes through the
		// repository directly, as this CLI's own `key create` does -- must
		// not repaint the terminal of the operator investigating a
		// compromise.
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, regions.StripControlChars(k.Name), formatActor(k.CreatedBy),
			formatKeyInstant(k.CreatedAt), lastUsed, revoked, revokedBy)
	}
	return nil
}

// keyRevoke is region-scoped: --region and --id must agree, or it is an
// error rather than a revoke of somebody else's key. RevokeRegionKey already
// enforces this at the repository layer; the id-in-another-region case
// surfaces as apikey.ErrNotFound, same as a wholly unknown id.
func keyRevoke(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("key revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required)")
	id := fs.Int64("id", 0, "key id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	var missing []string
	if !seen["region"] {
		missing = append(missing, "--region")
	}
	if !seen["id"] {
		missing = append(missing, "--id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("key revoke requires %s", strings.Join(missing, ", "))
	}

	if err := store.APIKeys().RevokeRegionKey(ctx, *regionID, *id, apikey.Actor{Kind: apikey.ActorCLI}, now); err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			return fmt.Errorf("key revoke: key %d not found in region %d", *id, *regionID)
		}
		return fmt.Errorf("key revoke: %w", err)
	}

	fmt.Fprintf(stdout, "revoked key %d\n", *id)
	return nil
}

// ---------------------------------------------------------------------------
// principal
// ---------------------------------------------------------------------------

// runPrincipal dispatches `sidecar-admin principal`'s subcommands.
func runPrincipal(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("principal requires a subcommand: create, list, revoke")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "create":
		return principalCreate(ctx, stdout, store, now, cmdArgs)
	case "list":
		return principalList(ctx, stdout, store, cmdArgs)
	case "revoke":
		return principalRevoke(ctx, stdout, store, now, cmdArgs)
	default:
		return fmt.Errorf("unknown principal subcommand %q; expected create, list, or revoke", cmd)
	}
}

// principalCreate mints the deployment-wide credential OBACloud will hold.
// Like keyCreate, the raw key is printed once, on its own line, and stored
// only as its hash.
func principalCreate(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("principal create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "a label for this principal (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["name"] {
		return errors.New("principal create requires --name")
	}

	raw, hash, err := apikey.NewPrincipalKey()
	if err != nil {
		return fmt.Errorf("principal create: %w", err)
	}
	created, err := store.APIKeys().CreatePrincipal(ctx, *name, hash, now)
	if err != nil {
		return fmt.Errorf("principal create: %w", err)
	}

	fmt.Fprintln(stdout, raw)
	fmt.Fprintf(stdout, "id: %d\tname: %s\n", created.ID, created.Name)
	return nil
}

// principalList prints one line per service principal: id, name, created,
// last used, revoked.
func principalList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("principal list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	list, err := store.APIKeys().ListPrincipals(ctx)
	if err != nil {
		return fmt.Errorf("principal list: %w", err)
	}
	for _, p := range list {
		lastUsed := "—"
		if p.LastUsedAt != nil {
			lastUsed = formatKeyInstant(*p.LastUsedAt)
		}
		revoked := "—"
		if p.RevokedAt != nil {
			revoked = formatKeyInstant(*p.RevokedAt)
		}
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\n",
			p.ID, regions.StripControlChars(p.Name), formatKeyInstant(p.CreatedAt), lastUsed, revoked)
	}
	return nil
}

// principalRevoke revokes a service principal and, by default, every live
// region key it minted (design spec section 2.2): after a leak, the
// attacker mints keys with the same credential Rails uses, so the
// legitimate keys and the attacker's cannot be told apart. The recovery
// path is to kill them all and re-provision. --keep-keys opts out, for a
// planned rotation of a principal whose existing keys are known good.
//
// RevokeRegionKeysByCreator runs BEFORE RevokePrincipal, deliberately. They
// are two separate statements, not one transaction, so a process that dies
// between them (a crash, a kill -9) leaves the system in one of two states:
// key sweep first leaves a live principal with dead keys, which is
// re-runnable -- revoking an already-revoked key is a no-op success, so
// running this command again finishes the job. The reverse order would
// leave a dead principal with live keys, which is effectively unrecoverable
// through this command: the principal already reads as "revoked", the
// natural signal that cleanup is complete, so nothing prompts a retry that
// would sweep the keys that are actually still live.
func principalRevoke(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("principal revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.Int64("id", 0, "principal id (required)")
	keepKeys := fs.Bool("keep-keys", false,
		"leave this principal's region keys live; for a planned rotation of a principal whose keys are known good")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["id"] {
		return errors.New("principal revoke requires --id")
	}

	if !*keepKeys {
		revokedIDs, err := store.APIKeys().RevokeRegionKeysByCreator(ctx,
			apikey.Actor{Kind: apikey.ActorPrincipal, ID: *id}, apikey.Actor{Kind: apikey.ActorCLI}, now)
		if err != nil {
			return fmt.Errorf("principal revoke: %w", err)
		}
		if len(revokedIDs) > 0 {
			ids := make([]string, len(revokedIDs))
			for i, kid := range revokedIDs {
				ids[i] = strconv.FormatInt(kid, 10)
			}
			// Printed so the operator can reconcile these ids against
			// whatever the consumer still holds.
			fmt.Fprintf(stdout, "revoked keys: %s\n", strings.Join(ids, ", "))
		}
	}

	if err := store.APIKeys().RevokePrincipal(ctx, *id, now); err != nil {
		if errors.Is(err, apikey.ErrNotFound) {
			return fmt.Errorf("principal revoke: principal %d not found", *id)
		}
		return fmt.Errorf("principal revoke: %w", err)
	}

	fmt.Fprintf(stdout, "revoked principal %d\n", *id)
	return nil
}
