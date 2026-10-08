# Field service architecture

This document describes how `field-service` is built and why. It assumes
[`framework/docs/architecture/field-guiding-service-contract.md`](../../framework/docs/architecture/field-guiding-service-contract.md)
has been read — the contract says what the service must do and never do; this
says how it does it.

## What this service is

A stateless HTTP service that owns field data — outings, log-book entries,
trail records, sign-offs, media records — and validates Chisimba bearer tokens
locally. It is one of three deployables:

| Deployable | Owns | Never does |
|---|---|---|
| Chisimba (PHP/MDB2) | identity, groups, permissions, the mentor's own UI | store an outing, a log entry or a sign-off |
| Field service (this) | all field data, media *records*, reference catalogues | store or verify a credential, decide visibility from its own tables |
| Object storage (S3) | media bytes | anything else |

A review is a command sent to this service, not a row written into Chisimba.
Chisimba renders what this service returns, so there is exactly one writer of
field data.

## Request path

`cmd/field-service/main.go` assembles the process and is careful about order:
the pool must exist before the session, the session before the guard, and the
guard before a single route is registered. Getting that order wrong does not
crash — it produces a service that validates tokens and then reads everything,
or refuses everything, and both look like working software until the first
field device is left behind.

Every request through the person door passes `httpapi.Guard.Serve`:

1. Parse the bearer token; the algorithm allowlist excludes `alg:none` and HMAC.
2. Load the caller's revocation epoch from Redis; a denylist that cannot be
   read fails closed.
3. Check the required scope (`FIELDSVC_REQUIRED_SCOPE`, default `field:write`).
4. Open a transaction and set `app.context_grants` and `app.caller_user_id`
   transaction-locally with `set_config(..., true)`.
5. Hand the transaction's query handle to the domain service.

The service door (`Guard.ServeService("field:verify")`) applies the same
mechanical checks but with a service token: no context column is inherited, so
every context rule in the request is written in the request.

Refusals are identical on the wire regardless of cause; the true reason goes
only to the structured log. A probe that can tell "revoked" from "unknown
caller" has been handed the answer before it has presented a credential.

## The token

RS256-family, locally verified against a JWKS cache (`internal/jwks`, refresh
interval `FIELDSVC_JWKS_REFRESH`, default 15m). Claims carry `sub` (the
Chisimba user id, unchanged — rule 2 keeps it text, not uuid), `type`
("access" vs "service"), `scope`, `ctx` (active context), `ctxs` (grant set),
`roles`, `epoch` (revocation), `ver`, and the usual exp/nbf/iat. A service
token is refused on the person door and vice versa.

## Isolation: three places at once

The contract requires row-level isolation enforced in three places; the code
records why each is a distinct layer:

- **The guard** sets `app.context_grants` from the token's `ctxs` before any
  query runs, so an honest caller never sees another context.
- **Postgres policies** on every field-data table read those settings; a row
  in a context the caller lacks cannot be read even if the guard has a bug.
- **The runtime role** (`fieldapp`, created in `0002`) is a non-superuser
  without `BYPASSRLS`, because the table owner and superusers are exempt from
  RLS entirely. The first version of the policies was inert precisely because
  the application connected as the superuser; `0002` is the fix and its header
  keeps the postmortem.

Catalogues (`species`, `competencies`) carry no `context_code` and no RLS,
deliberately: a lion is a lion in every reserve, and a per-context catalogue
would defeat the offline bundle the contract asks for.

## Sync

Push (`internal/push`): operations are intents, not replacements. Idempotency
is keyed by operation id scoped to the caller; a replay returns the originally
recorded outcome, including refusals. Conflicts are refused and the client's
version is preserved — never overwritten. The outcome row commits in the same
transaction as the entity change, so a retry can find out its fate without
re-applying. The write context comes from the token, never the body.

Pull (`internal/pull`): a change-feed page keyed by a bigserial cursor. The
page carries each change whole (`seq`, `entity_type`, `entity_id`, `revision`,
`changed_at`, `body`), because identity-only would turn one page into N
round trips over a marginal connection. A cursor older than the feed window is
answered `resync_required` — a first-class status, never an error, because an
empty page would tell a device that has silently fallen behind nothing at all.

## Storage layout

Migrations 0001–0010, idempotent, headers carry the reasoning. Highlights:

- `0001` creates the two columns every record carries (`context_code`,
  `revision`) plus `drive` and `sighting` to prove isolation; enums validate on
  the way in.
- `0003` adds the change feed, operation outcomes, media records, trail tables;
  the feed is append-only to the service role (no UPDATE/DELETE grants, and
  `0005` adds the missing sequence USAGE that running the first push exposed).
- `0007` replaces the `drive` parent with `outing` plus per-kind detail
  (`drive_detail`, `hike_detail`, `camp_detail`): a hike requires a rifle role
  and has no meaning on a drive, so the required fields live where they can be
  NOT NULL rather than as nullable columns on one wide table.
- `0008` removes `trail_log` as redundant: every column already existed on
  `outing`/`hike_detail`. The sightings become the log book.
- `0009`/`0010` seed the competency catalogue idempotently (stable codes,
  eight characters, never derived from names) and model a sign-off as two
  tables: the sign-off and its assessment rows. The "one to twenty
  assessments" bound cannot be a check constraint — a constraint sees one row
  — so it is enforced in one function before any row is written.

## Redis

Three unrelated jobs sharing one property: it is never the only copy of
anything, which is what lets the container run with
`--maxmemory-policy allkeys-lru` and no persistence. The revocation denylist
is bounded by access-token lifetime; losing Redis costs a re-fetch or a
delayed job, never a record.

## Tests

Unit tests per package alongside the code, with heavy "why" doc comments.
Store-level tests (`internal/store`, `pull/pgstore_test`, `push/pgstore_test`)
run against a real Postgres to prove the policies and isolation for real —
RLS bugs do not show up against fakes. The e2e suite
(`e2e/`, `go test -tags e2e ./e2e/`) drives the assembled binary over real
HTTP and its header records four wiring faults found only at first binary run
and now covered: a swallowed `redis.Nil`, a refusal that did not stop the
handler, a panic on a fault path, and a batch failing entirely.

## Known gaps (verified against the code)

- The sign-off lifecycle beyond create/read — `submit`, `PATCH`, `review`,
  the trainee listing — is specified in `migrations/0010_signoffs.sql` and not
  yet served (`internal/httpapi/signoff_routes.go`).
- Media endpoints and webhook delivery are designed but not implemented; only
  the record tables exist.
- The test `service_guard_test.go` references `/api/v1/logbook/x/verification`;
  production wiring is `/api/v1/log-book/verify`. The divergent test URL is
  stale and worth updating.
- A duplicated comment paragraph sits above `InTx` in `internal/wiring/wiring.go`.
- There is no README-level runbook for operating a deployment: env, migrations,
  JWKS rotation are only discoverable from `main.go` and this document.
