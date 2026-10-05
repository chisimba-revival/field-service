-- A Chisimba user id is not a uuid.
--
-- Rule 2 is explicit: "A user's identifier in the microservice is the Chisimba
-- user id, unchanged. No local surrogate keys, and no re-mapping." The gateway
-- returns user ids as strings -- /auth/me answers {"id":"1"} -- so typing this
-- column uuid could not hold the very value rule 2 requires. Nothing would have
-- caught it at the migration: Postgres accepted the declaration, and the first
-- push from a real device would have failed on the insert.
--
-- sighting.created_by is already text for the same reason. This column was the
-- odd one out, and the inconsistency is what made it look as though the
-- gateway's ids might be uuids after all.
--
-- The order below is not cosmetic. Postgres refuses to alter the type of a
-- column a policy depends on, so the policy is dropped first and recreated
-- afterwards. The policy text is identical; it is repeated rather than moved so
-- that this file is readable on its own.
drop policy if exists operation_outcome_own_only on operation_outcome;

alter table operation_outcome
  alter column caller_user_id type text using caller_user_id::text;

-- The primary key's key type changed with the column, and Postgres will not
-- convert an index's key type in place, so it is rebuilt rather than altered.
alter table operation_outcome drop constraint operation_outcome_pkey;
alter table operation_outcome
  add primary key (caller_user_id, operation_id);

create policy operation_outcome_own_only on operation_outcome
  using      (caller_user_id = current_setting('app.caller_user_id'))
  with check (caller_user_id = current_setting('app.caller_user_id'));
