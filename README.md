# field-service

The field guiding service for Chisimba 26. A stateless HTTP service that owns
field data — drives, sightings, trail records, sign-offs, media records — and
validates Chisimba bearer tokens locally against its own cached copy of the
issuer's JWKS. It holds no credentials, and no password ever reaches it.

The authoritative specification is
[`framework/docs/architecture/field-guiding-service-contract.md`](../framework/docs/architecture/field-guiding-service-contract.md),
the implementation design is
[`framework/docs/architecture/field-guiding-service-design.md`](../framework/docs/architecture/field-guiding-service-design.md),
and this service's own structure is documented in
[docs/architecture.md](docs/architecture.md).

## Architecture in one paragraph

One binary. An HTTP process (`cmd/field-service`) assembles a pgx pool, a Redis
client, and a JWKS cache, then builds an `authn.Validator`, a revocation
denylist (Redis), a session (the transaction that carries the caller's grants),
and an `httpapi.Guard` before a single route is registered. Handlers dispatch
to domain services (`push`, `pull`, `species`, `competency`, `signoff`,
`verify`) over pgx stores, into PostgreSQL 16 + PostGIS with row-level security
forced on every field-data table. Redis carries the denylist and runs
evict-only; it holds no record that counts as the source of truth.

## Routes

| Method and path | Door | Notes |
|---|---|---|
| `POST /api/v1/sync/push` | person | Idempotent batch of client operations |
| `POST /api/v1/sync/pull` | person | Change-feed page keyed by a bigserial cursor |
| `GET /api/v1/species`, `GET /api/v1/species/{code}` | person | Reference catalogue, no per-context copy |
| `GET /api/v1/competencies`, `GET /api/v1/competencies/{code}` | person | Reference catalogue |
| `POST /api/v1/signoffs`, `GET /api/v1/signoffs/{id}` | person | Draft creation and read-back |
| `POST /api/v1/log-book/verify` | service | Reached only with a service token |
| `GET /health` | none | Liveness probe, outside the guard |

Person routes are refused to service tokens and vice versa. Refusals are
identical on the wire; the reason goes only to the logs.

## Configuration

Read from the environment by `configFromEnv`, failing fast on anything missing:

| Variable | Default | Required |
|---|---|---|
| `FIELDSVC_LISTEN` | `:8080` | no |
| `FIELDSVC_DATABASE_URL` | — | yes |
| `FIELDSVC_REDIS_ADDR` | `127.0.0.1:6379` | no |
| `FIELDSVC_JWKS_URL` | — | yes |
| `FIELDSVC_ISSUER` | — | yes |
| `FIELDSVC_AUDIENCE` | — | yes |
| `FIELDSVC_REQUIRED_SCOPE` | `field:write` | no |
| `FIELDSVC_JWKS_REFRESH` | `15m` | no |

There are no defaults for issuer, audience or JWKS URL because a validator
built with a blank value would accept a token from anyone and enforce nothing.

## Running

The databases this service needs, and nothing else, are in `docker-compose.yml`:

```sh
docker compose up -d        # PostGIS 16 on 127.0.0.1:55433, Redis 7 on 127.0.0.1:56380
go run ./cmd/field-service
```

The ports are deliberately not the defaults: 5432 and 6379 collide with
whatever else is running on the machine.

## Schema

Applied in order from `migrations/`, idempotent so a migration that fails
halfway can be re-run. Migration headers carry the reasoning; summaries:

- `0001_isolation.sql` — isolation columns (`context_code`, `revision`), RLS forced
- `0002_runtime_role.sql` — the `fieldapp` runtime role that is actually subject to RLS
- `0003_sync_and_trails.sql` — trail logs, waypoints, change feed, operation outcomes, media
- `0004_caller_id_is_text.sql` — caller ids are text (Chisimba user ids), not uuid
- `0005_change_feed_sequence.sql` — sequence USAGE grant for the change feed
- `0006_species.sql` — the species catalogue
- `0007_drives_hikes_and_camps.sql` — outings, with per-kind detail tables
- `0008_log_book.sql` — sightings become the log book; redundant trail log dropped
- `0009_competencies.sql` — the competency catalogue
- `0010_signoffs.sql` — sign-offs and their assessment rows

## Tests

```sh
go test ./...                       # unit tests, no database needed
go test -tags e2e ./e2e/...         # end-to-end against real Postgres + Redis
```

The e2e suite drives the assembled binary over real HTTP. Its header records
four wiring faults that only appeared at first binary run and are now covered.

## Known gaps (verified against the code)

- The contract names the sign-off lifecycle routes `POST .../signoffs/{id}/submit`,
  `PATCH .../signoffs/{id}`, `PATCH .../signoffs/{id}/review` and
  `GET /api/v1/trainees/{id}/signoffs`. Only create and get-by-id are served
  today (`internal/httpapi/signoff_routes.go`).
- No media endpoints exist despite media records in `0003`, and no webhook
  delivery is implemented despite the design's Redis rationale.
- A duplicated comment paragraph sits above `InTx` in `internal/wiring/wiring.go`.
