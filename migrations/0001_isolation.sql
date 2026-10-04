-- 0001_isolation.sql — the two tables the isolation rules need, and the rules.
--
-- This migration is deliberately incomplete. It creates `drive` and `sighting`
-- and nothing else, because those two are enough to prove the isolation works
-- and isolation is the part that must not be written blind. The remaining
-- tables come in later migrations once these are tested against a real
-- database.
--
-- Every statement is idempotent so that the migration can be re-run against a
-- database that already has it. A migration that fails halfway and cannot be
-- re-run is a migration that has to be undone by hand.

create extension if not exists postgis;

-- The enums the contract names. They are created as native types rather than
-- as lookup tables because the contract requires validation and a native enum
-- validates on the way in, so a client cannot store a behaviour the service has
-- never heard of.
--
-- The values are provisional for behaviour and age_sex_class: the contract
-- declares both as enums but enumerates neither, so these are this project's
-- reading of the design's reference data rather than the contract's own list.
-- PostgreSQL has no `create type if not exists`, so a plain create type makes
-- this migration un-rerunnable and a migration that cannot be re-run has to be
-- undone by hand. The duplicate_object handler is what makes the claim above
-- true rather than aspirational.
do $$ begin
  create type sighting_status as enum (
    'pending', 'verified', 'rejected', 'needs_review'
  );
exception when duplicate_object then null;
end $$;

do $$ begin
  create type behaviour as enum (
    'feeding', 'resting', 'moving', 'hunting', 'drinking',
    'social', 'breeding', 'unknown'
  );
exception when duplicate_object then null;
end $$;

do $$ begin
  create type age_sex_class as enum (
    'adult_male', 'adult_female', 'subadult_male', 'subadult_female',
    'juvenile', 'calf_cub', 'unknown'
  );
exception when duplicate_object then null;
end $$;

create table if not exists drive (
  id               uuid primary key,
  context_code     text        not null,
  guide_id         text        not null,
  trainee_ids      text[]      not null default '{}',
  status           text        not null default 'planned',
  planned_start_time timestamptz,
  start_time       timestamptz,
  end_time         timestamptz,
  route            geometry(LineString, 4326),
  weather          text,
  notes            text,
  cancelled_reason text,
  sealed_at        timestamptz,

  -- Rule 14: ending a drive is not sealing it. A sealed drive refuses late
  -- observations and an unsealed one accepts them, so the two must not be the
  -- same column with two names.
  constraint drive_status_known check (
    status in ('planned', 'active', 'completed', 'cancelled')
  )
);

create table if not exists sighting (
  id               uuid primary key,
  context_code     text        not null,
  drive_id         uuid        not null references drive(id),
  revision         bigint      not null default 0,

  species_code     text,
  count            integer,
  location         geometry(Point, 4326) not null,
  location_accuracy_m double precision,
  distance_m       integer,
  bearing_deg      integer,
  behaviour        behaviour,
  age_sex_class    age_sex_class,
  notes            text,

  status           sighting_status not null default 'pending',
  verified_by      text,
  verified_at      timestamptz,
  verification_notes text,

  recorded_species_code text,
  recorded_count       integer,
  correction_reason    text,

  late_arrival     boolean     not null default false,
  captured_at      timestamptz not null,
  recorded_at      timestamptz not null,
  created_by       text        not null,

  deleted_at       timestamptz,
  tombstone_revision bigint,

  -- An absent count is not a zero. A zero asserts the animal was looked for and
  -- there were none, which is a different statement from not having counted.
  constraint count_positive check (count is null or count > 0),

  -- Rule 15: a correction changing species code or count requires a reason, and
  -- a silent overwrite cannot be represented as a completed review. As a table
  -- constraint there is no code path, ours or a migration's, that can produce a
  -- correction without one.
  constraint correction_has_reason check (
    (recorded_species_code is null and recorded_count is null)
    or correction_reason is not null
  )
);

-- The composite index is led by context_code because every list and every count
-- is `where context_code = any($grants)`. Without this leading column a scan
-- forgets the predicate, and a scan that forgets the predicate returns another
-- reserve's records.
create index if not exists sighting_context_drive_revision
  on sighting (context_code, drive_id, revision);
create index if not exists sighting_location_gix
  on sighting using gist (location);

-- ---------------------------------------------------------------------------
-- Isolation layer 2: row-level security.
--
-- Layer 1 is the query layer, where every repository function takes a caller
-- and adds the predicate. This is the belt to that braces.
-- ---------------------------------------------------------------------------

alter table drive    enable row level security;
alter table sighting enable row level security;

-- Without this the table owner bypasses its own policies, so a migration or a
-- manual psql session reads across every context with no error at all. It does
-- not protect against a superuser or a pg_dump, both of which bypass RLS
-- outright; that is accepted because this database holds field data and no
-- credential.
alter table drive    force row level security;
alter table sighting force row level security;

-- current_setting with no missing_ok default raises when the setting is absent,
-- which is deliberate: a code path that forgets to set the grants errors
-- instead of quietly reading everything. Adding a default here would deny all
-- reads — still safe, but as a total outage rather than a loud bug.
--
-- Measured, and slightly different from what the design predicted. On a
-- connection that has never set the variable, this raises:
--   ERROR: unrecognized configuration parameter "app.context_grants"
-- which is the loud failure and the better one. But on a connection where a
-- transaction ran `set local app.context_grants = 'north'` and committed, the
-- variable is no longer *absent* — PostgreSQL keeps the placeholder defined and
-- empty. The read then returns '' rather than raising, string_to_array('', ',')
-- is {""}, and the predicate matches nothing.
--
-- That second case denies every row rather than erroring. It is still safe, and
-- it is still better than leaking, but it is the outage mode rather than the
-- loud one, so a deployment that starts returning empty pages should check this
-- before it concludes the policies have failed. It is recorded here because the
-- two cases look identical from the application's side and only one of them is
-- a bug worth chasing.
drop policy if exists drive_in_granted_context on drive;
drop policy if exists sighting_in_granted_context on sighting;

-- The with-check clause is redundant here and is kept deliberately: PostgreSQL
-- applies the using expression to inserts when no with check is given, so
-- dropping it changes nothing (confirmed by removing it and re-running the
-- tests). It stays because a reader should not have to know that to be sure a
-- write cannot land in an ungranted context, and because the day the using
-- expression becomes an OR of two predicates the redundancy stops being
-- harmless.
create policy drive_in_granted_context on drive
  using (context_code = any (string_to_array(current_setting('app.context_grants'), ',')))
  with check (context_code = any (string_to_array(current_setting('app.context_grants'), ',')));

create policy sighting_in_granted_context on sighting
  using (context_code = any (string_to_array(current_setting('app.context_grants'), ',')))
  with check (context_code = any (string_to_array(current_setting('app.context_grants'), ',')));
