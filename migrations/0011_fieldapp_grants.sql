-- 0011_fieldapp_grants.sql — extend the runtime role's grants to every table
-- the service now uses.
--
-- WHY THIS EXISTS. Migration 0002 created the `fieldapp` role and granted it
-- DML on `drive` and `sighting`. The schema has since grown: `drive` was split
-- into `outing` plus a per-kind detail table, `sighting` was renamed to
-- `log_book_entry`, and several supporting tables were added (media, change
-- feed, operation outcome, competencies, sign-offs, species, waypoints).
-- No migration revisited the grants, so `fieldapp` could not write to the new
-- tables: any push that touched `drive_detail`, `log_book_entry`,
-- `operation_outcome` or the change feed was refused by Postgres with
-- "permission denied for table …", which surfaced to the client as the
-- unhelpful "commit unexpectedly resulted in rollback".
--
-- The grant set here is the same shape as 0002 — DML on base tables, no DDL,
-- no sequence grants (the schema uses UUIDs, not sequences) — but it names
-- every table the current code can reach. When a future migration adds a
-- table, this list must grow with it.

grant select, insert, update, delete on outing                to fieldapp;
grant select, insert, update, delete on drive_detail          to fieldapp;
grant select, insert, update, delete on hike_detail           to fieldapp;
grant select, insert, update, delete on camp_detail           to fieldapp;
grant select, insert, update, delete on log_book_entry        to fieldapp;
grant select, insert              on media                    to fieldapp;
grant select, insert, update, delete on trail_waypoint        to fieldapp;
grant select, insert, update, delete on change_feed           to fieldapp;
grant select, insert, update, delete on operation_outcome     to fieldapp;
grant select                      on species                  to fieldapp;
grant select                      on competency               to fieldapp;
grant select, insert, update      on signoff                  to fieldapp;
grant select, insert, update, delete on signoff_competency    to fieldapp;

-- Reference tables (species, competency) are read-only for the runtime role:
-- they are seeded by migrations and replaced wholesale, never edited through
-- the service. Signoff is insert+update because a sign-off is created and then
-- reviewed; deletion is not part of any flow. Media has no update or delete
-- grant yet because the service has no media endpoints — the grant set will
-- grow when the flow does.
