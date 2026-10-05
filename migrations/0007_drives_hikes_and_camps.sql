-- 0007_drives_hikes_and_camps.sql — an outing, and what is required of each kind.
--
-- A game drive is one way to spot nature and animals. Walking a trail is
-- another. Camping is a third. All three are things a trainee does and a mentor
-- assesses, and all three produce sightings — but they are not the same record.
--
-- ---------------------------------------------------------------------------
-- Why not one table with a kind column
--
-- Because a drive requires a guest count and a duration, and a walking trail
-- requires a rifle role and a walk length, and neither pair means anything for
-- the other. One table with nullable columns would make those fields optional on
-- every row, and the trails programme's own plan is explicit about why that is
-- wrong: a drive and a walking trail are "different records with different
-- required fields, not one record with some fields that happen to be hidden",
-- because "a hidden required field is a field the client did not send, which is
-- a validation failure discovered late rather than a rule enforced early".
--
-- So `outing` holds what all three share — who, when, where, and the lifecycle
-- rule 14 describes — and each kind's own required fields live in a detail table
-- keyed by the outing. A hike with no rifle role has no hike_detail row to hang
-- them on, so it cannot exist as a completed hike.
--
-- ---------------------------------------------------------------------------
-- Why sightings point at outing and not at three tables
--
-- sighting, trail_log and media each referenced drive(id) directly. Pointing
-- them at three possible parents would mean three nullable columns and a check
-- that exactly one is set, and every sighting query would then be conditional on
-- which parent is present. One parent, one column, no condition.
--
-- ---------------------------------------------------------------------------
-- The rename
--
-- drive becomes outing rather than being duplicated. Every existing row is a
-- drive, every existing sighting keeps its parent, and nothing has to be copied.
-- Renaming the table takes the foreign keys with it, so the three references are
-- repointed by renaming the column on each.

-- Every statement below is guarded. A rename is not naturally re-runnable, and a
-- migration that fails on its second run gets run once and then feared. The
-- guards are written so that "already applied" and "never applied" both finish
-- with the same schema.
do $$
begin
  if to_regclass('public.drive') is not null and to_regclass('public.outing') is null then
    alter table drive rename to outing;
  end if;
end $$;

-- The constraint and policy keep their old names otherwise, and "drive" is now a
-- kind rather than the table. Leaving them would make a schema dump misleading
-- rather than merely stale.
do $$
begin
  if exists (select 1 from pg_constraint where conname = 'drive_status_known') then
    alter table outing rename constraint drive_status_known to outing_status_known;
  end if;
  if exists (select 1 from pg_policies where policyname = 'drive_in_granted_context') then
    alter policy drive_in_granted_context on outing rename to outing_in_granted_context;
  end if;
end $$;

comment on table outing is
  'A drive, a hike or a camp. The kind says which; the detail tables say what that '
  'kind requires. Sightings, trail logs and media hang off this, never off a kind.';

-- ---------------------------------------------------------------------------
-- The kind
--
-- Defaulted to 'drive' rather than backfilled, because every row that existed
-- before this migration was a drive and saying so is a fact, not a guess. The
-- default remains for inserts that omit it.
alter table outing add column if not exists kind text not null default 'drive';

do $$
begin
  if not exists (select 1 from pg_constraint where conname = 'outing_kind_known') then
    alter table outing add constraint outing_kind_known
      check (kind in ('drive', 'hike', 'camp'));
  end if;
end $$;

comment on column outing.kind is
  'Which kind of outing this is. Decides which detail row must exist and which '
  'fields that kind requires. One of drive, hike, camp.';

-- ---------------------------------------------------------------------------
-- Drive detail
--
-- guest_count and duration_hours are nullable, and that is a concession to
-- history rather than a relaxation of the rule.
--
-- The trails programme requires both. Eighteen drives existed before this
-- migration and not one of them recorded either value, so there was nothing to
-- backfill: a NOT NULL column could only have been satisfied by inventing a
-- duration for a journey whose length nobody recorded. An invented 0.0 hours
-- would be indistinguishable from a drive that really did last no time, which is
-- precisely the sort of quiet wrongness this schema exists to prevent.
--
-- So the columns allow absence, and the push service refuses a *new* drive that
-- omits them. A legacy row stays readable; it cannot be created again.
create table if not exists drive_detail (
  outing_id      uuid primary key references outing(id),
  duration_hours numeric check (duration_hours is null or duration_hours > 0),
  guest_count    integer check (guest_count is null or guest_count >= 0)
);

comment on table drive_detail is
  'What a game drive requires. Absent on legacy drives predating this migration, '
  'which recorded neither value; required on every drive created since.';

-- ---------------------------------------------------------------------------
-- Hike detail
--
-- rifle_role is NOT NULL and constrained, and 'neither' is a permitted answer
-- rather than a nullable column. The programme counts first rifle, second rifle
-- and participant separately, and a participant who was on neither rifle is a
-- fact worth recording — distinct from a blank, which is a client that sent
-- nothing.
create table if not exists hike_detail (
  outing_id uuid primary key references outing(id),

  rifle_role       text    not null check (rifle_role in ('first', 'second', 'neither')),
  walk_length_km   numeric not null check (walk_length_km > 0),
  hours_walked     numeric check (hours_walked is null or hours_walked >= 0),
  description      text,
  lessons_learned  text
);

comment on table hike_detail is
  'What a walking trail requires. rifle_role is required and "neither" is a real '
  'answer, so the column is constrained rather than nullable.';

-- ---------------------------------------------------------------------------
-- Camp detail
--
-- A camp has no route, so nothing here is required beyond the link itself: where
-- it was and when it ran are on the outing, and a camp that did not move has no
-- path to record. Sightings attach to a camp normally, because animals at a camp
-- are exactly what a guide sees.
create table if not exists camp_detail (
  outing_id  uuid primary key references outing(id),
  site_name  text,
  facilities text
);

comment on table camp_detail is
  'Optional detail for a camp. Nothing is required: a camp is a place and a '
  'duration, both of which are on the outing.';

-- ---------------------------------------------------------------------------
-- Isolating the detail tables
--
-- Each has no context_code of its own, because copying the outing's would let the
-- two disagree and there would be no way to tell which was right. The policy
-- instead asks the parent, so a detail row is readable exactly when its outing
-- is — which is the same rule the parent already has.
--
-- Note that this is the rule as written, not the rule as enforced today: the
-- deployed role is a superuser and superusers bypass row-level security
-- outright, so the push path enforces this in application code. See
-- driveInContext's replacement in internal/push.
do $$
begin
  if not exists (select 1 from pg_policies where policyname = 'drive_detail_in_granted_context') then
    create policy drive_detail_in_granted_context on drive_detail
  using (exists (select 1 from outing o
                 where o.id = drive_detail.outing_id
                   and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))))
  with check (exists (select 1 from outing o
                      where o.id = drive_detail.outing_id
                        and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))));
  end if;
end $$;

do $$
begin
  if not exists (select 1 from pg_policies where policyname = 'hike_detail_in_granted_context') then
    create policy hike_detail_in_granted_context on hike_detail
  using (exists (select 1 from outing o
                 where o.id = hike_detail.outing_id
                   and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))))
  with check (exists (select 1 from outing o
                      where o.id = hike_detail.outing_id
                        and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))));
  end if;
end $$;

do $$
begin
  if not exists (select 1 from pg_policies where policyname = 'camp_detail_in_granted_context') then
    create policy camp_detail_in_granted_context on camp_detail
  using (exists (select 1 from outing o
                 where o.id = camp_detail.outing_id
                   and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))))
  with check (exists (select 1 from outing o
                      where o.id = camp_detail.outing_id
                        and o.context_code = any (string_to_array(current_setting('app.context_grants'), ','))));
  end if;
end $$;

alter table drive_detail enable row level security;
alter table drive_detail force row level security;
alter table hike_detail  enable row level security;
alter table hike_detail  force row level security;
alter table camp_detail  enable row level security;
alter table camp_detail  force row level security;

-- ---------------------------------------------------------------------------
-- Repointing the children
--
-- Renaming the column rather than dropping and re-adding the constraint, so the
-- foreign key survives as the same object against the renamed table. The
-- constraint names are renamed too: "sighting_drive_id_fkey" would otherwise
-- describe a column that no longer exists.
do $$
declare
  t text;
begin
  foreach t in array array['sighting', 'trail_log', 'media'] loop
    if exists (select 1 from information_schema.columns
               where table_schema = 'public' and table_name = t and column_name = 'drive_id') then
      execute format('alter table %I rename column drive_id to outing_id', t);
    end if;
  end loop;

  if exists (select 1 from pg_constraint where conname = 'sighting_drive_id_fkey') then
    alter table sighting rename constraint sighting_drive_id_fkey to sighting_outing_id_fkey;
  end if;
  if exists (select 1 from pg_constraint where conname = 'trail_log_drive_id_fkey') then
    alter table trail_log rename constraint trail_log_drive_id_fkey to trail_log_outing_id_fkey;
  end if;
  if exists (select 1 from pg_constraint where conname = 'media_drive_id_fkey') then
    alter table media rename constraint media_drive_id_fkey to media_outing_id_fkey;
  end if;
end $$;

drop index if exists sighting_context_drive_revision;
create index if not exists sighting_context_outing_revision
  on sighting (context_code, outing_id, revision);

-- ---------------------------------------------------------------------------
-- Backfill
--
-- Every pre-existing outing is a drive, so every one of them gets a drive_detail
-- row. Without it the new schema would assert that a drive need not have a
-- detail row, and the next drive created after this migration would be the first
-- one held to a different rule from the eighteen before it.
--
-- The row carries no duration and no guest count because none was ever recorded.
insert into drive_detail (outing_id)
select id from outing
on conflict (outing_id) do nothing;