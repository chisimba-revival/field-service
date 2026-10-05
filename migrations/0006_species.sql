-- 0006_species.sql — the species catalogue.
--
-- The contract asks for `GET /api/v1/species` and `GET /api/v1/species/{code}`,
-- and says the catalogues "are served from the microservice rather than from
-- Chisimba, because they belong with the field data and must be bundled into
-- the offline cache without a second round trip." So this is the table behind
-- that endpoint, and it lives here for the stated reason rather than by
-- convenience.
--
-- ---------------------------------------------------------------------------
-- No context_code, and no row-level security
--
-- Every field-data table here carries a context_code and an RLS policy. This one
-- deliberately does not, and the reason is worth stating because it looks like
-- an omission: species are reference data, not observations. A lion is a lion in
-- every reserve, so there is nothing to isolate. Giving the catalogue a
-- context_code would also mean a device caches one copy per context, which
-- defeats the offline bundle the contract asks for.
--
-- The consequence is that this table is readable by anyone who can reach the
-- service, and that is intended. It is also why it carries nothing sensitive:
-- no counts, no locations, no rare-species sightings.
--
-- ---------------------------------------------------------------------------
-- sighting.species_code is NOT a foreign key to this table
--
-- That is a decision, not an oversight, and it should stay a decision rather
-- than drifting into an accident.
--
-- A foreign key would be the obvious thing, and it was considered. It was
-- rejected because it would refuse any sighting naming a code this installation
-- does not carry, and a reserve recording an animal the catalogue has not been
-- told about yet is a normal event, not a client error. Refusing it would mean
-- losing the observation to protect the catalogue's tidiness.
--
-- The cost is real and is recorded here rather than discovered later: a typo in
-- a species code still lands, and it lands unsearchable, because nothing joins
-- it to a row here. "LEOP" and "LEOP " are different codes and only one of them
-- is a lion. A catalogue that is advisory can be wrong about the data without
-- anything noticing.
--
-- The `code` column below is nonetheless constrained, because a malformed code
-- in the catalogue would break every join a client makes against it. That is a
-- different question from whether a sighting may name an unknown code.

create table if not exists species (
  -- The contract fixes the shape: "four uppercase letters". Enforced here so
  -- the catalogue cannot drift away from the format clients are told to send.
  code            char(4)    primary key,
  common_name     text       not null,
  scientific_name text,
  description     text,

  constraint species_code_well_formed check (code ~ '^[A-Z]{4}$')
);

comment on table species is
  'Reference data, shared by every context. Deliberately unconstrained by '
  'sighting.species_code, which may name a code absent from here.';

comment on column species.code is
  'Four uppercase letters, per the service contract. LEOP and LION were already '
  'in use before this table existed and keep their codes; the rest were '
  'assigned alongside them.';

-- ---------------------------------------------------------------------------
-- The catalogue
--
-- These are the animals the reserve is known for. Two of them are not one
-- animal each: "rhinoceros" is white (Ceratotherium simum) and black (Diceros
-- bicornis), which are separate species, graze and browse differently, and are
-- told apart in the field by lip shape. One code for both would carry a
-- description that contradicts itself, and a mentor recording a black rhino
-- browse would have nothing to record it against.
--
-- The descriptions are the reserve's own wording and are stored as given.
-- Re-running this migration will not overwrite them, so an edit made through
-- the service is not undone by a later migration; it would take a deliberate
-- statement to change one.
insert into species (code, common_name, scientific_name, description) values
  ('LION', 'Lion', 'Panthera leo',
   'Highly social big cats that live in family groups called prides. Their powerful roars can carry up to 5 miles (8 km) across the savannah.'),
  ('LEOP', 'Leopard', 'Panthera pardus',
   'Solitary and nocturnal master stalkers. They use their spotted coats for camouflage and often hoist their kills high up into trees.'),
  ('ELEP', 'African Elephant', 'Loxodonta africana',
   'The largest land mammal on Earth. These intelligent, highly social animals travel in herds led by experienced matriarchs.'),
  ('WHRI', 'White Rhinoceros', 'Ceratotherium simum',
   'Massive herbivores with keratin horns. White rhinos have wide, flat lips for grazing grass.'),
  ('BLRI', 'Black Rhinoceros', 'Diceros bicornis',
   'Massive herbivores with keratin horns. Black rhinos have pointed lips for browsing on bushes and trees.'),
  ('BUFA', 'Cape Buffalo', 'Syncerus caffer',
   'Widely considered the most dangerous of the group. They form large, unpredictable herds and fiercely protect each other from predators')
on conflict (code) do nothing;