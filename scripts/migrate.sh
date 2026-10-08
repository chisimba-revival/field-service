#!/bin/sh
# Apply the field-service migrations in order.
#
# The migrations are idempotent, so re-running this against a database that
# already has them is safe. It runs as a one-shot container before
# field-service starts, so a fresh install has a schema rather than a
# connection error.
set -eu

DSN="${1:-postgres://fieldsvc:fieldsvc@field-service-postgis-1:5432/fieldlog?sslmode=disable}"

# Create migration tracking table if it doesn't exist
psql "$DSN" -v ON_ERROR_STOP=1 -c "
CREATE TABLE IF NOT EXISTS _migrations (
    filename text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);"

# Get already applied migrations
applied=$(psql "$DSN" -t -A -c "SELECT filename FROM _migrations;")

for f in /migrations/*.sql; do
    fname=$(basename "$f")
    if echo "$applied" | grep -q "^${fname}$"; then
        echo "skipping $fname (already applied)"
        continue
    fi
    echo "applying $fname"
    psql "$DSN" -v ON_ERROR_STOP=1 -f "$f"
    psql "$DSN" -v ON_ERROR_STOP=1 -c "INSERT INTO _migrations (filename) VALUES ('$fname')"
done

# The runtime role is created by 0002 with LOGIN and no password — inert
# until somebody sets one. The service connects as fieldapp, never as the
# migration superuser, because a superuser bypasses every RLS policy.
psql "$DSN" -v ON_ERROR_STOP=1 -c "ALTER ROLE fieldapp WITH LOGIN PASSWORD 'fieldsvc'"

# Grant SELECT on reference tables (no RLS, readable by all)
psql "$DSN" -v ON_ERROR_STOP=1 -c "GRANT SELECT ON species TO fieldapp"
psql "$DSN" -v ON_ERROR_STOP=1 -c "GRANT SELECT ON competency TO fieldapp"
# Also ensure fieldapp has USAGE on schema
psql "$DSN" -v ON_ERROR_STOP=1 -c "GRANT USAGE ON SCHEMA public TO fieldapp"

echo "migrations applied"
