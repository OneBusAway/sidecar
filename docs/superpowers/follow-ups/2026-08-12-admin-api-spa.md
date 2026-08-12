# Follow-ups from the admin API + SPA branch

Findings surfaced during review of `feature/admin-api-spa`, triaged by the final
whole-branch review as file-as-follow-up rather than merge blockers. Ordered by the
priority that review assigned.

## 1. Region timezone defaults to `'UTC'`, so the "never guess a zone" branch is dead code

**File this first.** It is the only item with rider-visible wrong-time consequences.

`internal/store/sqlite/migrations/00001_initial_schema.sql` defaults `regions.timezone`
to `'UTC'`, the directory sync never sets it, and `internal/httpapi/admin_regions.go`
refuses to store an empty one — so `region.timezone == ""` cannot occur through any
supported path. The zoneless raw-RFC-3339 UI branch in `AlertForm.svelte` only renders
for the no-region-chosen case.

The consequence is the inverse of the hazard the design defended against: an
unconfigured region shows a working `datetime-local` picker labelled `(UTC)`, so an
author typing `17:00` for Tampa Bay stores `17:00Z` — four hours off. The guess moved
from the client into the schema default.

Shipped mitigation (so the failure is warned, not silent): `AlertForm` warns whenever
the zone is `UTC`, and the regions screen labels it `UTC (default — not configured)`.

**Proper fix:** a nullable or explicitly-`configured` timezone — a migration plus API,
CLI, and SPA changes. Fold in `cmd/sidecar-admin/commands.go`'s `parseInstant`, which
still interpolates an empty timezone into `region %d is configured as %s`, producing
`region 16 is configured as :`; that message only becomes reachable once empty zones
are real.

## 2. Stale translations are not surfaced anywhere

`translationJSON` in `internal/httpapi/admin_alerts.go` drops `SourceSHA256`, which the
store does return. The SPA therefore cannot know which stored translations the rider
feed is withholding: after an English header is edited, the admin table lists the
Spanish translation exactly as before while the feed has already withheld it as stale.
The CLI does not surface staleness either, so this is systemic rather than an SPA gap.

Mitigation shipped: a standing prose note on the edit page.

**Fix:** pass the alert into `groupTranslations` and emit `header_stale` /
`description_stale` from `t.SourceSHA256 != alerts.SourceHash(current.HeaderText)`,
then surface it in both the SPA and `sidecar-admin alert show`.

## 3. `sidecar-admin user delete` last-user race, and `user passwd` atomicity

Two concurrent CLI invocations can both pass the last-user `--force` guard before
either deletes, leaving zero users. Recovery is one command (`user create` against an
empty users table is the documented bootstrap path), so exposure is minutes of
admin-UI downtime, not lockout.

Same follow-up should cover `userPasswd`'s non-atomic sequence — `UpdatePassword` →
`GetUserByUsername` → `DeleteUserSessions`. A crash between steps leaves
old-password sessions live for up to 30 days, contrary to the command's purpose.

**Fix:** wrap each in a single `BEGIN IMMEDIATE` transaction, which needs either new
`auth.Repository` methods or transaction plumbing through the interface — a change to
the Postgres-portability contract and the shared conformance suite. Resolve
`authRepo.db` (currently an unused field implying exactly this transactional path) as
part of it.

## 4. Smaller items

- `internal/auth/password.go` — `VerifyPassword` parses the PHC version with
  `fmt.Sscanf`, which accepts trailing garbage (`v=19xxx` parses as 19). Not
  exploitable; PHC strings only ever come from our own `HashPassword`. Fix: exact
  string compare against `"v=19"`.
- Header guards test `== ""`, not whitespace, so a header of `"   "` still passes all
  three surfaces. The neighbouring `agency_id` guard has the identical hole; fix both
  together, including a `.trim()` in the SPA's payload builders.
- `internal/store/sqlite/store_test.go` — `TestMigrateCreatesAuthTables` asserts the
  four timestamp columns are `INTEGER` but not the foreign key or the indexes. The
  FK's *behavior* is pinned engine-independently by the cascade conformance subtest,
  so only index existence is unasserted.
- `internal/store/storetest/authtest.go` — `assertInstant`'s location failure message
  renders as `location = UTC, want UTC` under `TZ=UTC` when the value carries
  `time.Local`: a real failure that reads like a test bug.
- `web/admin/src` — the hand-rolled `$app/paths` stub is copy-pasted across five test
  files and one copy has `resolve('/') -> '/admin'` where the real value is
  `'/admin/'`. Harmless today (the guard never calls it), but extract one shared,
  correct stub so the invented-fixture failure mode has a single place to be right.
- No CI workflow exists yet. Whoever adds one must route it through `make check`, or
  run `make web` before any `go test` step: the Go suite includes an embed regression
  test that needs a populated `internal/httpapi/adminui/dist/`.
