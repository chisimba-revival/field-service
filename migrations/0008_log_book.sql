-- 0008_log_book.sql — the log book, and the removal of the redundant trail log.
--
-- ---------------------------------------------------------------------------
-- Why "log book"
--
-- What a trainee writes down is a log. The system called it a "sighting", which
-- is only one thing a log holds, and it separately called something else a "trail
-- log" — which was not the sightings at all and held no observation of anything:
--
--   trail_log.outing_id       -> already on outing
--   trail_log.started_at      -> already on outing.start_time
--   trail_log.ended_at        -> already on outing.end_time
--   trail_log.rifle_role      -> already on hike_detail.rifle_role
--   trail_log.hours_walked    -> already on hike_detail.hours_walked
--   trail_log.lessons_learned -> already on hike_detail.lessons_learned
--
-- Every column existed twice. The table was a duplicate of outing and
-- hike_detail with nothing added, so two tables meant one thing for this session
-- and nothing for the next — which is how "the sightings are the trail logs" came
-- to be true in a way nobody intended when they said it.
--
-- So the sightings become the log book, and trail_log goes. There is nothing in it
-- that survives the move, which is the only reason this is a drop rather than a
-- migration: had it held anything of its own, the honest cost would have been
-- reconciling two answers to the same question rather than deleting one.
--
-- ---------------------------------------------------------------------------
-- Renamed, not copied
--
-- Every sighting already written keeps its identity, its revision and its
-- verification state. Copying into a new table and dropping the old would have
-- produced the same rows with the same ids and no reason to prefer it; a rename
-- cannot lose anything, because there is only ever one table.

-- The table. Dependent foreign keys follow it automatically, so media keeps
-- pointing at the same rows without being touched.
do $$
begin
  if to_regclass('public.sighting') is not null
     and to_regclass('public.log_book_entry') is null then
    alter table sighting rename to log_book_entry;
  end if;
end
$$;

-- Constraints and indexes carrying the old name, so a schema dump does not
-- describe a table that does not exist. Renamed in place rather than dropped and
-- recreated, because recreating a check constraint means writing its definition
-- out again and a definition written twice drifts.
do $$
declare r record;
begin
  for r in
    select conname from pg_constraint
     where conrelid = to_regclass('public.log_book_entry')
       and conname like 'sighting%'
  loop
    execute format('alter table log_book_entry rename constraint %I to %I',
      r.conname, replace(r.conname, 'sighting', 'log_book_entry'));
  end loop;

  for r in
    select indexname from pg_indexes
     where tablename = 'log_book_entry' and indexname like 'sighting%'
  loop
    execute format('alter index %I rename to %I',
      r.indexname, replace(r.indexname, 'sighting', 'log_book_entry'));
  end loop;

  for r in
    select policyname from pg_policies
     where tablename = 'log_book_entry' and policyname like 'sighting%'
  loop
    execute format('alter policy %I on log_book_entry rename to %I',
      r.policyname, replace(r.policyname, 'sighting', 'log_book_entry'));
  end loop;
end
$$;

-- media referenced a sighting, and it still holds the same rows under the new
-- name. Renamed so that nothing anywhere still says sighting.
do $$
declare r record;
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'media'
       and column_name = 'sighting_id'
  ) and not exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'media'
       and column_name = 'log_book_entry_id'
  ) then
    alter table media rename column sighting_id to log_book_entry_id;
  end if;

  for r in
    select conname from pg_constraint
     where conrelid = to_regclass('public.media')
       and conname like '%sighting%'
  loop
    execute format('alter table media rename constraint %I to %I',
      r.conname, replace(r.conname, 'sighting', 'log_book_entry'));
  end loop;
end
$$;

-- ---------------------------------------------------------------------------
-- trail_waypoint, which was the one thing trail_log was not redundant about
--
-- A first pass at this migration checked trail_log's COLUMNS and concluded it held
-- nothing of its own. It did not: trail_waypoint held a foreign key to it. Every
-- column was duplicated and the table still had a dependent, which is the same
-- mistake as checking a function's locals and missing what it returns.
--
-- A waypoint is a point along a walk, and it belongs to the outing: outing.route
-- is the LineString those points make up. So the column is repointed rather than
-- the points dropped. Repointing is the honest choice because the table has a real
-- job — it is the only place a walk's breadcrumb can live, and a hike with no
-- track is a hike nobody can follow afterwards.
--
-- DROP ... CASCADE would have removed it silently, and the hint Postgres offers
-- for this exact case is the wrong suggestion to accept: the dependent is the
-- data, not the constraint.

do $$
declare r record;
begin
  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'trail_waypoint'
       and column_name = 'trail_log_id'
  ) and not exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'trail_waypoint'
       and column_name = 'outing_id'
  ) then
    alter table trail_waypoint rename column trail_log_id to outing_id;
  end if;

  for r in
    select conname from pg_constraint
     where conrelid = to_regclass('public.trail_waypoint')
       and conname like '%trail_log%'
  loop
    execute format('alter table trail_waypoint rename constraint %I to %I',
      r.conname, replace(r.conname, 'trail_log', 'outing'));
  end loop;

  -- The column still points at the old table, so the constraint has to be
  -- replaced rather than renamed.
  for r in
    select conname from pg_constraint
     where conrelid = to_regclass('public.trail_waypoint')
       and contype = 'f' and confrelid = to_regclass('public.trail_log')
  loop
    execute format('alter table trail_waypoint drop constraint %I', r.conname);
  end loop;

  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'trail_waypoint'
       and column_name = 'outing_id'
  ) then
    if not exists (
      select 1 from pg_constraint
       where conrelid = to_regclass('public.trail_waypoint')
         and conname = 'trail_waypoint_outing_id_fkey'
    ) then
      alter table trail_waypoint
        add constraint trail_waypoint_outing_id_fkey
        foreign key (outing_id) references outing(id);
    end if;
  end if;
end
$$;

-- media could point at a trail log as well as at an observation, and never did:
-- every media row has a null trail_log_id. The column is dropped rather than
-- repointed because there is nothing to point it at — a waypoint is not something
-- a photograph is of, and a walk is not an observation.
do $$
declare r record;
begin
  for r in
    select conname from pg_constraint
     where conrelid = to_regclass('public.media')
       and conname = 'media_trail_log_id_fkey'
  loop
    execute format('alter table media drop constraint %I', r.conname);
  end loop;

  if exists (
    select 1 from information_schema.columns
     where table_schema = 'public' and table_name = 'media'
       and column_name = 'trail_log_id'
  ) then
    alter table media drop column trail_log_id;
  end if;
end
$$;

-- Only now, with nothing left pointing at it.
do $$
begin
  if to_regclass('public.trail_log') is not null then
    drop table trail_log;
  end if;
end
$$;

-- ---------------------------------------------------------------------------
-- The constraint that went with trail_log, and what replaces it
--
-- 0003 gave media exactly one parent:
--
--   constraint media_has_one_parent check (
--     (sighting_id is not null)::int + (trail_log_id is not null)::int = 1)
--
-- Dropping the column dropped that constraint, silently, because a check that
-- mentions a column is owned by it. This is the one way a migration can lose a
-- rule without saying so: not a failed statement, just a rule that is no longer
-- being enforced. It was found by reading the constraint list afterwards rather
-- than by the tests, because the tests only notice what they assert.
--
-- The rule itself is obsolete rather than lost. The two subjects it chose between
-- were an observation and a walk, and the walk is now part of the outing — which
-- media already named, not null, for every row. So the outing is the subject in
-- the case the constraint used to call "a trail log", and what was genuinely
-- being enforced was that a media record pointed at one specific thing rather than
-- nothing or two things.
--
-- What is left worth enforcing is stronger and could not be expressed before: an
-- entry a media row names must belong to the outing that row names. That is a
-- comparison across two tables, which a check constraint cannot do at all, so it
-- is a trigger. Losing the old rule and failing to notice would have allowed a
-- photograph to be filed under an outing it was not taken on, and the row would
-- have looked entirely ordinary.

create or replace function media_entry_belongs_to_outing() returns trigger
language plpgsql as $$
begin
  if new.log_book_entry_id is null then
    return new;
  end if;
  if not exists (
    select 1 from log_book_entry e
     where e.id = new.log_book_entry_id
       and e.context_code = new.context_code
       and e.outing_id  = new.outing_id
  ) then
    raise exception
      'media names a log book entry that is not in this outing or context'
      using errcode = '23514';
  end if;
  return new;
end;
$$;

do $$
begin
  if to_regclass('public.media') is not null
     and not exists (
       select 1 from pg_trigger
        where tgrelid = to_regclass('public.media')
          and tgname = 'media_entry_belongs_to_outing'
     ) then
    create trigger media_entry_belongs_to_outing
      before insert or update on media
      for each row execute function media_entry_belongs_to_outing();
  end if;
end
$$;

comment on table trail_waypoint is
  'The ordered points of an outing''s track, for the walk or drive as it was '
  'actually made. This was a child of trail_log; it belongs to the outing, whose '
  'route is the line these points describe.';

comment on table log_book_entry is
  'One entry in a trainee''s log book: an observation, a note, or a reflection. '
  'Verification state lives here because field-service owns it — a mentor '
  'verifies from a Chisimba module, which calls in with a service token rather '
  'than a user''s.';

comment on column log_book_entry.correction_reason is
  'Required whenever recorded_species_code or recorded_count differs from what '
  'the trainee wrote. The correction is the lesson; without a reason it is an '
  'overwrite, and the trainee is left with a record that looks correct and a gap '
  'where their reasoning should have been.';

comment on column log_book_entry.status is
  'pending, verified, rejected or needs_review. needs_review is not a failure and '
  'not a pending in disguise: it records a mentor who examined the work and was '
  'not willing to commit to a judgement.';