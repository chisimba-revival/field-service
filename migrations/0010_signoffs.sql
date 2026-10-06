-- 0010_signoffs.sql — sign-offs, and the assessments they carry.
--
-- The contract specifies: `POST /api/v1/signoffs`, `POST /api/v1/signoffs/{id}/submit`,
-- `PATCH /api/v1/signoffs/{id}`, `PATCH /api/v1/signoffs/{id}/review` and
-- `GET /api/v1/trainees/{id}/signoffs`, over a sign-off carrying "one to twenty
-- assessments", each "a code, a rating of not_observed, developing, competent,
-- proficient or expert, and free-text evidence drawn from the drive".
--
-- ---------------------------------------------------------------------------
-- Two tables, because the array is not a column
--
-- The contract calls the assessments an array. It is not one: each assessment is
-- referenced by the competency catalogue's code, carries its own rating and its
-- own evidence, and is queried by competency across a trainee's history — which is
-- the whole point of retaining per outing and per competency rather than
-- collapsing to one grade. A jsonb column could hold the shape and would then
-- need a foreign key replaced by a convention, a rating vocabulary duplicated as
-- text, and a query per trainee that parses every row to answer "was this person
-- ever competent at navigation". So it is a table, and the array is the rows.
--
-- ---------------------------------------------------------------------------
-- The one-to-twenty bound is not a constraint here
--
-- It cannot be. A check constraint sees one row of signoff_competency and has no
-- way to count its siblings, and a trigger that counts on every insert would make
-- the bound true only at the instant the last row landed — so deleting a row would
-- have to re-check it too, or the invariant would be reachable by removal. The
-- bound is enforced by the service, in one function, before any row is written,
-- which is also where the whole submission is decided rather than half-applied.
-- This is recorded here so its absence reads as a decision and not an oversight.
--
-- ---------------------------------------------------------------------------
-- competency_code is a foreign key, and that is now safe
--
-- The catalogue retires rather than deletes precisely so this key can exist. A
-- foreign key to a table whose rows vanish is a key that eventually fails to
-- insert, and the workaround for that is to drop the key — which would orphan
-- every assessment recorded against a code the curriculum no longer offers.
-- on delete restrict is explicit for the same reason: a hard delete is refused
-- outright rather than allowed to cascade into someone's training history.
--
-- ---------------------------------------------------------------------------
-- Isolation
--
-- Both tables carry context_code with row-level security enabled AND forced, the
-- same as every other field-data table here, reading the grants the guard set for
-- the request. signoff_competency asks its parent for the context rather than
-- copying it, because a copied context_code can disagree with the parent and
-- nothing would say which is right.

create table if not exists signoff (
  id              uuid primary key,
  context_code    text   not null,
  -- Chisimba logical user identifiers, held as text like created_by elsewhere:
  -- a trainee is identified by the id Chisimba gives them and is never re-mapped
  -- to an internal number, so the record a mentor reads names the person they
  -- know rather than a row they have to look up.
  trainee_id      text   not null,
  mentor_id       text   not null,
  -- The evidence this rests on. Any outing kind: a walk teaches what a drive
  -- does not, and refusing that would quietly make the drive the only activity
  -- that can teach.
  outing_id       uuid   not null references outing (id),

  overall_comment text,

  status          text   not null default 'draft',
  submitted_at    timestamptz,
  reviewed_at     timestamptz,
  -- Derived from submitted_at by a configurable window and RECORDED here rather
  -- than recomputed on read, so a record cannot change its own deadline because
  -- the configuration did.
  expires_at      timestamptz,

  revision        integer not null default 1,
  created_by      text   not null,
  deleted_at      timestamptz,
  created_at      timestamptz not null default now(),
  updated_at      timestamptz not null default now(),

  constraint signoff_status_known check (status in
    ('draft', 'submitted', 'changes_requested', 'approved', 'rejected', 'expired')),

  -- A submission starts the review clock, and a review closes it. Neither can be
  -- half-present: a submitted record with no expiry would remain indefinitely
  -- actionable, which is exactly the staleness expiry exists to prevent.
  constraint signoff_clock_coherent check (
    (status = 'draft'
       and submitted_at is null
       and reviewed_at is null
       and expires_at is null)
    or (status in ('changes_requested', 'expired')
       and reviewed_at is not null
       and submitted_at is null
       and expires_at is null)
    or (status = 'submitted'
       and submitted_at is not null
       and expires_at is not null
       and reviewed_at is null)
    or (status in ('approved', 'rejected')
       and submitted_at is not null
       and reviewed_at is not null)
  ),

  -- One sign-off per trainee per outing. A second submission for the same outing
  -- is not a correction — it is a second record of the same assessment, and two
  -- would make "what did the mentor conclude on this drive" ambiguous. The
  -- revision cycle exists so a trainee revises and resubmits the same record
  -- rather than starting a new one.
  constraint signoff_one_per_trainee_per_outing
    unique (context_code, trainee_id, outing_id)
);

comment on table signoff is
  'A mentor''s assessment of named competencies against one outing. Retained per '
  'outing and per competency: never collapsed to one grade per trainee, because '
  'the gaps are the information that aims the next outing.';

comment on column signoff.status is
  'Editing is permitted only while draft or changes_requested, which is what '
  'makes "request changes" an actionable state rather than a dead end.';

comment on column signoff.expires_at is
  'Recorded at submission from the configured window, not recomputed on read, so '
  'a record cannot change its own deadline because the configuration did.';

create table if not exists signoff_competency (
  signoff_id     uuid not null references signoff (id) on delete cascade,
  -- The join key. See the note above on why this can safely be a foreign key.
  competency_code varchar(8) not null references competency (code) on delete restrict,
  -- Position in the mentor's list, so the order they assessed in survives and
  -- two attempts at the same sign-off produce the same rows in the same order.
  position       smallint not null,
  rating         text   not null,
  -- Mandatory even at not_observed, where it records why the competency could
  -- not be judged. A sign-off that can record a rating without evidence is a
  -- self-assessment, and the whole point of the record is that someone else can
  -- read the reasoning.
  evidence       text   not null,

  constraint signoff_competency_rating_known check (rating in
    ('not_observed', 'developing', 'competent', 'proficient', 'expert')),

  -- A rating is never a standing grade, so one competency appears once per
  -- sign-off and the history is read by joining across them. That is the
  -- unique index below rather than a check constraint: a check cannot name a
  -- pair of columns, and an earlier attempt at one wrote
  -- "check (signoff_id, competency_code) is not null", which is not valid SQL and
  -- would have been redundant anyway — signoff_id is NOT NULL with a foreign key
  -- and the index is what actually prevents a duplicate.
  constraint signoff_competency_pkey primary key (signoff_id, position),

  constraint signoff_competency_evidence_present check (length(btrim(evidence)) > 0)
);

create unique index if not exists signoff_competency_code_unique
  on signoff_competency (signoff_id, competency_code);

comment on table signoff_competency is
  'One assessment. The contract calls this an array; it is a table so the '
  'competency code can be a real foreign key and so a trainee''s history can be '
  'read by competency without parsing every row.';

comment on column signoff_competency.evidence is
  'Mandatory at every rating, including not_observed, where it records why the '
  'competency could not be judged.';

-- ---------------------------------------------------------------------------
-- Row-level security
--
-- The policies read the grants the guard set for this request, the same as every
-- other field-data table. signoff_competency resolves its context through its
-- parent rather than storing one.

alter table signoff enable row level security;
alter table signoff force row level security;

alter table signoff_competency enable row level security;
alter table signoff_competency force row level security;

drop policy if exists signoff_isolation on signoff;
create policy signoff_isolation on signoff
  using (
    context_code = any (current_setting('app.context_grants', true)::text[])
    and created_by = current_setting('app.caller_id', true)
  );

drop policy if exists signoff_competency_isolation on signoff_competency;
create policy signoff_competency_isolation on signoff_competency
  using (
    exists (
      select 1 from signoff s
      where s.id = signoff_competency.signoff_id
        and s.context_code = any (current_setting('app.context_grants', true)::text[])
        and s.created_by = current_setting('app.caller_id', true)
    )
  );

create index if not exists signoff_context_status on signoff (context_code, status);
create index if not exists signoff_context_trainee on signoff (context_code, trainee_id, submitted_at desc);
create index if not exists signoff_context_outing on signoff (context_code, outing_id);
create index if not exists signoff_competency_by_code on signoff_competency (competency_code);