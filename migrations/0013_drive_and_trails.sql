-- 0013_drive_and_trails.sql — the two logbooks, in the schema.
--
-- The drive logbook and the trail logbook change what the service must record
-- for a drive and for a hike, and add one record kind that has no home in the
-- schema so far: a dangerous-game encounter.
--
-- ---------------------------------------------------------------------------
-- Drive logbook
--
-- A game drive now records which vehicle carried the guests, whether the
-- pre-drive inspection (oil, water, tyres) was done, how long was daylight
-- driving and how long was night driving, and whether any of it was off-road.
-- Night driving is a separate qualification and a separate number of hours, so
-- the two are distinct columns rather than one duration with a flag: a drive
-- that ran 3 hours of daylight and 2 of night is two facts a report must show
-- separately, and one column plus a flag would force a reader to invent one of
-- them.
--
-- The inspection columns are booleans rather than timestamps or a single
-- "inspected" flag, because the trails programme counts each check separately
-- and "checked oil but not tyres" is a different record from "checked nothing".
-- They are nullable: a legacy drive predating this migration recorded none of
-- them, and the column allows absence for history while the newest records hold
-- the rule, exactly as 0007 treated duration and guest count.
--
-- ---------------------------------------------------------------------------
-- Trail logbook
--
-- A walking trail gains a guide role — lead or backup, the role a trainee
-- actually held — and free text for the rifle details (make, calibre, serial)
-- that the rifle_role column on its own does not carry. rifle_role says which
-- of the three positions held a rifle; rifle_details says what rifle it was.
-- The weather, tracked on the hike, stays on the outing where 0007 put a
-- weather column: it belongs to the outing, not to the detail table, and moving
-- it would make the two disagree.
--
-- A trail waypoint gains an elevation, because a trail logbook is read by its
-- track and elevation is what makes a track readable. It also gains a stable
-- id: the change feed addresses every other entity by a uuid entity_id, and a
-- waypoint keyed only by (outing_id, ordinal) could not be named in it. The
-- positional key stays the primary key — a waypoint's ordinal is the content,
-- not an attribute — and id is unique rather than primary.
--
-- ---------------------------------------------------------------------------
-- The dangerous-game encounter
--
-- An encounter with dangerous game is not a sighting. A sighting is an
-- observation for the logbook; an encounter is an incident, and the trails
-- programme records it separately with its own fields: the distance, what the
-- animal did, and what the party did about it. It is its own entity and its own
-- change-feed row, exactly as a log_book_entry is: it arrives by push, it is
-- read back by pull, and it hangs off the outing it happened during.
--
-- It also settles the trail_waypoint grant inconsistency. 0011 granted
-- update, delete on trail_waypoint and change_feed, which undid 0003's
-- deliberate narrowing — a waypoint's ordering is the content, and a position
-- that can be edited in place is a position that is no longer the record. That
-- grant is revoked here, so the narrow set 0003 chose is the set that stays.

-- ---------------------------------------------------------------------------
-- Drive detail: vehicle, inspection, day and night, off-road
alter table drive_detail add column if not exists vehicle_id text;
alter table drive_detail add column if not exists inspection_oil_ok   boolean;
alter table drive_detail add column if not exists inspection_water_ok boolean;
alter table drive_detail add column if not exists inspection_tyres_ok boolean;
alter table drive_detail add column if not exists daylight_hours numeric;
alter table drive_detail add column if not exists night_hours   numeric;
alter table drive_detail add column if not exists off_road_seconds integer;
alter table drive_detail add column if not exists off_track_used boolean;

-- Hours and seconds are non-negative; a negative duration is an impossible
-- drive, and the column is nullable for history so absence stays readable.
do $$
begin
  if not exists (select 1 from pg_constraint where conname = 'drive_detail_daylight_non_negative') then
    alter table drive_detail add constraint drive_detail_daylight_non_negative
      check (daylight_hours is null or daylight_hours >= 0);
  end if;
  if not exists (select 1 from pg_constraint where conname = 'drive_detail_night_non_negative') then
    alter table drive_detail add constraint drive_detail_night_non_negative
      check (night_hours is null or night_hours >= 0);
  end if;
  if not exists (select 1 from pg_constraint where conname = 'drive_detail_off_road_non_negative') then
    alter table drive_detail add constraint drive_detail_off_road_non_negative
      check (off_road_seconds is null or off_road_seconds >= 0);
  end if;
end $$;

comment on column drive_detail.vehicle_id is
  'The vehicle that carried the guests, by registration or fleet id. A drive '
  'with no vehicle is a drive that cannot be traced, which is not a drive it is '
  'useful to have recorded.';
comment on column drive_detail.daylight_hours is
  'Hours driven in daylight. Separate from night_hours because night driving is '
  'a different qualification: 3 daylight and 2 night are two facts, not one 5.';
comment on column drive_detail.night_hours is
  'Hours driven after dark (the programme cuts the day at 18:00, with a manual '
  'override on the device). Refused for a trainee without the night endorsement.';
comment on column drive_detail.off_road_seconds is
  'Time spent off the marked track, in seconds. Rolled up on the device from '
  'the waypoint track; the seconds keep a 90-second excursion distinct from a '
  '41-minute one at the same precision an integer hour count would lose.';
comment on column drive_detail.off_track_used is
  'Whether any of the drive ran off the marked track. A rollup of off_road_seconds '
  'for queries that only need the yes or no.';

-- ---------------------------------------------------------------------------
-- Hike detail: the guide role and the rifle details
alter table hike_detail add column if not exists guide_role text;
alter table hike_detail add column if not exists rifle_details text;

do $$
begin
  if not exists (select 1 from pg_constraint where conname = 'hike_detail_guide_role_known') then
    alter table hike_detail add constraint hike_detail_guide_role_known
      check (guide_role in ('lead', 'backup'));
  end if;
end $$;

comment on column hike_detail.guide_role is
  'Which post the trainee actually held on this trail: lead or backup. One of '
  'the two, or absent on a legacy trail that recorded neither.';
comment on column hike_detail.rifle_details is
  'The rifle carried: make, calibre, serial or fleet number. rifle_role says '
  'which post held a rifle; this says what rifle it was.';

-- ---------------------------------------------------------------------------
-- Trail waypoint: a stable id the change feed can name, and an elevation
alter table trail_waypoint add column if not exists id uuid;
alter table trail_waypoint add column if not exists elevation_m double precision;

-- The id is backfilled rather than defaulted: a default on the column would
-- hide a client that never sent one, and every waypoint must be nameable in the
-- feed. The update is guarded by the same predicate the column's new null
-- constraint asserts, so a re-run of this migration never overwrites an id.
update trail_waypoint set id = gen_random_uuid() where id is null;

alter table trail_waypoint alter column id set not null;
alter table trail_waypoint alter column id set default gen_random_uuid();

do $$
begin
  if not exists (select 1 from pg_constraint where conname = 'trail_waypoint_id_unique') then
    alter table trail_waypoint add constraint trail_waypoint_id_unique unique (id);
  end if;
end $$;

comment on column trail_waypoint.id is
  'Stable id the change feed can address. The primary key stays '
  '(outing_id, ordinal): a waypoint place in the sequence is the content, not '
  'an attribute.';

-- ---------------------------------------------------------------------------
-- Outing revisions
--
-- An outing is updated, not corrected: the drive's end — the duration, the
-- daylight and night hours, the off-road rollup — arrives as a second
-- operation against the revision the create returned, with the same
-- optimistic gate a correction uses. That needs a revision to bump, so the
-- outing gains one here, seeded at 0 for every row that predates the rule.
-- The create path writes it as 1, so the revision a create reports and the
-- revision the row holds are the same number from the start.
alter table outing add column if not exists revision bigint not null default 0;

do $$
begin
  if not exists (select 1 from pg_constraint where conname = 'outing_revision_non_negative') then
    alter table outing add constraint outing_revision_non_negative check (revision >= 0);
  end if;
end $$;

comment on column outing.revision is
  'The version a client read. An update names the revision it saw, and a change '
  'that arrives against an older one is refused, exactly as a correction is.';

-- ---------------------------------------------------------------------------
-- The dangerous-game encounter
create table if not exists dangerous_game_encounter (
  id                 uuid primary key,
  context_code       text not null,
  outing_id          uuid references outing(id),
  species_code       text not null,
  distance_m         double precision check (distance_m is null or distance_m >= 0),
  animal_behaviour   text,
  action_taken       text,
  location           geometry(Point, 4326) not null,
  captured_at        timestamptz not null,
  recorded_at        timestamptz not null default now(),
  created_by         text not null,
  revision           bigint not null default 0 check (revision >= 0),
  deleted_at         timestamptz,
  tombstone_revision bigint,
  note               text
);

comment on table dangerous_game_encounter is
  'An incident with dangerous game during an outing: the distance, what the '
  'animal did, and what the party did about it. An incident is not a sighting; '
  'it is recorded on its own and a mentor reads it on its own.';

-- The index a context-bound feed read and a per-outing listing both want, led
-- by context_code exactly as the other context-bound tables are.
create index if not exists dangerous_game_encounter_context_outing_revision
  on dangerous_game_encounter (context_code, outing_id, revision);

-- Encounters are located by where they happened, the same way sightings are.
create index if not exists dangerous_game_encounter_location_gix
  on dangerous_game_encounter using gist (location);

-- ---------------------------------------------------------------------------
-- Isolating the encounter, and keeping the waypoints append-only
--
-- The encounter carries its own context_code and gets the same isolation as
-- every other context-bound record in 0003: row-level security enabled *and*
-- forced, with a policy reading the grants the guard set for this request. It
-- could have asked its parent instead, as the detail tables do, but it is a
-- change-feed entity — its own row, its own revisions, its own feed entry —
-- and the change-feed entities all carry the code and the policy.
do $$
declare t text;
begin
  foreach t in array array['dangerous_game_encounter'] loop
    execute format('alter table %I enable row level security', t);
    execute format('alter table %I force row level security', t);
    execute format('drop policy if exists %I on %I', t || '_in_granted_context', t);
    execute format('create policy %I on %I using (context_code = any (string_to_array(current_setting(''app.context_grants''), '',''))) with check (context_code = any (string_to_array(current_setting(''app.context_grants''), '','')))',
                   t || '_in_granted_context', t);
  end loop;
end $$;

-- ---------------------------------------------------------------------------
-- The runtime role's grants
--
-- The encounter is created by push and read back by pull and feed, and 0011's
-- comment says plainly that the grant list must grow with every table the code
-- can reach. Update and delete are granted too because a correction of an
-- encounter is a future kind this schema is not closing the door on — the
-- append-only *sequence* rule applies to waypoints, not to incidents.
grant select, insert, update, delete on dangerous_game_encounter to fieldapp;

-- And the 0011 widening that the waypoint contract never sanctioned is undone.
-- 0003 granted select, insert on trail_waypoint and change_feed and then
-- revoked update, delete on the feed explicitly: appends are additive, the
-- sequence of waypoints is itself the record, and a position that can be
-- edited in place is a position that is no longer the record. 0011 re-granted
-- full DML on both when it swept the grant list; this migration gives the
-- runtime role back exactly the narrow set 0003 chose.
revoke update, delete on trail_waypoint from fieldapp;
revoke update, delete on change_feed    from fieldapp;