-- 0003_sync_and_trails.sql — trail logs, waypoints, the change feed,
-- operation outcomes and media records.
--
-- Every table here carries a `context_code` and gets the same isolation as
-- sighting and drive: row-level security enabled *and* forced, with a policy
-- reading the grants the guard set for this request. The one exception is
-- operation_outcome, which is keyed by caller rather than by context, and is
-- isolated differently below.

-- ---------------------------------------------------------------------------
-- Trail logs
--
-- The contract's headline: "Appends are additive; a trail log is not rewritten,
-- because the sequence of observations is itself the record. This is the one
-- entity where ordering is the content, so a revision conflict on a waypoint
-- list is resolved by appending rather than by refusing."
--
-- So there is no revision column and no updated_at on the log or its waypoints.
-- A correction is another row. Nothing here can be edited in place, which is
-- what makes that promise structural rather than a convention.
create table if not exists trail_log (
  id             uuid primary key,
  -- Not a foreign key. There is no context table in the contract: the code
  -- comes from the token's `ctx`/`ctxs` claims, and the only things that make
  -- it trustworthy are the policies below and the index led by it. A
  -- reference to drive(context_code) would invent a context table and add a
  -- second way for a context to be rejected.
  context_code   text        not null,
  drive_id       uuid        not null references drive(id),
  started_at     timestamptz not null,
  ended_at       timestamptz,
  lessons_learned text,
  hours_walked   double precision,
  rifle_role     text,
  created_by     text        not null,

  -- First, second or participant, never merged. Merging them produces one
  -- larger number that means nothing a mentor could act on.
  constraint trail_log_rifle_role_known check (
    rifle_role is null or rifle_role in ('first', 'second', 'none')
  ),

  -- Ending a walk is not sealing it, and the two are separate columns for the
  -- same reason they are on drive: a finished walk's log still accepts late
  -- waypoints.
  constraint trail_log_ends_after_it_starts check (
    ended_at is null or ended_at >= started_at
  ),

  -- A reflection is not a claim about the world, so it cannot be verified. It is
  -- also not optional: the sightings are what was observed and the reflection is
  -- what it taught.
  constraint trail_log_teaches_something check (
    lessons_learned is null or btrim(lessons_learned) <> ''
  )
);

-- A waypoint's identity *is* its position in the walk, so `ordinal` is part of
-- the key. There is no surrogate id to renumber and no updated_at, because a row
-- that could be edited in place would contradict the rule the contract names.
--
-- A guide who stops twice in the same place has two waypoints at the same
-- coordinates and two ordinals: the second stop is a real event even though the
-- position repeats.
create table if not exists trail_waypoint (
  trail_log_id uuid    not null references trail_log(id),
  -- Denormalised from the parent log rather than joined to it. Every field
  -- record carries its owning context, and a policy cannot reach through a join
  -- to the parent without a SECURITY DEFINER function — which would move the
  -- decision out of the database's own hands and into a function whose owner
  -- can see everything.
  context_code text    not null,
  ordinal      integer not null,
  point        geometry(Point, 4326) not null,
  captured_at  timestamptz not null,
  accuracy_m   double precision,
  note         text,
  primary key (trail_log_id, ordinal),

  constraint trail_waypoint_ordinal_positive check (ordinal >= 0)
);

-- The geometry index is the difference between "was this walk near that
-- sighting" being a query and being a scan.
create index if not exists trail_waypoint_point_gix on trail_waypoint using gist (point);

-- ---------------------------------------------------------------------------
-- The change feed
--
-- It carries the row, not just its identity. Identity-only turns one page into N
-- round trips over a connection that is already marginal. A live join is wrong
-- in a way that only shows under load: the page stops being a consistent
-- snapshot, so a client can receive revisions 4 and 5 of one entity and apply
-- them in the wrong order, or receive a row deleted between the feed read and
-- the join.
--
-- Retention is 30 days, which is what makes `resync_required` necessary rather
-- than theoretical: a cursor older than the window has to be answered with a
-- distinct status rather than an empty page.
create table if not exists change_feed (
  seq          bigserial primary key,
  context_code text        not null,
  entity_type  text        not null,
  entity_id    uuid        not null,
  revision     bigint      not null,
  changed_at   timestamptz not null default now(),
  body         jsonb       not null
);

-- Led by context_code, like every other index here: every pull is
-- `where context_code = any($grants) and seq > $cursor`.
create index if not exists change_feed_context_seq on change_feed (context_code, seq);

-- ---------------------------------------------------------------------------
-- Operation outcomes
--
-- Rule 7: idempotency keyed by operation id, never entity id. The caller is
-- part of the key because an operation id is never proof of identity — it is a
-- string somebody chose.
--
-- This table is NOT isolated by context. A caller may only ever see their own
-- outcomes, which is a different boundary and needs a different setting, set by
-- the guard alongside the grants.
create table if not exists operation_outcome (
  caller_user_id uuid        not null,
  operation_id   uuid        not null,
  entity_type    text        not null,
  entity_id      uuid        not null,
  kind           text        not null,
  outcome        text        not null,
  result         jsonb,
  recorded_at    timestamptz not null default now(),
  primary key (caller_user_id, operation_id),

  constraint operation_outcome_known check (
    outcome in ('applied', 'noop', 'deferred', 'refused', 'unknown_operation')
  )
);

-- ---------------------------------------------------------------------------
-- Media records
--
-- The binaries never pass through this service. A record is initialised, the
-- client uploads straight to object storage with a pre-signed URL, and then
-- confirms the object arrived. Without the confirm step an upload that failed
-- silently leaves a record that renders as a broken image, so `state` is what
-- tells a client whether the bytes are coming.
--
-- An abandoned initialisation is marked `unavailable` and swept, never deleted:
-- something that referenced it should keep finding it.
create table if not exists media (
  id           uuid primary key,
  context_code text        not null,
  drive_id     uuid        not null references drive(id),
  kind         text        not null,
  sighting_id  uuid        references sighting(id),
  trail_log_id uuid        references trail_log(id),
  filename     text        not null,
  mime_type    text        not null,
  size_bytes   bigint      not null,
  -- From the file where it has one, falling back to the device clock. A phone
  -- that has been out of coverage can have a clock wrong in a way that quietly
  -- corrupts every sighting taken that day.
  captured_at  timestamptz,
  state        text        not null default 'pending',
  uploaded_at  timestamptz,

  constraint media_kind_known check (kind in ('photo', 'video', 'audio')),

  -- Exactly one parent, and the contract says "exactly one applies".
  constraint media_has_one_parent check (
    (sighting_id is not null)::int + (trail_log_id is not null)::int = 1
  ),

  constraint media_state_known check (
    state in ('pending', 'confirmed', 'unavailable')
  ),

  constraint media_size_non_negative check (size_bytes >= 0)
);

create index if not exists media_context_state on media (context_code, state);

-- ---------------------------------------------------------------------------
-- Isolation
--
-- Every context-bearing table gets the same three layers as sighting and drive.
-- Enumerated rather than written by hand so that adding a table without a policy
-- is a visible omission rather than a silent hole.
-- ---------------------------------------------------------------------------
do $$
declare
  t text;
begin
  foreach t in array array[
    'trail_log', 'trail_waypoint', 'change_feed', 'media'
  ] loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format('drop policy if exists %I on %I', t || '_in_granted_context', t);
    execute format(
      'create policy %I on %I using (context_code = any ('
      || 'string_to_array(current_setting(''app.context_grants''), '','')))'
      || ' with check (context_code = any ('
      || 'string_to_array(current_setting(''app.context_grants''), '','')))',
      t || '_in_granted_context', t);
  end loop;
end $$;

-- operation_outcome is bounded by the caller rather than the context. The
-- setting must fail rather than default: an unset caller would otherwise read
-- as a caller id that matches nothing, which denies everything, and a default of
-- empty would deny everything too — both safe, but a loud error is a better
-- failure than a total outage nobody can explain.
alter table operation_outcome enable row level security;
alter table operation_outcome force row level security;

drop policy if exists operation_outcome_own_only on operation_outcome;
create policy operation_outcome_own_only on operation_outcome
  using (caller_user_id::text = current_setting('app.caller_user_id'))
  with check (caller_user_id::text = current_setting('app.caller_user_id'));
