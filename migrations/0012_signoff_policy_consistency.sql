-- The signoff RLS policy cast current_setting directly to text[], which expects
-- a PostgreSQL array literal like {north,south}. Every other table's policy in
-- this service uses string_to_array(current_setting(...), ','), which expects a
-- comma-separated string like north,south. The two formats are incompatible, and
-- a session variable set to one refused the other with a malformed-literal error.
--
-- This migrates the signoff policy to the comma-separated form so one session
-- variable format serves every policy. The check text is unchanged.

drop policy if exists signoff_isolation on signoff;
create policy signoff_isolation on signoff
  using (
    context_code = any (string_to_array(current_setting('app.context_grants', true), ','))
    and created_by = current_setting('app.caller_id', true)
  );
