-- 0009_competencies.sql — the competency catalogue.
--
-- The contract asks for `GET /api/v1/competencies`, "list by category and level",
-- and requires that a competency assessment carry "a code, a rating of
-- not_observed, developing, competent, proficient or expert, and free-text
-- evidence". A sign-off is an array of one to twenty of those.
--
-- ---------------------------------------------------------------------------
-- This catalogue is a starting point, and it is expected to be edited
--
-- No competency list existed when this was written: the contract specifies the
-- shape of an assessment but names no competency, and there was no table. So the
-- rows below are a proposal, not an authority, and they are seeded the same way
-- the species were — idempotently, so re-running this migration will not
-- overwrite an edit made through the service.
--
-- Two consequences of expecting that editing, and they are the reason for most
-- of the design here:
--
-- Codes are stable. A code is the join key between a historical assessment and
-- whatever the row says today. Renaming a competency's name or description is
-- free; changing its code orphans every assessment that cites the old one. So
-- `code` is short and fixed-width like the species codes, and it is not derived
-- from the name — a competency called "Navigation" that is later renamed
-- "Orienteering" keeps its code, because the alternative silently rewrites
-- history.
--
-- The width is eight, not the four the species codes use, because these codes
-- carry a category prefix and two of them are seven characters. That was found by
-- the constraint rather than by reading the seed: char(6) was written first and
-- the first run failed on NAVPATH, which is the correct outcome for a constraint
-- doing its job.
--
-- Retirement is not deletion. `retired_at` exists because a competency drops out
-- of a curriculum long before anyone wants its assessments to stop resolving. A
-- hard delete would leave every past sign-off citing a code the catalogue no
-- longer knows, and the history would read as though that competency had never
-- been assessed. A retired competency stays readable, is not offered for new
-- assessments, and is what the catalogue returns by default so a client can tell
-- a trainee "you were assessed on this" rather than showing them a gap.
--
-- ---------------------------------------------------------------------------
-- Reference data, so no context_code and no row-level security
--
-- The same reasoning as the species catalogue: a competency is not an
-- observation, there is nothing to isolate, and a per-context catalogue would
-- mean a device caching one copy per context.
--
-- signoff_competency is NOT a foreign key to this table yet, because the
-- sign-off table does not exist yet either. When it is built, that relationship
-- should be a foreign key to `code` — the reason this table has retirement
-- rather than deletion is precisely so that such a foreign key stays safe. A
-- foreign key to a table whose rows can vanish is a foreign key that will
-- eventually fail to insert.

create table if not exists competency (
  -- varchar and not char, deliberately. A char(n) column pads with trailing
  -- spaces to the full width, and Postgres applies a regex CHECK to the padded
  -- value, so char(8) makes a five-character code fail its own constraint
  -- because of the padding it was given. The species table only escapes this
  -- because every species code is exactly four characters and fills char(4)
  -- exactly. A fixed-width code buys nothing here, because the check constraint
  -- already fixes the shape and a padded value would compare unequal to its own
  -- contents anyway.
  code        varchar(8) primary key,
  name        text     not null,
  category    text     not null,
  level       smallint not null,
  description text,

  -- When the competency left the curriculum. NULL means it is current. Nothing
  -- ever hard-deletes from this table; see the note above.
  retired_at  timestamptz,

  constraint competency_code_well_formed check (code ~ '^[A-Z]{4,8}$'),
  constraint competency_level_in_range check (level between 1 and 5),
  constraint competency_name_unique unique (name)
);

comment on table competency is
  'Reference data. Retired rather than deleted so historical assessments keep '
  'resolving. Codes are the join key and must stay stable across renames.';

comment on column competency.level is
  'The stage of the scheme at which this competency is normally signed off, 1 '
  'to 5. This is a property of the competency and not the per-trainee rating; '
  'the rating is on the assessment. It is here so the catalogue can be listed '
  '"by category and level" and so a sign-off can be checked against what a '
  'mentor is expected to be assessing.';

comment on column competency.retired_at is
  'Set when the competency leaves the curriculum. Retired rows remain readable '
  'and are returned by default, because an assessment recorded against them is '
  'still a real assessment. Nothing deletes from this table.';

-- ---------------------------------------------------------------------------
-- The catalogue
--
-- These are the categories a field guiding programme around a big-five reserve
-- actually teaches, and they are ordered from least to most about the job
-- itself: a guide who can drive but cannot interpret has not been trained.
--
-- Level is the stage at which the competency is normally signed off. A level-1
-- competency is one a trainee should hold before being left alone with a
-- vehicle; a level-5 one is a thing a trainee is not signed off on until they
-- are guiding.
insert into competency (code, name, category, level, description) values
  ('VEHCK', 'Vehicle daily check', 'vehicle', 1,
   'Walk-round check before the first guest boards: tyres, water, fuel, oil, spare wheel, tools. A vehicle that leaves the yard with a fault is found out about in the field, where there is no yard.'),
  ('VEHDV', 'Safe driving on reserve track', 'vehicle', 2,
   'Keeping speed appropriate to the surface, giving elephants and buffalo the room they need, and reversing a trailer without looking back. Most reserve incidents happen at a speed the terrain did not allow.'),
  ('NAVMAP', 'Map and compass', 'navigation', 2,
   'Reading a topographic sheet and a compass, and knowing which way is north when neither is to hand. A GPS is a convenience and not a substitute.'),
  ('NAVPATH', 'Steering without instruments', 'navigation', 3,
   'Navigating by sun, by vegetation line and by terrain, and admitting when the position is not known. A guide who guesses quietly is worse than one who says so.'),
  ('TRKSPR', 'Reading spoor and sign', 'tracking', 2,
   'Recognising spoor, feeding sign, scratch and call, and telling a track made this morning from one made last week.'),
  ('TRKNOW', 'Presence without sighting', 'tracking', 3,
   'Concluding from sign, sound and smell that an animal is close or has moved off, and acting on that conclusion rather than needing to see it.'),
  ('SAFFIR', 'Firearm handling', 'safety', 1,
   'Loading, unloading and carrying a rifle safely, and treating every weapon as loaded. A trainee who has not been drilled on this does not carry one.'),
  ('SAFBIG', 'Behaviour of dangerous animals', 'safety', 2,
   'Reading elephant, buffalo and lion behaviour, knowing the distance at which to stop the vehicle, and recognising the difference between curiosity and a charge.'),
  ('SAFFA', 'First aid and incident response', 'safety', 3,
   'First response to injury and to the incidents a reserve actually produces: a snared wire, a heat casualty, a vehicle in a ditch. Knowing who to call and what to say to them.'),
  ('CAMPSE', 'Setting up camp', 'campcraft', 2,
   'Selecting a site, pitching, water discipline, and leaving it with no trace. Camping is a place and a duration, not a route, and the site choice is the whole judgement.'),
  ('INTNAT', 'Natural history knowledge', 'interpretation', 2,
   'The species of the reserve: what they eat, how they move, when they are active, and what their presence means for the ground they are on.'),
  ('INTCLI', 'Explaining to guests', 'interpretation', 3,
   'Turning an observation into something a guest understands without dumbing it down, and answering the question they were not brave enough to ask.'),
  ('CLIBRF', 'Briefing guests', 'client', 1,
   'What the day involves, what the rules are, what to do if something goes wrong. Most guest complaints are a briefing that was skipped.'),
  ('CLIMNG', 'Managing a group', 'client', 2,
   'Keeping a vehicle of people with different expectations together and moving, and holding a quiet vehicle quiet.'),
  ('CONECO', 'Minimum-impact guiding', 'conservation', 2,
   'Keeping vehicles on the track, keeping distance from animals that have not asked for company, and explaining why. The guiding is the tourism.'),
  ('DATREC', 'Recording field data', 'conservation', 1,
   'Writing a log book entry a mentor can act on: what was seen, how many, where, and what was not recorded and why. A number with no evidence cannot be corrected, only argued with.')
on conflict (code) do nothing;