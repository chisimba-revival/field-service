package store_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"field-service/internal/store"
)

// These tests exist because of a specific failure that a review of the SQL would
// not have found.
//
// The row-level security policies in migrations/0001 were correct. They were
// also completely inert, because the application connected as a superuser and
// PostgreSQL exempts superusers from RLS outright. Reading every row in every
// context succeeded with no error and no visible leak, and the policies read
// exactly as intended in a diff.
//
// So every property below is asserted as behaviour against a real database, and
// each names the failure it guards rather than the SQL it expects. A test that
// asserted the policy text would have passed throughout.
//
// A contributor without Docker should not see a red build, so an unreachable
// database skips rather than fails. These are integration tests by nature.

// adminDSN connects as the migration role, which owns the tables and may assume
// the runtime role. Test fixtures are written through it because the runtime
// role deliberately cannot see rows it holds no grant for, and so cannot
// reliably set one up.
//
// It is a var rather than a const so that TestMain can repoint it; as a const
// the override would have compiled, looked deliberate, and done nothing.
var adminDSN = "postgres://fieldsvc:fieldsvc@127.0.0.1:55433/fieldlog?sslmode=disable"

const (
	ctxNorth = "north-reserve"
	ctxSouth = "south-reserve"
)

const (
	outingNorth = "11111111-1111-1111-1111-111111111111"
	outingSouth = "22222222-2222-2222-2222-222222222222"
)

func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Skipf("no database at %s: %v", adminDSN, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// asRuntime runs fn on a connection impersonating the application role.
//
// Impersonating rather than logging in is deliberate. The runtime role is
// created with LOGIN and no password precisely so nothing can log in as it by
// accident, and a test that needed to set a password in order to run would
// weaken the arrangement it exists to check.
func asRuntime(t *testing.T, fn func(context.Context, *pgx.Conn)) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), adminDSN)
	if err != nil {
		t.Skipf("no database at %s: %v", adminDSN, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	ctx := context.Background()
	// SET ROLE is itself not subject to the policies, which is what lets a
	// superuser arrange the test that the policies apply to everyone else.
	if _, err := conn.Exec(ctx, "set role "+store.MigrationRuntimeRole); err != nil {
		t.Fatalf("could not assume %s: %v", store.MigrationRuntimeRole, err)
	}
	fn(ctx, conn)
}

// seed leaves one sighting in each context, and clears anything a previous run
// left, so the counts these tests assert are the counts they are about.
func seed(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	// cascade, and every table listed: migration 0003 added trail_log,
	// trail_waypoint and media, which reference drive and sighting, so a
	// bare truncate of the two original tables is refused. Found by running
	// these tests after 0003, which is the only place it could be found.
	if _, err := conn.Exec(ctx, `truncate outing, sighting, trail_log, trail_waypoint, change_feed, media, operation_outcome cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	for i, d := range []struct{ id, context, guide string }{
		{outingNorth, ctxNorth, "guide-a"},
		{outingSouth, ctxSouth, "guide-b"},
	} {
		if _, err := conn.Exec(ctx,
			`insert into outing (id, context_code, guide_id, status)
			 values ($1, $2, $3, 'active')`,
			d.id, d.context, d.guide); err != nil {
			t.Fatalf("insert drive %s: %v", d.context, err)
		}
		if _, err := conn.Exec(ctx,
			`insert into sighting (id, context_code, outing_id, location,
			   captured_at, recorded_at, created_by, species_code)
			 values ($1, $2, $3, st_setsrid(st_makepoint($4, $5), 4326),
			         now(), now(), $6, 'LEOP')`,
			"aaaaaaaa-0000-0000-0000-00000000000"+string(rune('1'+i)),
			d.context, d.id, 36.8+float64(i)*0.1, -1.2, d.guide); err != nil {
			t.Fatalf("insert sighting for %s: %v", d.context, err)
		}
	}
}

// sightingsVisibleIn runs a transaction that sets the grants and then counts.
func sightingsVisibleIn(ctx context.Context, conn *pgx.Conn, grants string) (int, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// set local, never set. The plain form outlives the transaction and a pooled
	// connection would hand this caller's grants to the next request.
	//
	// Written as set_config rather than `set local`: SET is a utility command
	// and takes no bind parameters, and the grants come from a token. Building
	// the statement with them interpolated would be an injection point in the
	// one place that decides what a caller may read. The third argument is what
	// makes it local, so this is set local exactly.
	if _, err := tx.Exec(ctx,
		`select set_config('app.context_grants', $1, true)`, grants); err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(ctx, `select count(*) from sighting`).Scan(&n); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}

// The most important test in this file, and the one whose absence allowed an
// inert security control to be committed with a clean test run beside it.
func TestTheRuntimeRoleIsSubjectToThePolicies(t *testing.T) {
	seed(t, admin(t))

	var readErr error
	asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
		var n int
		readErr = conn.QueryRow(ctx, `select count(*) from sighting`).Scan(&n)
	})
	if readErr == nil {
		t.Fatal("the runtime role read the table with no grants set at all. The " +
			"policies are not in force, so every other test in this file is " +
			"measuring nothing. Check that the runtime role is not a superuser " +
			"and does not have BYPASSRLS.")
	}
	if !strings.Contains(readErr.Error(), "app.context_grants") {
		t.Fatalf("expected the failure to name the unset grants setting, got: %v", readErr)
	}
}

func TestGrantsDecideWhatIsReadable(t *testing.T) {
	seed(t, admin(t))

	cases := []struct {
		grants string
		want   int
		why    string
	}{
		{ctxNorth, 1, "a grant in one context must not reveal the other"},
		{ctxSouth, 1, "the same in the other direction"},
		{ctxNorth + "," + ctxSouth, 2, "two grants are two contexts"},
		{"nowhere-at-all", 0, "a grant in a context with no rows reads nothing rather than everything"},
	}
	for _, c := range cases {
		t.Run(c.grants, func(t *testing.T) {
			var got int
			var err error
			asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
				got, err = sightingsVisibleIn(ctx, conn, c.grants)
			})
			if err != nil {
				t.Fatalf("grants %q: %v", c.grants, err)
			}
			if got != c.want {
				t.Errorf("grants %q saw %d rows, want %d — %s",
					c.grants, got, c.want, c.why)
			}
		})
	}
}

// The read boundary must be the grant set, not the active context. The contract
// says using the active context as the read boundary "would hide a guide's
// northern-reserve drives while they happen to be working in the southern one".
func TestTheReadBoundaryIsTheGrantSetNotTheActiveContext(t *testing.T) {
	seed(t, admin(t))

	for _, granted := range []string{ctxNorth, ctxSouth} {
		t.Run(granted, func(t *testing.T) {
			var seen []string
			var err error
			asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
				tx, e := conn.Begin(ctx)
				if e != nil {
					err = e
					return
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, e := tx.Exec(ctx,
					`select set_config('app.context_grants', $1, true)`,
					granted); e != nil {
					err = e
					return
				}
				rows, e := tx.Query(ctx,
					`select context_code from sighting order by context_code`)
				if e != nil {
					err = e
					return
				}
				for rows.Next() {
					var c string
					if e := rows.Scan(&c); e != nil {
						err = e
						break
					}
					seen = append(seen, c)
				}
				rows.Close()
				if e := rows.Err(); e != nil {
					err = e
					return
				}
				err = tx.Commit(ctx)
			})
			if err != nil {
				t.Fatalf("%v", err)
			}
			if len(seen) != 1 || seen[0] != granted {
				t.Errorf("granted %s but saw %v — the boundary is not the grant set",
					granted, seen)
			}
		})
	}
}

// The design's own warning: set and set local look interchangeable and only one
// is correct here. This asserts the difference rather than the intent, because
// the difference is the whole reason the form matters.
func TestSetLocalRevertsAndPlainSetDoesNot(t *testing.T) {
	seed(t, admin(t))

	var afterLocal, afterPlain int
	var localErr, plainErr error

	asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
		localTx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := localTx.Exec(ctx,
			`select set_config('app.context_grants', $1, true)`, ctxNorth); err != nil {
			t.Fatalf("set local: %v", err)
		}
		if err := localTx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		// After the commit the grant must be gone. This does not raise, because
		// PostgreSQL keeps the placeholder defined and empty, so the read
		// succeeds and matches nothing. Zero rows is the safe outcome; two would
		// mean the grant survived the commit.
		localErr = conn.QueryRow(ctx, `select count(*) from sighting`).Scan(&afterLocal)

		// A plain set is expected to persist. Asserted because it is the failure
		// the design warns about: if this ever stopped being true the warning
		// would need rewriting against this PostgreSQL version.
		// The non-local form, deliberately not wrapped in a transaction: this is
		// the statement the design warns about, and it has to outlive one to be
		// worth asserting.
		if _, err := conn.Exec(ctx,
			`select set_config('app.context_grants', $1, false)`, ctxNorth); err != nil {
			t.Fatalf("plain set: %v", err)
		}
		plainErr = conn.QueryRow(ctx, `select count(*) from sighting`).Scan(&afterPlain)
	})

	if localErr == nil && afterLocal != 0 {
		t.Errorf("after set local and commit, %d rows were still visible. The grant "+
			"survived its transaction, and a pooled connection would give the next "+
			"request this caller's grants.", afterLocal)
	}
	if plainErr != nil {
		t.Fatalf("plain set then read: %v", plainErr)
	}
	if afterPlain != 1 {
		t.Errorf("a plain set did not persist on the connection (%d rows). The "+
			"design's warning that set outlives its transaction no longer holds "+
			"and should be re-checked against this version.", afterPlain)
	}
}

// A client that tries to write into a context it was not granted must not be
// able to. This is the with-check half of the policy.
func TestWritingIntoAnUngrantedContextIsRefused(t *testing.T) {
	seed(t, admin(t))

	var writeErr error
	asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
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
			`insert into sighting (id, context_code, outing_id, location,
			   captured_at, recorded_at, created_by)
			 values ('bbbbbbbb-0000-0000-0000-000000000001', $1, $2,
			         st_setsrid(st_makepoint(36.9, -1.3), 4326),
			         now(), now(), 'x')`, ctxSouth, outingSouth)
	})
	if writeErr == nil {
		t.Fatal("a write into an ungranted context succeeded. The context a record " +
			"lands in has to come from the token, not the request body.")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected a row-level security violation (42501), got: %v", writeErr)
	}
}

// Without this the application could switch its own policies off and the next
// request would read every context.
func TestTheRuntimeRoleCannotDisableThePolicies(t *testing.T) {
	var alterErr error
	asRuntime(t, func(ctx context.Context, conn *pgx.Conn) {
		_, alterErr = conn.Exec(ctx,
			`alter table sighting disable row level security`)
	})
	if alterErr == nil {
		t.Fatal("the runtime role disabled row-level security on a table it uses. " +
			"Isolation layer 2 can be switched off from inside layer 1.")
	}
	if !strings.Contains(alterErr.Error(), "owner") {
		t.Errorf("expected a not-the-owner failure, got: %v", alterErr)
	}
}

// The constraints stop a client making a claim it should not, and they belong in
// the schema so that no code path can skip them.
func TestAnAbsentCountIsNotAZero(t *testing.T) {
	conn := admin(t)
	seed(t, conn)
	ctx := context.Background()

	cases := []struct {
		name    string
		count   any
		wantErr bool
	}{
		{"absent", nil, false},
		{"a real count", 3, false},
		{"zero", 0, true},
		{"negative", -1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := conn.Exec(ctx,
				`insert into sighting (id, context_code, outing_id, location, count,
				   captured_at, recorded_at, created_by)
				 values (gen_random_uuid(), $1, $2,
				         st_setsrid(st_makepoint(36.8, -1.2), 4326), $3,
				         now(), now(), 'guide')`, ctxNorth, outingNorth, c.count)
			if c.wantErr && err == nil {
				t.Error("accepted. A zero asserts the animal was looked for and " +
					"there were none, which is not the same as not having counted.")
			}
			if !c.wantErr && err != nil {
				t.Errorf("rejected a legitimate value: %v", err)
			}
		})
	}
}

func TestACorrectionWithoutAReasonIsRefused(t *testing.T) {
	conn := admin(t)
	seed(t, conn)

	_, err := conn.Exec(context.Background(),
		`insert into sighting (id, context_code, outing_id, location, species_code,
		   recorded_species_code, captured_at, recorded_at, created_by)
		 values (gen_random_uuid(), $1, $2,
		         st_setsrid(st_makepoint(36.8, -1.2), 4326), 'LION', 'LEOP',
		         now(), now(), 'guide')`, ctxNorth, outingNorth)
	if err == nil {
		t.Fatal("a correction changing species code was accepted with no reason. " +
			"Rule 15: a silent overwrite cannot be represented as a completed review.")
	}
	if !strings.Contains(err.Error(), "correction_has_reason") {
		t.Errorf("expected the correction_has_reason constraint, got: %v", err)
	}
}

// The role is created with LOGIN and no password so nothing can log in as it by
// accident. A migration that quietly left a working credential behind would undo
// the whole arrangement.
func TestTheRuntimeRoleIsNotPrivileged(t *testing.T) {
	conn := admin(t)
	var canLogin, isSuper, bypasses bool
	err := conn.QueryRow(context.Background(),
		`select rolcanlogin, rolsuper, rolbypassrls
		 from pg_roles where rolname = $1`, store.MigrationRuntimeRole).
		Scan(&canLogin, &isSuper, &bypasses)
	if err != nil {
		t.Skipf("role %s not present: %v", store.MigrationRuntimeRole, err)
	}
	if isSuper || bypasses {
		t.Errorf("the runtime role is superuser=%v bypassrls=%v. Either one disables "+
			"the policies entirely, and the failure is invisible.", isSuper, bypasses)
	}
	if canLogin {
		// Fine once a deployment sets a password; it does not mean anything is
		// wrong yet.
		t.Log("the runtime role can log in, so it needs a password to do so")
	}
}

func TestMain(m *testing.M) {
	// So CI can point these at a different database without the tests knowing
	// how they are configured. An unset variable leaves the default alone.
	if dsn := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_ADMIN_DSN")); dsn != "" {
		adminDSN = dsn
	}
	// Serialise against the other integration package. See dblock_test.go for
	// why this is a lock and not a reminder to pass -p 1.
	if conn, err := pgx.Connect(context.Background(), adminDSN); err == nil {
		release := holdAdvisoryLock(conn)
		defer release()
		defer func() { _ = conn.Close(context.Background()) }()
		code := m.Run()
		release()
		os.Exit(code)
	}
	// No database: let the tests skip themselves rather than fail here, so a
	// checkout without Postgres still builds and vets.
	os.Exit(m.Run())
}

// Why the runtime role exists, asserted rather than assumed.
//
// `force row level security` extends policies to the table owner. It does not
// extend them to a superuser: PostgreSQL exempts superusers from RLS outright,
// whatever the table's settings are.
//
// The migration role is a superuser, because the container's POSTGRES_USER is
// one. So the owner of these tables is a superuser, and the policies do not
// apply to it. That is not a defect in the policies and it is not fixable by
// another setting — it is the reason the application has a separate,
// non-privileged role at all.
//
// This test therefore asserts the bypass rather than pretending to prevent it.
// If a future migration makes the migration role a non-superuser, this test
// fails and the `force` clause starts doing real work, at which point it needs
// a test of its own that queries as the owner with no grants set.
func TestTheMigrationRoleBypassesThePoliciesAndMustNotBeUsedAtRuntime(t *testing.T) {
	conn := admin(t)
	seed(t, conn)
	ctx := context.Background()

	var isSuper bool
	if err := conn.QueryRow(ctx,
		`select rolsuper from pg_roles where rolname = current_user`).
		Scan(&isSuper); err != nil {
		t.Fatalf("could not read the current role's attributes: %v", err)
	}

	var n int
	err := conn.QueryRow(ctx, `select count(*) from sighting`).Scan(&n)
	switch {
	case err == nil && isSuper:
		t.Logf("the migration role is a superuser and read all %d rows with no "+
			"grants set. Correct, and the reason the application cannot use it.", n)
	case err == nil && !isSuper:
		t.Errorf("a non-superuser owner read %d rows with no grants set. Either "+
			"`force row level security` has gone, or the owner is not the role "+
			"the policies were written for.", n)
	case !isSuper:
		t.Errorf("a non-superuser owner was refused, so `force row level "+
			"security` is doing its job. Update this test: the bypass the "+
			"runtime role exists to avoid no longer happens this way (%v).", err)
	default:
		t.Fatalf("unexpected: superuser=%v err=%v", isSuper, err)
	}
}
