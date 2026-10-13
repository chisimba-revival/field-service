-- 0015_planned_routes.sql — planned routes and their waypoints.
--
-- A planned route belongs to an outing and defines the path the guide intends
-- to follow. Waypoints are ordered vertices (vertices of the route geometry)
-- plus optional POIs (gates, waterholes, landmarks, custom).
--
-- The contract is local-first: the device plans the route, and the server may
-- or may not ever receive it. A planned route is not the same as a trail log
-- — a trail log records where the guide *actually* went; a planned route records
-- where they *intend* to go.
--
-- Isolation follows the same layers as every other context-bearing table
-- (0001_isolation, 0003_sync_and_trails, 0013_drive_and_trails): `context_code`
-- is a bare text column — there is no context table in the contract — and access
-- is enforced by row level security keyed off `app.context_grants`. The runtime
-- role `fieldapp` is granted DML so the push handler can reach the tables.

-- planned_routes table
create table if not exists planned_routes (
    id          uuid primary key,
    context_code text not null,
    outing_id   uuid not null references outing(id) on delete cascade,
    created_at  timestamptz not null default now(),
    updated_at  timestamptz not null default now(),
    revision    int not null default 1,
    is_tombstone boolean not null default false,
    deleted_at  timestamptz
);

-- Index for finding routes by outing
create index if not exists planned_routes_outing_idx on planned_routes(outing_id);
create index if not exists planned_routes_context_idx on planned_routes(context_code);

-- route_waypoints table
create table if not exists route_waypoints (
    route_id uuid not null references planned_routes(id) on delete cascade,
    ordinal  int not null,
    latitude double precision not null,
    longitude double precision not null,
    label text,
    kind text not null default 'vertex' check (kind in ('vertex','gate','waterhole','landmark','custom')),
    note text,
    revision int not null default 1,
    is_tombstone boolean not null default false,
    deleted_at timestamptz,
    primary key (route_id, ordinal)
);

-- Index for finding waypoints by route
create index if not exists route_waypoints_route_idx on route_waypoints(route_id);

-- Converge any database that applied an earlier draft of this migration, which
-- invented a foreign key to a `context` table the contract does not have. On a
-- fresh database the constraint was never created and this is a no-op.
alter table planned_routes drop constraint if exists planned_routes_context_code_fkey;

-- RLS: a caller only sees routes in the contexts they have been granted.
-- `force` rather than `enable` so the table owner is subject too, as elsewhere.
alter table planned_routes enable row level security;
alter table planned_routes force row level security;
alter table route_waypoints enable row level security;
alter table route_waypoints force row level security;

drop policy if exists planned_routes_in_granted_context on planned_routes;
create policy planned_routes_in_granted_context on planned_routes
  using (context_code = any (string_to_array(current_setting('app.context_grants'), ',')))
  with check (context_code = any (string_to_array(current_setting('app.context_grants'), ',')));

-- Waypoints carry no context_code of their own: they inherit the isolation of
-- the route they belong to, the same shape 0007 uses for the detail tables.
drop policy if exists route_waypoints_in_granted_context on route_waypoints;
create policy route_waypoints_in_granted_context on route_waypoints
  using (route_id in (
      select id from planned_routes
      where context_code = any (string_to_array(current_setting('app.context_grants'), ','))))
  with check (route_id in (
      select id from planned_routes
      where context_code = any (string_to_array(current_setting('app.context_grants'), ','))));

-- The runtime role needs DML on both tables; without it the push handler cannot
-- reach them at all.
grant select, insert, update, delete on planned_routes to fieldapp;
grant select, insert, update, delete on route_waypoints to fieldapp;

-- Trigger to update updated_at on planned_routes
create or replace function set_updated_at()
returns trigger language plpgsql as $$
begin
    new.updated_at = now();
    return new;
end $$;

drop trigger if exists planned_routes_updated_at on planned_routes;
create trigger planned_routes_updated_at
before update on planned_routes
for each row execute function set_updated_at();

-- Change-feed is handled by the push package's feed() function, which inserts
-- into change_feed. The tables are already covered by the existing change_feed
-- infrastructure (the feedTables map in pgstore.go).
