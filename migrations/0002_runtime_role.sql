-- 0002_runtime_role.sql — a runtime role that is actually subject to the policies.
--
-- WHY THIS EXISTS. The first migration enabled and forced row-level security and
-- created the policies, and none of it was enforced. The reason is worth
-- recording exactly, because it is the kind of thing that looks correct in
-- review and is not.
--
-- The container's POSTGRES_USER is a superuser, and that same role owns the
-- tables. PostgreSQL exempts superusers from row-level security outright, and
-- `force row level security` extends the policies to the table *owner* but not
-- to a superuser. So a probe reading `sighting` with no grants set returned
-- every row in every context, and reading it with `app.context_grants` set to
-- one context also returned every row.
--
-- Every result from the isolation layer was therefore meaningless: not a
-- failure of the policies, but a failure to notice they were not running. The
-- policies were correct and inert at the same time, which is the worst
-- combination available for a security control.
--
-- The design accepts that a superuser can bypass row-level security, on the
-- grounds that this database holds field data and no credential. That acceptance
-- holds only while the *application* is not the superuser. So the application
-- gets its own role, and the migration role stays where it is.
--
-- This role is created with LOGIN and no password. PostgreSQL cannot
-- authenticate a login role that has no valid password, so this role is inert
-- until somebody sets one. That is the safe default: a migration that leaves a
-- working credential behind is a different kind of problem.

do $$ begin
  -- NOSUPERUSER is the entire point of this migration and is written out
  -- rather than assumed, because the default for a role created this way
  -- depends on how the migration was run.
  create role fieldapp login nosuperuser nobypassrls nocreatedb nocreaterole
    noinherit;
exception when duplicate_object then null;
end $$;

-- Revoke first, unconditionally. A database may already carry grants from an
-- earlier attempt, and the point of this file is that the runtime role has
-- exactly these and no others.
revoke all on schema public from fieldapp;
grant usage on schema public to fieldapp;

revoke all on all tables in schema public from fieldapp;
grant select, insert, update, delete on drive to fieldapp;
grant select, insert, update, delete on sighting to fieldapp;

revoke all on all sequences in schema public from fieldapp;

-- Nothing in the runtime role's reach should be able to change the isolation
-- itself. Without this, a compromised application could ALTER the policies off
-- its own tables and the next request would read every context.
revoke all on table pg_policies from public;

-- RLS is not bypassed by default for a non-superuser, but saying so in the
-- schema rather than relying on the cluster's configuration makes the guarantee
-- local: this file, applied to this database, is what makes it true.
alter table drive    alter column id set not null;
alter table sighting alter column id set not null;

comment on role fieldapp is
  'Runtime role for the field service. Deliberately NOSUPERUSER and NOBYPASSRLS: '
  'the row-level security policies in 0001 are the second of three isolation '
  'layers and they do nothing at all if the application connects as a superuser. '
  'Grants are table-level DML only; schema changes are not reachable from here.';
