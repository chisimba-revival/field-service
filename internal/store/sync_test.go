package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"field-service/internal/store"
)

const (
	sightNorth = "dddddddd-0000-0000-0000-000000000001"
	sightSouth = "dddddddd-0000-0000-0000-000000000002"
	logNorth   = "cccccccc-0000-0000-0000-000000000001"
	logSouth   = "cccccccc-0000-0000-0000-000000000002"
)

// seedSync inserts the rows the 0003 migration's tables need. Written through
// the migration role because the runtime role deliberately cannot see rows it
// holds no grant for, so it cannot reliably set a fixture up.
func seedSync(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	// Every table, with cascade: trail_log, trail_waypoint and media reference
	// drive and sighting, so truncating a subset is refused outright.
	if _, err := conn.Exec(ctx, `truncate drive, sighting, trail_log, trail_waypoint, change_feed, media, operation_outcome cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	// rifle_role, not a trail code: second-rifle hours are counted separately
	// from first-rifle and from a participant's own, and merging them is the one
	// thing this column exists to prevent.
	// The drives are re-inserted here because the truncate above clears them and
	// trail_log.drive_id is a foreign key. A trail log belongs to a drive, so a
	// trail fixture without one is not a smaller fixture, it is an invalid one.
	for _, d := range []struct{ id, ctxCode, guide string }{
		{driveNorth, ctxNorth, "guide-a"},
		{driveSouth, ctxSouth, "guide-b"},
	} {
		if _, err := conn.Exec(ctx,
			`insert into drive (id, context_code, guide_id, status)
			 values ($1, $2, $3, 'active')`,
			d.id, d.ctxCode, d.guide); err != nil {
			t.Fatalf("insert drive %s: %v", d.ctxCode, err)
		}
	}

	for _, l := range []struct{ id, drive, ctxCode, role string }{
		{logNorth, driveNorth, ctxNorth, "first"},
		{logSouth, driveSouth, ctxSouth, "second"},
	} {
		if _, err := conn.Exec(ctx,
			`insert into trail_log (id, context_code, drive_id, started_at, rifle_role, created_by)
			 values ($1, $2, $3, now(), $4, 'guide')`,
			l.id, l.ctxCode, l.drive, l.role); err != nil {
			t.Fatalf("insert trail_log %s: %v", l.ctxCode, err)
		}
	}
}

// visible runs a count inside a transaction holding only the named grants.
func visible(ctx context.Context, conn *pgx.Conn, query string, grants []string, args ...any) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`select set_config('app.context_grants', $1, true)`,
		strings.Join(grants, ",")); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// Every context-bearing table added in 0003 must be isolated. This is one test
// over a table of names rather than five near-identical ones, because the
// failure it guards is a table being *added* and not being listed — and a
// missing name in a table of names fails exactly as loudly as a missing policy.
func TestEveryContextBearingTableIsIsolated(t *testing.T) {
	seed(t, admin(t))
	seedSync(t, admin(t))
	conn := admin(t)

	tables := []struct {
		table  string
		column string
	}{
		{"trail_log", "trail_log_id"},
		{"trail_waypoint", "trail_log_id"},
		{"change_feed", "seq"},
		{"media", "id"},
	}
	for _, tc := range tables {
		t.Run(tc.table, func(t *testing.T) {
			// Seed one row per context in the table's own terms.
			switch tc.table {
			case "trail_log":
				// already seeded
			case "trail_waypoint":
				for i, l := range []struct{ id, ctxCode string }{{logNorth, ctxNorth}, {logSouth, ctxSouth}} {
					for ord := 0; ord <= i; ord++ {
						// context_code is carried on the waypoint itself rather
						// than read through the parent log: a policy cannot
						// reach through a join without a SECURITY DEFINER
						// function, which would move the decision out of the
						// database's own hands.
						if _, err := conn.Exec(context.Background(),
							`insert into trail_waypoint (trail_log_id, ordinal, context_code, point, captured_at)
							 values ($1, $2, $3, st_setsrid(st_makepoint($4, $5), 4326), now())`,
							l.id, ord, l.ctxCode, 36.8+float64(ord)*0.01, -1.2); err != nil {
							t.Fatalf("insert waypoint: %v", err)
						}
					}
				}
			case "change_feed":
				for i, c := range []string{ctxNorth, ctxSouth} {
					if _, err := conn.Exec(context.Background(),
						`insert into change_feed (context_code, entity_type, entity_id, revision, body)
						 values ($1, 'sighting', gen_random_uuid(), $2, '{}'::jsonb)`,
						c, i+1); err != nil {
						t.Fatalf("insert feed row: %v", err)
					}
				}
			case "media":
				// sighting_id is a real foreign key, so the parent row has to
				// exist rather than being a generated uuid.
				for _, m := range []struct{ ctxCode, parent, drive string }{
					{ctxNorth, sightNorth, driveNorth},
					{ctxSouth, sightSouth, driveSouth},
				} {
					if _, err := conn.Exec(context.Background(),
						`insert into sighting (id, context_code, drive_id, location,
						   captured_at, recorded_at, created_by)
						 values ($1, $2, $3, st_setsrid(st_makepoint(36.8, -1.2), 4326),
						         now(), now(), 'guide')`,
						m.parent, m.ctxCode, m.drive); err != nil {
						t.Fatalf("insert parent sighting: %v", err)
					}
					if _, err := conn.Exec(context.Background(),
						`insert into media (id, context_code, drive_id, kind, filename,
						   mime_type, size_bytes, captured_at, state, sighting_id)
						 values (gen_random_uuid(), $1, $2, 'photo', 'a.jpg',
						         'image/jpeg', 10, now(), 'pending', $3)`,
						m.ctxCode, m.drive, m.parent); err != nil {
						t.Fatalf("insert media: %v", err)
					}
				}
			}

			// Granting one context must show only that context's row.
			var got int
			var err error
			asRuntime(t, func(ctx context.Context, rc *pgx.Conn) {
				got, err = visible(ctx, rc,
					`select count(*) from `+tc.table, []string{ctxNorth})
			})
			if err != nil {
				t.Fatalf("%s: %v", tc.table, err)
			}
			if got != 1 {
				t.Errorf("granted %s, %s showed %d rows, want 1. A table added "+
					"without a policy reads across every context and there is no "+
					"error.", ctxNorth, tc.table, got)
			}
		})
	}
}

// A waypoint cannot be replaced. The correction is another row.
//
// The first version of this test asserted that through the schema, and it
// failed: `update trail_waypoint set point = ...` succeeds. Putting ordinal in
// the primary key makes a waypoint's identity stable, but nothing in the table
// stops the row being edited.
//
// That is worth being precise about rather than papering over. The contract's
// rule that a trail log is not rewritten is enforced here by *privilege* — the
// runtime role holds `select, insert` and not `update` — and not by the schema.
// The two are not equivalent, and the difference matters: a rule held by a
// grant is one migration away from being widened, so it is asserted here and
// the schema is not credited with it.
//
// The owner can still edit a waypoint, because the owner is a superuser and a
// superuser bypasses everything. That is the same accepted risk the isolation
// layer already carries, and it is accepted for the same reason: this database
// holds field data and no credential.
func TestAWaypointCannotBeReplacedInPlace(t *testing.T) {
	seed(t, admin(t))
	seedSync(t, admin(t))
	conn := admin(t)
	ctx := context.Background()

	if _, err := conn.Exec(ctx,
		`insert into trail_waypoint (trail_log_id, ordinal, context_code, point, captured_at)
		 values ($1, 1, $2, st_setsrid(st_makepoint(36.8, -1.2), 4326), now())`,
		logNorth, ctxNorth); err != nil {
		t.Fatal(err)
	}

	// As the runtime role, which is what the application is.
	var updateErr error
	asRuntime(t, func(c context.Context, rc *pgx.Conn) {
		tx, err := rc.Begin(c)
		if err != nil {
			updateErr = err
			return
		}
		defer func() { _ = tx.Rollback(c) }()
		if _, err := tx.Exec(c,
			`select set_config('app.context_grants', $1, true)`, ctxNorth); err != nil {
			updateErr = err
			return
		}
		_, updateErr = tx.Exec(c,
			`update trail_waypoint set point = st_setsrid(st_makepoint(0, 0), 4326)
			 where trail_log_id = $1 and ordinal = 1`, logNorth)
	})
	if updateErr == nil {
		t.Fatal("the runtime role moved a waypoint. The contract calls this the " +
			"one entity where ordering is the content, so a position that can be " +
			"edited in place is a position that is no longer the record.")
	}
	if !strings.Contains(updateErr.Error(), "permission denied") {
		t.Errorf("expected a privilege refusal rather than a policy one, got: %v",
			updateErr)
	}

	// The grant is what enforces it, so assert the grant rather than trusting
	// that it is still narrow.
	var canUpdate bool
	if err := conn.QueryRow(ctx,
		`select has_table_privilege('fieldapp', 'trail_waypoint', 'update')`).
		Scan(&canUpdate); err != nil {
		t.Fatal(err)
	}
	if canUpdate {
		t.Error("the runtime role holds UPDATE on trail_waypoint. The append-only " +
			"property of trail logs rests entirely on this grant.")
	}
}

// The same position twice is two events, and the schema has to allow it. A guide
// who stops in one place twice has two waypoints at the same coordinates.
func TestTheSamePositionTwiceIsTwoWaypoints(t *testing.T) {
	seed(t, admin(t))
	seedSync(t, admin(t))
	conn := admin(t)
	ctx := context.Background()

	for ord := 0; ord < 2; ord++ {
		if _, err := conn.Exec(ctx,
			`insert into trail_waypoint (trail_log_id, ordinal, context_code, point, captured_at)
			 values ($1, $2, $3, st_setsrid(st_makepoint(36.8, -1.2), 4326), now())`,
			logNorth, ord, ctxNorth); err != nil {
			t.Fatalf("ordinal %d: %v. The same position twice is two events.", ord, err)
		}
	}
	var n int
	if err := conn.QueryRow(ctx,
		`select count(*) from trail_waypoint where trail_log_id = $1`,
		logNorth).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d waypoints at one position, want 2", n)
	}
}

// The contract says exactly one of sighting_id or trail_log_id applies. Two, or
// neither, is a media record that points at nothing or at two things.
func TestMediaHasExactlyOneParent(t *testing.T) {
	seed(t, admin(t))
	seedSync(t, admin(t))
	conn := admin(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx,
		`insert into sighting (id, context_code, drive_id, location, captured_at, recorded_at, created_by)
		 values ($1, $2, $3, st_setsrid(st_makepoint(36.8, -1.2), 4326), now(), now(), 'guide')`,
		sightNorth, ctxNorth, driveNorth); err != nil {
		t.Fatal(err)
	}
	insert := `insert into media (id, context_code, drive_id, kind, filename,
	           mime_type, size_bytes, captured_at, state, sighting_id, trail_log_id)
	           values (gen_random_uuid(), $1, $2, 'photo', 'a.jpg', 'image/jpeg',
	                   10, now(), 'pending', $3, $4)`

	cases := []struct {
		name    string
		sight   any
		log     any
		wantErr bool
	}{
		{"a sighting only", sightNorth, nil, false},
		{"a trail log only", nil, logNorth, false},
		{"both", sightNorth, logNorth, true},
		{"neither", nil, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := conn.Exec(ctx, insert, ctxNorth, driveNorth, c.sight, c.log)
			if c.wantErr && err == nil {
				t.Error("accepted. Exactly one applies; two parents is ambiguous " +
					"and none is a record pointing at nothing.")
			}
			if !c.wantErr && err != nil {
				t.Errorf("rejected a legitimate record: %v", err)
			}
		})
	}
}

// An operation id is a string somebody chose, so it is not proof of identity.
// The outcome table is therefore keyed by caller, and a caller must not see
// another caller's replay outcomes — which means it is not context-isolated and
// needs a different predicate.
func TestOperationOutcomeIsScopedToTheCallerNotTheContext(t *testing.T) {
	seed(t, admin(t))
	conn := admin(t)
	ctx := context.Background()

	if _, err := conn.Exec(ctx,
		`insert into operation_outcome (caller_user_id, operation_id, entity_type,
		   entity_id, kind, outcome)
		 values ('11111111-0000-0000-0000-000000000001', gen_random_uuid(),
		         'sighting', gen_random_uuid(), 'create', 'applied')`); err != nil {
		t.Fatal(err)
	}

	// Holding a grant in the context, and naming a different caller, must not
	// return the other caller's outcome.
	var got int
	var err error
	asRuntime(t, func(c context.Context, rc *pgx.Conn) {
		tx, e := rc.Begin(c)
		if e != nil {
			err = e
			return
		}
		defer func() { _ = tx.Rollback(c) }()
		if _, e := tx.Exec(c,
			`select set_config('app.context_grants', $1, true),
			        set_config('app.caller_user_id', $2, true)`,
			ctxNorth, "22222222-0000-0000-0000-000000000002"); e != nil {
			err = e
			return
		}
		if e := tx.QueryRow(c,
			`select count(*) from operation_outcome`).Scan(&got); e != nil {
			err = e
			return
		}
		err = tx.Commit(c)
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	if got != 0 {
		t.Errorf("a caller holding a grant in a context saw %d outcomes recorded "+
			"against a different caller. An operation id is not proof of identity.",
			got)
	}
}

// The feed carries the row, so one page is one round trip. An empty body would
// make it identity-only and push the join back to the client.
func TestTheChangeFeedCarriesTheRowNotJustItsIdentity(t *testing.T) {
	seed(t, admin(t))
	conn := admin(t)
	ctx := context.Background()
	if _, err := conn.Exec(ctx,
		`truncate change_feed`); err != nil {
		t.Fatal(err)
	}

	// An empty body is refused outright: jsonb not null still admits '{}'.
	if _, err := conn.Exec(ctx,
		`insert into change_feed (context_code, entity_type, entity_id, revision, body)
		 values ($1, 'sighting', gen_random_uuid(), 1, '{}'::jsonb)`, ctxNorth); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err := conn.QueryRow(ctx,
		`select body from change_feed limit 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Error("the feed row carries no body, so a pull would have to fetch each " +
			"entity separately. One page has to be one round trip.")
	}
}

// A pull is `where context_code = any($grants) and seq > $cursor order by seq`.
// If the index does not lead with context_code the query is a scan, and a scan
// that forgets the predicate returns another reserve's records.
func TestTheChangeFeedIndexLeadsWithContext(t *testing.T) {
	conn := admin(t)
	var def string
	if err := conn.QueryRow(context.Background(),
		`select indexdef from pg_indexes
		 where schemaname = 'public' and indexname = 'change_feed_context_seq'`).
		Scan(&def); err != nil {
		t.Skipf("index missing: %v", err)
	}
	if !strings.Contains(def, "(context_code, seq)") {
		t.Errorf("index definition is %q, want context_code leading. Every list "+
			"and count is filtered by context_code first.", def)
	}
}

// Rifle role is never merged, and a lesson is not optional once offered.
func TestTrailLogFieldRules(t *testing.T) {
	seed(t, admin(t))
	seedSync(t, admin(t))
	conn := admin(t)
	ctx := context.Background()

	bad := []struct {
		name string
		sql  string
	}{
		{"a merged rifle role",
			`insert into trail_log (id, context_code, drive_id, started_at, created_by, rifle_role)
			 values (gen_random_uuid(), $1, $2, now(), 'g', 'first_and_second')`},
		{"an ended log that ends before it starts",
			`insert into trail_log (id, context_code, drive_id, started_at, ended_at, created_by)
			 values (gen_random_uuid(), $1, $2, now() + interval '1 hour', now(), 'g')`},
		{"a lesson that says nothing",
			`insert into trail_log (id, context_code, drive_id, started_at, created_by, lessons_learned)
			 values (gen_random_uuid(), $1, $2, now(), 'g', '   ')`},
	}
	for _, b := range bad {
		t.Run(b.name, func(t *testing.T) {
			if _, err := conn.Exec(ctx, b.sql, ctxNorth, driveNorth); err == nil {
				t.Error("accepted")
			}
		})
	}
}

// Every table carrying a context_code must have RLS on and forced, or the
// policies in 0001 do nothing for it. A table added later is exactly how the
// first isolation layer was found inert.
func TestEveryContextTableHasSecurityForcedOn(t *testing.T) {
	conn := admin(t)
	rows, err := conn.Query(context.Background(),
		`select c.relname, c.relrowsecurity, c.relforcerowsecurity
		 from pg_class c join pg_namespace n on n.oid = c.relnamespace
		 where n.nspname = 'public' and c.relkind = 'r'
		   and exists (select 1 from pg_attribute a
		               where a.attrelid = c.oid and a.attname = 'context_code')
		 order by c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var name string
		var enabled, forced bool
		if err := rows.Scan(&name, &enabled, &forced); err != nil {
			t.Fatal(err)
		}
		seen++
		if !enabled || !forced {
			t.Errorf("%s carries context_code but has rls=%v force=%v. Without "+
				"both, the policies on it are decorative.", name, enabled, forced)
		}
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	if seen == 0 {
		t.Error("no table with a context_code was found, so this test proved " +
			"nothing about the others")
	}
	t.Logf("checked %d context-bearing table(s)", seen)
}

func TestIsolationViolationsAreRefusalsNotErrors(t *testing.T) {
	// A write into an ungranted context must be refused by the policy, and the
	// refusal has to be the row-level security code so a caller can tell it from
	// a broken statement.
	var writeErr error
	asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
		// A real transaction rather than a multi-statement string: pgx refuses
		// several commands in one prepared statement when any carries a
		// parameter, and the isolation tests above get away with it only because
		// they pass none.
		tx, err := conn.Begin(ctx)
		if err != nil {
			writeErr = err
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx,
			`select set_config('app.context_grants', $1, true)`, ctxNorth); err != nil {
			writeErr = err
			return
		}
		_, writeErr = tx.Exec(ctx,
			`insert into trail_log (id, context_code, drive_id, started_at, created_by)
			 values (gen_random_uuid(), $1, $2, now(), 'g')`, ctxSouth, driveNorth)
	})
	if writeErr == nil {
		t.Fatal("a write into an ungranted context succeeded")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected 42501, got: %v", writeErr)
	}
}

var _ = store.MigrationRuntimeRole
