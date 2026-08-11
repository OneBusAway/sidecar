# OneBusAway Sidecar Services Specification

**Version 1.0 — August 2026**

This document, together with [`openapi.yaml`](openapi.yaml), specifies the OneBusAway
*sidecar services*: the region-scoped HTTP APIs that the OneBusAway mobile apps use for
features the core OneBusAway REST API server does not provide — service alerts, trip
departure alarms, iOS Live Activities, rider surveys, ghost bus reports, push
notification registration, weather, vehicle search, and donations.

The purpose of this spec is to let you **build your own sidecar server** that is fully
compatible with the OneBusAway iOS and Android apps, with **no dependency on OBACloud**
(OneBusAway's hosted implementation). Everything a conforming implementation must do is
described here in implementation-neutral terms: URL shapes, request/response contracts,
background behaviors, push-delivery semantics, and the reasons behind them.

- **What** each endpoint accepts and returns → [`openapi.yaml`](openapi.yaml) is
  normative (the outbound alarm and Live Activity push payloads are modeled there as
  OpenAPI `webhooks`).
- **How** the server must behave between requests (polling, firing alarms, updating Live
  Activities, reaping state) → this document is normative.
- **Why** the contracts look the way they do → explained inline, so you can make safe
  judgment calls where your implementation differs.

The key words MUST, SHOULD, and MAY are used as in RFC 2119.

---

## 1. Architecture and rationale

### 1.1 What a sidecar is

A OneBusAway deployment has three server-side pieces:

1. **The OneBusAway REST API server** (`onebusaway-application-modules`): serves static
   GTFS and real-time arrivals. Stateless with respect to riders — it knows nothing about
   individual devices.
2. **The regions directory** (`regions-v3.json`): a JSON document the apps download that
   lists every known region and its service URLs.
3. **The sidecar** (this spec): everything stateful and rider-facing that the REST API
   server can't do — because it requires storing per-device state (alarms, push tokens,
   Live Activity subscriptions), calling external services (APNs, FCM, Stripe, a weather
   provider), or content authoring (service alerts, surveys).

The sidecar sits *beside* the OBA REST API server (hence the name). It is a **client** of
the OBA REST API — it polls arrivals to decide when alarms fire and what Live Activities
display — and a **peer** of the apps, which call it directly over HTTPS.

```
┌────────────┐   regions-v3.json    ┌──────────────────────┐
│ Mobile app │◄─────────────────────│  Regions directory   │
└─────┬──────┘  (sidecarBaseUrl,    └──────────────────────┘
      │          obaBaseUrl, id)
      │
      ├── arrivals, stops, trips ──►┌──────────────────────┐
      │                             │  OBA REST API server │
      │                             └──────────▲───────────┘
      │                                        │ arrival-and-departure
      │                                        │ polling
      └── alarms, alerts, surveys… ─►┌─────────┴───────────┐        ┌──────────┐
                                     │       Sidecar       │──────► │ APNs/FCM │
                                     │     (this spec)     │ push   └──────────┘
                                     └─────────────────────┘
```

### 1.2 Discovery: how apps find your sidecar

Apps discover the sidecar through the regions directory. Each region entry carries:

| Field            | Meaning                                                        |
|------------------|----------------------------------------------------------------|
| `id`             | Integer region identifier. This is the `{regionId}` in every sidecar path. |
| `sidecarBaseUrl` | HTTPS base URL of the sidecar serving this region.             |
| `obaBaseUrl`     | Base URL of the region's OBA REST API server.                  |

A single sidecar instance can serve many regions; the `{regionId}` path segment scopes
every request. Your implementation MUST treat the numeric region identifier as the
region's public primary key and MUST return `404 {"error": "Couldn't find Region"}` for
identifiers it does not serve.

> **Why an integer id?** The regions directory predates the sidecar by a decade and its
> integer `id` is baked into shipped apps. Apps added via deep link without a directory
> entry synthesize a random id, so unrecognized ids are a normal condition, not an attack.

### 1.3 Trust model

**All rider-facing sidecar endpoints are unauthenticated.** There are no API keys, no
sessions, and no CSRF tokens. This is deliberate:

- The audience is anonymous riders; forcing account creation would gut adoption of
  safety-relevant features (alarms, alerts).
- Nothing rider-facing exposes anyone else's data. Every mutable resource is addressed by
  an unguessable server-generated token (see §2.4), so possession of the URL is the
  ownership proof.
- Anonymous per-device identity, where needed (surveys, ghost bus reports), is a
  client-generated UUID (`user_identifier`) stored on the device.

The cost of this model is abuse surface, which conforming implementations MUST bound with
rate limiting (§2.6) and data-retention limits (§8).

---

## 2. Conventions

### 2.1 Base URL and versioning

All endpoints live under `/api/v1/` or `/api/v2/` on the `sidecarBaseUrl` host. The
version prefix is per-endpoint, not per-deployment: v1 and v2 coexist, and shipped app
versions call a fixed mix of both. A conforming sidecar MUST implement both versions of
the alarms API (§5) because old app builds only know v1.

### 2.2 Request encoding

Clients send parameters as query strings, `application/x-www-form-urlencoded` bodies, or
`application/json` bodies depending on app version and endpoint. Implementations MUST
accept both form-encoded and JSON bodies for every POST endpoint (Rails, which the
reference implementation uses, merges all three sources into one params bag; shipped
clients rely on that flexibility). The one exception is `POST /api/v1/payment_intents`,
whose body is always raw JSON.

### 2.3 Response encoding

Responses are JSON (`application/json`) unless noted. The two non-JSON endpoints are the
alerts feed (GTFS-realtime protocol buffer, §3) and empty-body responses (`204`, `404` on
DELETEs).

### 2.4 Resource tokens

Every alarm and Live Activity is publicly addressed by a **secure token**: an
unguessable, URL-safe random string (the reference implementation uses 22-character
URL-safe base64 encoding 128 random bits). Creation responses return a fully-qualified
`url` whose last path segment is this token; clients later `DELETE` that exact URL.
Survey responses and ghost bus reports use the same pattern with a hex
`public_identifier` (the reference uses 20 hex chars).

Clients treat the returned `url` as opaque, and one part of it is genuinely opaque: the
**region path segment in server-generated URLs is not guaranteed to be the bare
integer**. The reference emits an id-prefixed slug there (e.g.
`…/api/v2/regions/1-puget-sound/alarms/<token>`), and shipped apps DELETE that exact
URL. Servers MUST accept the bare integer region id in the path and MUST accept any
segment whose leading integer is the region id (parse the integer prefix; ignore the
rest).

Implementations MUST NOT expose sequential database ids in these URLs. Beyond the obvious
insecure-direct-object-reference risk, sequential ids leak row counts (how many alarms or
reports exist), which is competitive/operational information.

### 2.5 Error shapes

Three error shapes exist, for historical reasons; shipped clients parse all three, so
implementations MUST reproduce them where the OpenAPI file specifies them:

| Shape | Status | Used by |
|---|---|---|
| `{"error": "<message>"}` | 404 | Unknown region, unknown survey |
| `{"error": "<code or message>", "messages": ["…", …]}` | 422 | Alarms, Live Activities, push registrations, ghost bus reports |
| `{"errors": ["…", …]}` | 422 | Surveys and survey responses |

DELETE endpoints respond with bodyless `204` (deleted) or `404` (no such token).

> **Why 204 and not an empty 200:** shipped iOS builds contain a workaround that treats
> an empty-bodied `200` as a disguised `404`, so a successful alarm cancellation
> surfaced in the app as a failure. `204` is unambiguous to both broken and fixed
> builds. Do not "improve" this to `200`.

A deletion MUST only report `204` after the row is actually gone. If the delete fails,
return a `5xx` — the client treats the `204` as a binding "it's cancelled" signal, and a
false positive means a rider gets woken by an alarm they cancelled.

### 2.6 Rate limiting

Because the API is unauthenticated, conforming implementations MUST rate-limit the
abuse-prone write endpoints. The reference limits, which are known to be compatible with
real client behavior:

| Endpoint | Limit |
|---|---|
| `/api/v2/regions/{id}/push_registrations` (any method) | 30/minute per IP |
| `POST /api/v2/regions/{id}/ghost_bus_reports` | 10/hour per IP **and** 20/day per `user_identifier` |

Push registration is throttled because junk tokens inflate an agency's apparent push
reach and turn every alert send into a fan-out to garbage. (The reference throttle
matches on path alone, so opt-out DELETEs share the POST bucket.) Ghost bus reports are
throttled twice because report volume is used operationally and a hostile rider can
rotate IPs but usually keeps a device id. The per-`user_identifier` counter MUST read
the identifier from JSON bodies as well as query/form parameters — §2.2 requires
accepting JSON, and a counter that only sees form encoding is silently bypassed by
JSON-encoded submissions. Cap that body read (the reference caps at 8 KB): it runs
before anything has been rejected, so an unbounded read hands attackers a free memory
amplifier. An oversized body slipping through *uncounted* would itself be a bypass, so
the reference rejects JSON bodies declaring more than the cap outright (`403` — a
legitimate report is far smaller). Extract the identifier with the same precedence the
framework gives the controller (body first, then query string), so the throttle bucket
always matches the identifier the stored report is filed under.

### 2.7 The `apns_sandbox` parameter

Endpoints that register an APNs-routable token (`v2` alarms, Live Activities, push
registrations) accept an optional `apns_sandbox` parameter. A **debug build** of the iOS
app carries the development APNs entitlement, so every token it produces is only valid
against the APNs *sandbox* host; the build declares this at registration time, and the
flag MUST be persisted with the token because the actual push happens later, from a
background job.

Parsing is a strict allow-list, and this is normative:

- Truthy: `1`, `t`, `true`, `on` (case-insensitive, trimmed)
- Falsy: `0`, `f`, `false`, `off`, empty, absent
- **Anything else: falsy**, and SHOULD be logged.

Do not use a permissive boolean cast that treats unrecognized strings as true. The two
failure directions are asymmetric: a debug build wrongly routed to production fails in
front of a developer, while a production token wrongly routed to the sandbox bounces
with `BadDeviceToken` in front of a rider — and for Live Activities that bounce is
terminal (§6.5), silently destroying the subscription.

`apns_sandbox` is meaningless off iOS and MUST be ignored/cleared for Android tokens.

---

## 3. Service alerts feed

```
GET /api/v1/regions/{regionId}/alerts        → GTFS-realtime FeedMessage (binary protobuf)
GET /api/v1/regions/{regionId}/alerts.pbtext → the same message as protobuf JSON (debugging)
```

The sidecar is the *authoring and distribution* side of rider-facing service alerts
("Route 44 detoured this weekend"). How alerts get authored is out of scope — that's your
admin UI's problem. What is in scope is the feed contract:

- The response is a standard **GTFS-realtime `FeedMessage`** (`gtfs_realtime_version:
  "1.0"`) containing only `Alert` entities. Entity ids must be stable per alert across
  requests (the reference uses `Alert_<id>`).
- Only *published* alerts appear. If you support drafts, they must be invisible here.
- Alerts marked as **test alerts** are excluded unless the request carries `?test=1`
  (any non-blank value). This lets an agency verify end-to-end delivery on production
  infrastructure without showing riders the test.
- Order by start time descending; cap the feed (the reference caps at 20). The apps
  re-fetch frequently; this is a "current conditions" feed, not an archive.
- Each alert's `informed_entity` carries one `EntitySelector` with the `agency_id` the
  region's OBA server uses, so apps can match alerts to the agency.
- `active_period`: `start` is the alert's start time; if the author set no end time,
  advertise `start + 8 hours`. Apps hide alerts outside the active period, and an
  open-ended alert would otherwise pin itself to the top of riders' feeds forever.
- `cause`, `effect`, and `severity_level` are the standard GTFS-RT enums.

**Localization.** `header_text` and `description_text` are `TranslatedString`s: English
first, then any available translations. If your implementation stores translations,
withhold any translation that is *stale* — produced from an earlier version of the
English source text — so riders fall back to accurate English rather than reading
outdated information. `url` is English-only.

Alerts also drive **push notifications** to registered devices (§4); the feed and the
push audience are two views of the same authored content.

---

## 4. Push notification registrations

```
POST   /api/v2/regions/{regionId}/push_registrations
DELETE /api/v2/regions/{regionId}/push_registrations?token=…
```

Apps register their device push token on launch and whenever the rider grants
notification permission. This registry is the **audience for service-alert pushes** and
the honest measure of push "reach" — opted-in devices, not just riders who happen to have
created alarms.

**Registration is an upsert** keyed on `(region, token)`: re-registration rewrites
`operating_system` and `apns_sandbox` and refreshes a `last_seen_at` timestamp.
`locale`, `test_device`, and `description` are **sticky** — overwritten only when the
request carries an actual value (a JSON `null` counts as absent, not as false/blank),
because a routine launch-time re-POST that omits them must not drop a rider's stored
locale or silently demote an admin-marked test device. An explicit
`test_device=false` still demotes (and clears the description). `apns_sandbox` is
deliberately *not* sticky: each registration states its own build's APNs environment,
and absent means production (§2.7) — a debug build that becomes an App Store build
must demote to production routing. Success is a bodyless `204` — there is nothing to
hand back; the token itself is the handle.
Implementations MUST handle the concurrent-first-registration race (two upserts for the
same new token) by retrying rather than erroring.

Request fields:

| Field | Notes |
|---|---|
| `token` | APNs device token (iOS) or FCM registration token (Android). Required. Max length 4096. |
| `operating_system` | `ios` or `android`. Required. |
| `locale` | BCP-47 tag, e.g. `es-MX`. Optional; used to localize alert pushes. |
| `apns_sandbox` | §2.7. iOS only. |
| `test_device` | Boolean-ish. Marks the device as a test-push target for admins. |
| `description` | Human label ("Aaron's iPhone 17"). **Required if `test_device` is true**, cleared otherwise — a test device must be traceable to a human. |

**Locale normalization.** If you localize alert pushes, map the reported tag onto your
translation catalog at registration time: exact case-insensitive match, then known
aliases (`zh-CN`/`zh-SG`→`zh-Hans`, `zh-TW`/`zh-HK`→`zh-Hant`, `fil`→`tl`,
`pt-BR`→`pt`), then bare primary subtag (`es-MX`→`es`), else null (which means English
copy).

**Opt-out.** `DELETE` with the token (query or body parameter) removes the registration:
`204`, or `404` if unknown. Possession of the token is the ownership proof.

**Retention.** Registrations whose `last_seen_at` is older than **180 days** MUST be
reaped (the app refreshes the timestamp on every launch — and V2 alarm creation
refreshes it too, per §5.2 — so a silent row is a dead device). Additionally, terminal delivery failures reported by APNs (`Unregistered`,
`BadDeviceToken`, `DeviceTokenNotForTopic`) SHOULD delete the registration immediately
(§6.5), so reach counts stay honest.

**What gets pushed.** When an alert is sent as a push, fan out to the region's
registrations grouped by `(platform, locale, APNs environment)` — each group gets copy in
its language and routes to the right APNs host. Batch the sends and reconcile both the
synchronous provider response and asynchronous feedback (§6.5) into per-token accounting
if you want delivery stats; that accounting is internal and not part of this contract.

---

## 5. Alarms (departure notifications)

An alarm is a one-shot request: *"push me N seconds before this bus leaves this stop."*
The sidecar owns all the timing; the app can be killed, offline, or in a pocket. This is
the feature that most tightly couples the sidecar to the OBA REST API, because deciding
*when* to fire requires live arrival predictions.

### 5.1 The two API versions

```
V2 (current):   POST/DELETE /api/v2/regions/{regionId}/alarms[/{token}]
V1 (legacy):    POST/DELETE /api/v1/regions/{regionId}/alarms[/{token}]
```

Both create an alarm and return `201 {"url": "https://…/alarms/<token>"}`. The
differences are wire-level and behavioral:

| | V1 | V2 |
|---|---|---|
| `user_push_id` means | OneSignal player id (legacy push aggregator) | Raw APNs device token / FCM registration token |
| `apns_sandbox` | Not supported | Supported (§2.7) |
| OBA lookup fails at creation | Request fails (the reference lets the error escape as an unstructured `500`) | Alarm is created anyway with a generic message ("The bus leaves in N minutes") |
| Duplicate registration | Idempotent: re-POST of the same `(user_push_id, trip_id, stop_id, service_date)` returns the *existing* alarm's URL, without applying changed fields | Not deduplicated |

> **Why V1 dedupes:** legacy clients re-POST the same alarm aggressively — 11 identical
> registrations for one trip in 23 minutes was observed in production — and each
> duplicate would otherwise fire simultaneously later. V2 clients don't re-post, so V2
> skips the lookup. If you only ever serve current apps you MAY implement V1 as a thin
> alias of V2 with the dedupe kept.

### 5.2 Creation contract

Request fields (both versions): `user_push_id`, `stop_id`, `trip_id`, `service_date`
(epoch **milliseconds**, as used throughout the OBA REST API), `vehicle_id` (optional),
`stop_sequence`, `seconds_before`, `operating_system` (`ios`|`android`; V1 defaults to
`ios`), and on V2 `apns_sandbox`.

- `seconds_before` values ≤ 0 or non-numeric MUST be replaced with the default **600**.
- On creation the server SHOULD resolve the arrival via the region's OBA server
  (`arrival-and-departure-for-stop`) to compose a human message naming the route and
  headsign (e.g. *"The 44 to Ballard leaves in 10 minutes"*). The message is composed
  **at creation time** and stored; it is what eventually gets pushed.
- Store the alarm with a fresh secure token (§2.4) and return
  `201 {"url": "<sidecarBaseUrl>/api/v2/regions/{regionId}/alarms/<token>"}`.
- Validation failure → `422 {"error": "Unable to register alarm", "messages": […]}`.

**What is actually validated.** Only `user_push_id` (and, on V2, `operating_system`)
produce a `422` when missing. The trip-identity fields (`stop_id`, `trip_id`,
`service_date`, `stop_sequence`) are *client obligations, not server-enforced*: the
reference stores whatever arrives, and a V2 registration missing them still returns
`201` — the OBA lookup just fails and the alarm carries the generic message (and null
trip fields in its eventual push payload). Don't build stricter validation than this,
or clients that today get a degraded-but-working alarm will start getting errors. An
`operating_system` outside `ios`/`android` is rejected with the standard `422` on V2;
V1 treats it like an absent value and falls back to `ios`.

**Side effect (V2):** every successful V2 alarm creation also upserts a push
registration (§4) for `user_push_id` — refreshing `operating_system` and
`last_seen_at`, but carrying no `locale` and no `apns_sandbox`. Consequences:
alarm-only devices are part of the alert-push audience and reach counts, and their
180-day retention clock is refreshed by alarm creation, not only app launches. (Known
reference wart: because `apns_sandbox` isn't propagated, a sandbox debug build that
only ever creates alarms gets a production-routed *alert* registration, which bounces
terminally and is pruned.)

### 5.3 Firing behavior (normative)

A conforming implementation MUST run a scheduler equivalent to the following loop, at a
cadence of **once per minute** per pending alarm:

1. Fetch the alarm's current `arrival-and-departure-for-stop` from the region's OBA
   server (keyed by stop, trip, service date, vehicle, stop sequence).
2. Compute `seconds_until_departure` from the *predicted* departure when available,
   otherwise scheduled.
3. If `seconds_until_departure > seconds_before`: not yet — reset the failure counter
   (see below) and stop.
4. If `seconds_until_departure < 0`: the bus already left; **delete the alarm without
   pushing**. Waking a rider for a departed bus is worse than silence.
5. Otherwise fire: send the push (§6), then **delete the alarm**. An alarm fires at most
   once.

**Reaping unresolvable alarms.** If the OBA lookup fails with a "trip not found" or an
empty response, increment a per-alarm consecutive-failure counter; after **3 consecutive
failures**, delete the alarm. A successful lookup resets the counter. Without this, an
alarm whose trip has aged out of the feed is re-checked every minute forever — it never
fires and never dies. Transient network errors SHOULD NOT count toward the streak.

### 5.4 Push payload

The alarm push carries the stored message as its body, with title `OneBusAway` on
transports where the sender supplies the title (gorush, FCM; the legacy OneSignal
path's title comes from OneSignal app configuration, not the payload). It MUST also
carry a structured data payload the app uses to deep-link into the trip:

```json
{
  "arrival_and_departure": {
    "region_id": 1, "stop_id": "1_570", "trip_id": "1_604370",
    "service_date": 1754809200000, "vehicle_id": "1_4361",
    "stop_sequence": 3
  }
}
```

This exact key set and nesting is parsed by shipped apps; treat it as a wire contract.
`region_id` is the **public region identifier** — the same number as `{regionId}` in
the API paths. (Reference quirk: OBACloud sends its internal database id here, a
distinct column that happens to coincide with the public identifier in practice; a new
implementation should send the public identifier, which is the value the app can
actually match against its region list.)

### 5.5 Cancellation

`DELETE …/alarms/{token}` → `204` after actual deletion, `404` for an unknown token.
See §2.5 for why the distinction is load-bearing.

---

## 6. iOS Live Activities

```
POST   /api/v2/regions/{regionId}/live_activities
DELETE /api/v2/regions/{regionId}/live_activities/{token}
```

A Live Activity is a Lock Screen widget showing upcoming departures for one bookmarked
route+headsign at one stop. Unlike an alarm — one push, then gone — a Live Activity is a
**stateful subscription** the sidecar updates repeatedly via ActivityKit push
notifications until it ends. This is the most behaviorally demanding sidecar service.

### 6.1 Registration

Request fields: `activity_id` (the ActivityKit activity's id), `push_token` (the
activity's ActivityKit push token — *not* the device's alert token), `stop_id`,
`route_short_name`, `trip_headsign`, `apns_sandbox`, plus optional trip metadata
(`trip_id`, `service_date`, `vehicle_id`, `stop_sequence`).

Registration MUST be an **upsert keyed on `(region, activity_id)`**: ActivityKit rotates
push tokens over an activity's lifetime and iOS re-POSTs the same activity with each new
token. Each re-registration re-reads all fields including `apns_sandbox` (a rotated token
comes from the same build that sent the original). Handle the concurrent
first-registration race with a single retry. Response: `201 {"url": …}` with a secure
token (§2.4), same shape as alarms.

Identity is *bookmark-scoped* — stop + route + headsign, matching what the widget shows —
not trip-scoped. The trip fields are display metadata only. An activity lives through
many individual trips (the "next bus" keeps changing); that's the point.

Set a hard expiry at registration: **8 hours** (Apple's HIG ceiling for Live Activities).

### 6.2 The content-state contract

Every update push carries a `content-state` JSON object the widget decodes. This is a
strict wire contract with shipped apps:

```json
{
  "arrivals": [
    { "departure_time": 1767980460, "schedule_status": "on_time",
      "schedule_deviation": 60, "is_arrival": false }
  ]
}
```

- `arrivals`: at most **3** entries, sorted ascending by `departure_time`.
- `departure_time`: epoch **seconds** (not ms — the rest of the OBA ecosystem is ms;
  this field is seconds because ActivityKit dates decode from seconds).
- `schedule_status`: `"early"`, `"on_time"`, `"delayed"`, or `"unknown"` (unknown =
  schedule-only, no real-time). Thresholds, mirroring the iOS app exactly and
  half-open on the late side: deviation < −1.5 min → early; −1.5 (inclusive) to +1.5
  (exclusive) → on_time; ≥ +1.5 → delayed. Exactly −1.5 is on_time; exactly +1.5 is
  delayed.
- `schedule_deviation`: seconds, 0 when schedule-only.
- `is_arrival`: whether the time shown is an arrival (any stop but the trip's first,
  i.e. `stopSequence != 0`) or a departure (first stop). The app and the sidecar MUST
  agree on this rule or the displayed verb and minutes visibly flip between a pushed
  update and the app's own local refresh.

**No timestamp or other always-changing field may ever be added to the content state** —
change detection (§6.3) compares consecutive states, and a build stamp would defeat it.

Building the state from an OBA `arrivals-and-departures-for-stop` response (window:
5 minutes back, 120 minutes ahead):

1. Filter entries to the activity's `route_short_name` + `trip_headsign`.
2. **Collapse duplicate vehicle reports**: a realtime feed can briefly assign two
   vehicles to one trip (an AVL ghost beside the real coach), producing one entry per
   vehicle for a single stop visit — rendered as two identical rows. Group by the visit
   identity `(stopId, tripId, routeId, serviceDate, stopSequence)` and keep the entry
   with the newest `lastUpdateTime` (ties: first in response order). Never group by
   `tripId` alone — a loop route visits one stop twice per trip at different stop
   sequences, and the same `tripId` recurs across service dates within one 8-hour
   activity; those are distinct buses and must both survive. An entry missing any
   identity component MUST be kept uncollapsed rather than pooled into a nil-keyed
   group: showing a duplicate row is cosmetic, hiding a rider's bus is not.
   This algorithm is a deliberate port of the app's own client-side dedupe — both sides
   must pick the *same* survivor or pushed and locally-refreshed cards disagree.
3. For each entry choose arrival vs departure times per `is_arrival` above, falling back
   to departure times when a feed omits arrival times at a non-first stop; use predicted
   times when `predicted` is true and the predicted value is positive, else scheduled.
4. Drop entries whose chosen time is in the past; sort; take the first 3.

### 6.3 Update cycle (normative)

Run once per minute per live subscription:

1. **Expired?** (`now > expires_at`) → end the activity (§6.4, reason: expired).
2. Fetch arrivals and build the content state. On OBA/network error, count a failure
   (below) and stop this cycle.
3. **Empty `arrivals`** → count a failure and stop. One empty cycle MUST NOT end the
   activity: night headways and brief feed gaps produce valid-but-empty responses on
   healthy subscriptions. After **3 consecutive** failures (errors or empty), end it.
   Any successful non-empty build resets the counter.
4. Push an `update` event (§6.6) if the content changed **or** a keepalive is due. The
   keepalive threshold is **55 seconds** since the last push — deliberately *under* the
   1-minute cadence, because the last-push timestamp is stamped after the push
   round-trip returns; at exactly 60s the next cycle misses by milliseconds and the
   widget updates every *other* minute. The widget renders its countdown as a static
   string computed at push time — the push is the only thing that advances the on-screen
   clock, so in practice you push nearly every cycle.
5. Record the pushed state and timestamp.

Cost control: N subscriptions on one stop MUST NOT cost N upstream requests per cycle —
cache the per-stop OBA response for ~55 seconds and share it.

### 6.4 Ending

An activity ends by being **deleted**, with a best-effort `end` push (dismissal date
15 minutes out, reusing the last known content state). Triggers:

- Expiry (8 hours).
- 3 consecutive empty/error cycles.
- Client `DELETE …/live_activities/{token}` → `204`/`404` (no end push — the client is
  dismissing it itself).
- Terminal token feedback (§6.5) — also no end push: the token is dead, so an end push
  could not be delivered anyway.

The end push MUST be best-effort: delete the subscription even if the push fails,
otherwise a dead token keeps the row being re-checked and re-failed forever.

### 6.5 Push-failure feedback

APNs reports token death asynchronously (`Unregistered`, `BadDeviceToken`,
`DeviceTokenNotForTopic` — matched by substring; note `ExpiredProviderToken` is about
*your* provider JWT and is transient, not terminal). A conforming implementation SHOULD
consume its push transport's failure feedback and:

- **Live Activity token** → delete the subscription. Every future update would bounce.
- **Alert push token** → delete the push registration (§4), keeping reach honest.

Without this loop things still *mostly* work — alarms self-destruct after firing and
activities die at expiry — but you'll push into the void for hours and your reach counts
rot.

### 6.6 APNs specifics

Live Activity pushes have hard platform requirements; getting these wrong produces a
frozen Lock Screen card, which riders read as "the bus vanished":

- `apns-push-type: liveactivity`; the APNs topic is
  `<app bundle id>.push-type.liveactivity`.
- The `aps` payload carries `event` (`"update"`/`"end"`), `content-state` (§6.2),
  `timestamp` (epoch seconds, must advance each push), `stale-date` (SHOULD be ~10
  minutes out: if pushes stop, the widget dims as stale instead of showing a confident
  wrong time), and on `end`, `dismissal-date` (~15 minutes out).
- **APNs priority 10** (immediate), not 5: verified on-device, at priority 5 an idle
  phone simply holds every push and the Lock Screen sits frozen for 5+ minutes.
  Priority-10 Live Activity pushes count against Apple's hourly budget, which is why the
  *app* ships `NSSupportsLiveActivitiesFrequentUpdates` — without it these pushes get
  throttled too.
- Respect `apns_sandbox` per subscription (§2.7). The stakes are highest here: a
  misrouted push bounces `BadDeviceToken`, which your own feedback loop (§6.5) reads as
  terminal and destroys the subscription.

---

## 7. Surveys and survey responses

The survey service lets an agency run rider questionnaires inside the app: shown on the
map, on specific stops, or linking out to an external survey tool.

### 7.1 Fetching surveys

```
GET /api/v1/regions/{regionId}/surveys?user_id=<device uuid>
```

`user_id` is required (`422 {"errors": ["user_id is required"]}` without it); it lets an
implementation exclude surveys the device already answered, though the reference
currently returns all active surveys and lets the client filter. Return only surveys that
are marked available *and* currently inside their scheduled window (surveys with no
schedule are always active).

Response (see OpenAPI for the full schema):

```json
{
  "surveys": [
    {
      "id": 7, "name": "Rider satisfaction", "start_date": "…", "end_date": "…",
      "show_on_map": false, "show_on_stops": true, "always_visible": false,
      "allows_multiple_responses": false,
      "visible_stop_list": ["1_570", "1_578"], "visible_route_list": null,
      "study": { "id": 3, "name": "…", "description": "…" },
      "questions": [
        { "id": 21, "position": 1, "required": true,
          "content": { "type": "radio", "label_text": "How was your trip?",
                       "options": ["Great", "Fine", "Bad"] } }
      ],
      "created_at": "…", "updated_at": "…"
    }
  ],
  "region": { "id": 1, "name": "Puget Sound" }
}
```

Question `content.type` is one of:

| Type | Extra content fields |
|---|---|
| `text` | — (free text answer) |
| `label` | — (display-only, never answered, never `required`) |
| `radio` | `options: [string]` |
| `checkbox` | `options: [string]` |
| `external_survey` | `url`, `survey_provider`, `embedded_data_fields: [string]`, `sdk_configuration_values` (JSON object) — hands off to a third-party survey SDK; never `required` |

Targeting fields: `visible_stop_list` / `visible_route_list` are arrays of stop/route ids
(or `null` = everywhere); `show_on_map` / `show_on_stops` say where the survey surfaces;
`always_visible` pins it above the fold.

### 7.2 Submitting and amending responses

```
POST /api/v1/survey_responses                    → 201, creates
POST|PUT|PATCH /api/v1/survey_responses/{id}     → 200, amends ({id} = public identifier)
```

Note these paths are **not region-scoped** — the survey id already identifies the region.

Create fields: `survey_id`, `user_identifier` (device UUID), optional `stop_identifier` +
`stop_latitude`/`stop_longitude` (required together; a survey can be configured to
require the stop id), and `responses` — which is, by long-standing client contract, **a
JSON-array-encoded *string***, not a nested JSON array:

```
responses = "[{\"question_id\":21,\"question_type\":\"radio\",\"question_label\":\"How was your trip?\",\"answer\":\"Great\"}]"
```

Create response:

```json
{ "survey_response": { "id": "<public identifier>", "user_identifier": "…",
                       "update_path": "/api/v1/survey_responses/<public identifier>" } }
```

`update_path` exists because riders answer surveys *incrementally* — the app submits the
first answer immediately (so partial data survives an abandoned survey) and then amends.
The amend endpoint accepts the same `responses` parameter and **merges by
`question_id`**: an incoming answer replaces the stored answer to the same question;
answers to other questions are preserved. `POST` to the member path is supported because
some shipped clients POST to `update_path` rather than PATCH.

Unknown `{id}` on amend → `404 {"error": …}`, the same shape as an unknown `survey_id`
on create.

---

## 8. Ghost bus reports

```
POST /api/v2/regions/{regionId}/ghost_bus_reports
```

A "ghost bus" is a vehicle the app predicted but that never showed. Riders file a report
from the trip screen; agencies use the aggregate to find bad feeds and blocks. This is a
fire-and-forget write endpoint — there is no rider-facing read API.

Request fields:

| Field | Notes |
|---|---|
| `user_identifier` | Device UUID. Required. |
| `trip_identifier` | Required. |
| `service_date` | Epoch **milliseconds**. Required. |
| `route_identifier`, `stop_identifier`, `vehicle_identifier`, `stop_sequence` | Optional context. |
| `predicted` | Whether the app was showing a real-time prediction. |
| `schedule_deviation_minutes` | Deviation the app displayed. |
| `wait_duration_minutes` | How long the rider waited. MUST be one of `5, 10, 15, 20, 30` (30 = "30+"; the choice list is mirrored in the app UI). |
| `comment` | Free text, max 1000 chars. |
| `user_latitude` / `user_longitude` | Rider position, validated to ±90/±180. |
| `scheduled_arrival_at`, `predicted_arrival_at`, `prediction_last_updated_at` | Epoch **milliseconds**. |

All epoch-ms timestamp fields MUST be parsed as integers; a non-integer value (e.g. an
ISO date string) MUST coerce to null — and for `service_date`, then fail the presence
validation — rather than being fuzzily parsed. `service_date` participates in the dedupe
key, so its stored value must be deterministic.

**Dedupe (normative):** one report per `(region, user_identifier, trip_identifier,
service_date)`. A duplicate — whether caught by validation or by a concurrent-submission
race at the storage layer — MUST return a `422` with **`"error": "already_reported"`**;
that code is what clients key on, and they treat it as a benign "thanks, got it
already". A `500` on the race path would make the app retry forever. The `messages`
array is human-readable only and MAY differ between the two paths (the reference sends
"User identifier has already been taken" from validation and "User has already reported
this trip" from the race rescue).

Success: `201 {"id": "<public identifier>"}`.

Rate limits: §2.6. **Enrichment** (capturing a snapshot of the trip's real-time status
from the OBA server for the agency dashboard) SHOULD happen asynchronously after the
`201` — never make the rider wait on an upstream call — and is otherwise out of scope.

---

## 9. Weather

```
GET /api/v1/regions/{regionId}/weather
```

Returns current + hourly forecast for the region's center, in a normalized shape the apps
render on the stop screen. The reference implementation proxies
[Pirate Weather](https://pirateweather.net/) (a Dark Sky-compatible API); any provider
works if you map to the response schema (see OpenAPI): top-level `latitude`, `longitude`,
`region_identifier`, `region_name`, `retrieved_at`, `units`, `today_summary`,
`current_forecast`, and `hourly_forecast[]`, where each forecast object has `icon`
(Dark Sky icon vocabulary: `clear-day`, `rain`, `snow`, …), `summary`, `temperature`,
`temperature_feels_like`, `precip_per_hour`, `precip_probability`, `wind_speed`, `time`
(epoch seconds).

Cache upstream responses (the reference: 30 minutes) — weather changes slowly and the
upstream is metered. On upstream failure return `403` (the body, if any, is ignored);
shipped apps treat any non-200 as "hide the weather UI", and 403 is what they've been
tested against.

---

## 10. Vehicle search

```
GET /api/v1/regions/{regionId}/vehicles?query=<substring>
```

Fuzzy vehicle-id search across the region's agencies, used by "find my bus" UI. Proxies
the OBA server's vehicle listing:

- `query` under 3 characters (or absent) → `200 []` (avoids full-fleet scans).
- Substring match: the *query* is lowercased and trimmed, then matched against the raw
  vehicle ids — so matching is effectively case-insensitive only when fleet ids are
  lowercase or numeric (they almost always are). Replicate exactly (downcase the query,
  not the ids) rather than implementing true case-insensitivity, or results diverge on
  fleets with uppercase ids.
- Response: `[{ "id": "<agency id>", "name": "<agency name>", "vehicle_id": "1_4361" }]`.
- Cache aggressively (reference: fleet list 30 min, per-query results 5 min); the
  upstream call is expensive and the search box fires per keystroke.

---

## 11. Donations

```
POST /api/v1/payment_intents
```

Powers in-app donations via Stripe's PaymentSheet. Not region-scoped. The body is raw
JSON (§2.2 exception):

```json
{ "donation_amount_in_cents": 500, "donation_frequency": "one_time",
  "name": "Jane Rider", "email": "jane@example.com", "test_mode": "0" }
```

- `donation_frequency`: `"recurring"` → create a monthly Stripe subscription
  (`payment_behavior: default_incomplete`) and return
  `{ "client_secret", "customer_id", "ephemeral_key", "id" }` — the client secret of the
  subscription's first invoice's PaymentIntent, plus the customer + ephemeral key the
  PaymentSheet needs to manage payment methods.
- Anything else → one-time PaymentIntent (`usd`, automatic payment methods, receipt to
  `email`) and return `{ "client_secret", "id" }`.
- `id` is a per-request UUID for client-side correlation, not a Stripe id.
- Customers are found-or-created by email.
- `test_mode: "1"` routes to Stripe test keys, letting app builds exercise the flow
  against production infrastructure.
- Stripe errors → `500` with empty body.

This service is **optional**: a sidecar that doesn't take donations simply doesn't
implement it, and the apps hide the donations UI for regions that don't advertise it.

---

## 12. Background processing summary

A sidecar is not just a CRUD API — the background half is where the product lives. A
conforming implementation runs:

| Loop | Cadence | Behavior |
|---|---|---|
| Alarm checker | every minute, all alarms | §5.3: fetch prediction → fire/delete/skip; reap after 3 failed lookups |
| Live Activity updater | every minute, all subscriptions | §6.3: build state → push update/keepalive; end on expiry or 3-failure streak |
| Alert push fan-out | on alert send | §4: page audience, group by platform/locale/environment, reconcile failures |
| Push token pruning | daily | delete registrations not seen in 180 days |
| Feedback consumption | continuous | §6.5: terminal APNs errors delete the subscription/registration |

All loops MUST be safe under at-least-once execution (a crashed worker's work re-runs).
Alarm delivery is itself at-least-once: the alarm is deleted only *after* the push
send returns (a send can't join a database transaction), so a crash in the gap between
send and delete re-fires the alarm on the next cycle — an accepted duplicate, preferred
over deleting first and losing the notification. Live Activity pushes are naturally
idempotent (same content state); alert fan-out should track a cursor so a resumed send
doesn't re-push the whole audience.

### Push transport

The reference implementation sends every push through
[gorush](https://github.com/appleboy/gorush), a small self-hostable push gateway that
fronts APNs and FCM and posts asynchronous delivery feedback to a webhook. You MAY use
gorush, talk to APNs/FCM directly, or use any other transport — the normative
requirements are only the payload semantics in §5.4 and §6.6 and the feedback behavior
in §6.5. (Historical note: V1 alarms predate this and pushed through OneSignal; that is
why V1's `user_push_id` is a OneSignal player id.)

## 13. Data model and retention summary

| Entity | Key | Lifetime |
|---|---|---|
| Alarm | secure token | Until fired, cancelled, expired (departure passed), or reaped (3 failed lookups) |
| Live Activity | secure token; upsert identity `(region, activity_id)` | ≤ 8 hours; ends earlier on dismissal, failure streak, or dead token |
| Push registration | `(region, token)` | 180 days after last seen; earlier on terminal bounce or opt-out |
| Survey response | public identifier | Indefinite (agency data) |
| Ghost bus report | public identifier; dedupe `(region, user, trip, service_date)` | Indefinite (agency data) |
| Alert | server id (feed entity id `Alert_<id>`) | Authored content; feed shows published alerts only (apps hide out-of-window ones client-side via `active_period`) |

Privacy expectations baked into the contracts: no accounts, no PII beyond what riders
volunteer (donation name/email go to Stripe; survey answers and ghost-bus comments are
agency data), anonymous device UUIDs are client-generated, and every device-addressable
secret (push tokens) is reachable only by whoever presented it in the first place.

## 14. Out of scope

Deliberately **not** part of the sidecar contract:

- **Admin/authoring UI** for alerts and surveys, and agency dashboards for ghost-bus
  reports — implementation-specific.
- **`GET /api/v1/regions`** — a vestigial, non-functional endpoint in the reference
  implementation; apps use the regions directory instead.
- **`GET /api/v1/service_discovery/watchdog`** — OBACloud-internal monitoring
  (basic-auth-protected Prometheus service discovery), not a rider-facing service.
- **Webhooks** (`/webhooks/*`) — transport-specific plumbing between OBACloud and its
  own gorush/Render deployments; §6.5 states the required *behavior* however your
  transport reports failures.
- The OBA REST API itself, the regions directory format beyond the three fields in §1.2,
  and GTFS/GTFS-RT production.

## 15. Conformance checklist

A minimal sidecar that supports current apps implements:

1. Region scoping + 404 contract (§1.2) and error shapes (§2.5)
2. Alerts feed (§3) — even if it always returns an empty `FeedMessage`
3. Push registrations with upsert, throttle, and pruning (§4)
4. V2 + V1 alarms with the firing loop (§5)
5. Live Activities with the update loop and content-state contract (§6)
6. Surveys + responses (§7) — even if the survey list is always empty
7. Ghost bus reports with dedupe (§8)
8. Weather (§9) and vehicle search (§10) — apps degrade gracefully without them, but
   they are cheap proxies
9. `apns_sandbox` allow-list parsing everywhere a token is registered (§2.7)

Donations (§11) are optional. Validate your implementation against
[`openapi.yaml`](openapi.yaml) with any OpenAPI 3.1 tooling, and against the mobile apps
themselves — the iOS app (`OneBusAway/onebusaway-ios`) contains the client-side halves of
every contract here, including a decodable fixture of the Live Activity content state.
