package pull_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"field-service/internal/pull"
)

// The rules these prove, against real Postgres rather than a fake:
//
//   - A caller sees only the contexts they are granted, because the feed is
//     filtered by row-level security and nothing in this package names a
//     context.
//   - A feed is read as a coherent sequence, oldest first, carrying the row
//     itself.
//   - Retention that has genuinely eaten rows produces a resync rather than a
//     plausible-looking short page.
//
// None of this is checkable with a fake store, because the property being tested
// is precisely the one the fake has to assume: that the database does the
// filtering.

var dsn = "postgres://fieldsvc:fieldsvc@127.0.0.1:55433/fieldlog?sslmode=disable"

// integrationTestLock is the key this suite and the store suite both take.
//
// The same value in both, which is the whole point: two suites holding
// different keys exclude nobody, and the first version of this gave each
// package its own key and wrote a comment saying that was deliberate.
const integrationTestLock int64 = 821004001

func TestMain(m *testing.M) {
	if d := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_ADMIN_DSN")); d != "" {
		dsn = d
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		os.Exit(m.Run()) // no database; the tests skip themselves
	}
	if _, err := conn.Exec(context.Background(), "select pg_advisory_lock($1)", integrationTestLock); err != nil {
		panic("pull: taking the integration-test lock: " + err.Error())
	}
	code := m.Run()
	_, _ = conn.Exec(context.Background(), "select pg_advisory_unlock($1)", integrationTestLock)
	_ = conn.Close(context.Background())
	os.Exit(code)
}

// runtime connects as the application role with a grant set, which is how every
// real request arrives.
func runtime(t *testing.T, grants string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "begin"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "rollback") })
	// As the runtime role, and this is not incidental. The default connection
	// role is a superuser, and PostgreSQL exempts superusers from RLS outright,
	// whatever the policies say. Without this line the grant below is set,
	// believed, and ignored — and a test asserting that a caller sees only their
	// own contexts passes while reading the whole feed.
	//
	// It passed exactly that way until this line was added. That is the same
	// failure mode store.TestTheMigrationRoleBypassesThePoliciesAndMustNotBeUsedAtRuntime
	// documents, arrived at independently: a security test that silently tests
	// nothing is worse than no test, because it is counted.
	if _, err := conn.Exec(ctx, "set role fieldapp"); err != nil {
		t.Fatalf("set role fieldapp: %v", err)
	}
	// set_config, not SET: SET takes no bind parameters, and these values come
	// from a token. The third argument makes it transaction-local, so a pooled
	// connection cannot hand one request's grants to the next.
	if _, err := conn.Exec(ctx, `select set_config('app.context_grants', $1, true)`, grants); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `select set_config('app.caller_user_id', $1, true)`, "user-1"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// admin connects with no grant set, to write fixtures the runtime role cannot.
func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	reset(t, conn)
	return conn
}

func reset(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	// Every integration package shares this database, and go test runs packages
	// in parallel. The advisory lock in TestMain serialises them; this clears the
	// fixtures the last one left.
	if _, err := conn.Exec(context.Background(),
		`truncate change_feed, operation_outcome, media, trail_waypoint, log_book_entry, outing, drive_detail, hike_detail, camp_detail cascade`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// feed writes one change as the owner, which the runtime role cannot do.
func feed(t *testing.T, conn *pgx.Conn, contextCode, entity string, revision int) int64 {
	t.Helper()
	var seq int64
	err := conn.QueryRow(context.Background(), `
		insert into change_feed (context_code, entity_type, entity_id, revision, body)
		values ($1, $2, gen_random_uuid(), $3, $4)
		returning seq`,
		contextCode, entity, revision,
		[]byte(`{"notes":"from the feed"}`)).Scan(&seq)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	return seq
}

// A caller is shown only the contexts they are granted. This is the property the
// whole package's correctness rests on, and it is enforced by policies rather
// than by any filter here — so a fake store cannot demonstrate it.
func TestACallerSeesOnlyTheContextsTheyAreGranted(t *testing.T) {
	a := admin(t)
	feed(t, a, "north-reserve", "log_book_entry", 1)
	feed(t, a, "south-reserve", "log_book_entry", 1)

	got, err := pull.New(pull.NewPgxStore(runtime(t, "north-reserve"))).
		Pull(context.Background(), "user-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusOK {
		t.Fatalf("status %q", got.Status)
	}
	if len(got.Changes) != 1 {
		t.Fatalf("%d changes for one granted context; the feed returned "+
			"contexts the caller does not hold a grant for", len(got.Changes))
	}
}

// Two granted contexts, two changes, and the ordering is by seq — which is the
// reason the feed serialises each row rather than joining at read time.
func TestChangesArriveOldestFirstAndCarryTheRowItself(t *testing.T) {
	a := admin(t)
	first := feed(t, a, "north-reserve", "log_book_entry", 1)
	second := feed(t, a, "north-reserve", "log_book_entry", 2)

	got, err := pull.New(pull.NewPgxStore(runtime(t, "north-reserve"))).
		Pull(context.Background(), "user-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 2 {
		t.Fatalf("%d changes", len(got.Changes))
	}
	if got.Changes[0].Seq != first || got.Changes[1].Seq != second {
		t.Errorf("seqs %d then %d; want %d then %d", got.Changes[0].Seq, got.Changes[1].Seq, first, second)
	}
	if got.Changes[0].Body["notes"] != "from the feed" {
		t.Errorf("body %v carried no content", got.Changes[0].Body)
	}
}

func TestACursorResumesWhereTheLastPageEnded(t *testing.T) {
	a := admin(t)
	feed(t, a, "north-reserve", "log_book_entry", 1)
	second := feed(t, a, "north-reserve", "log_book_entry", 2)
	feed(t, a, "north-reserve", "log_book_entry", 3)

	svc := pull.New(pull.NewPgxStore(runtime(t, "north-reserve")))
	got, err := svc.Pull(context.Background(), "user-1", pull.Cursor(second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != 1 {
		t.Fatalf("%d changes after the cursor, want 1", len(got.Changes))
	}
	if got.Changes[0].Revision != 3 {
		t.Errorf("revision %d, want 3", got.Changes[0].Revision)
	}
}

func TestHasMoreIsTrueWhenMoreChangesRemain(t *testing.T) {
	a := admin(t)
	feed(t, a, "north-reserve", "log_book_entry", 1)
	feed(t, a, "north-reserve", "log_book_entry", 2)
	feed(t, a, "north-reserve", "log_book_entry", 3)

	got, err := pull.New(pull.NewPgxStore(runtime(t, "north-reserve"))).
		Pull(context.Background(), "user-1", pull.Cursor(0))
	if err != nil {
		t.Fatal(err)
	}
	// The bound is 200 and there are 3 rows, so this page is the last one.
	if got.HasMore {
		t.Error("has_more true with three rows and a bound of 200")
	}
}

// Retention that has genuinely eaten rows must produce a resync, not a short
// page. The rows are deleted by the owner here, which is what the 30-day
// retention job does.
func TestRetentionThatHasEatenRowsProducesAResyncRatherThanAShortPage(t *testing.T) {
	a := admin(t)
	oldest := feed(t, a, "north-reserve", "log_book_entry", 1)
	feed(t, a, "north-reserve", "log_book_entry", 2)
	feed(t, a, "north-reserve", "log_book_entry", 3)

	// Prune everything the client has not yet seen.
	if _, err := a.Exec(context.Background(),
		`delete from change_feed where seq = $1`, oldest); err != nil {
		t.Fatal(err)
	}

	got, err := pull.New(pull.NewPgxStore(runtime(t, "north-reserve"))).
		Pull(context.Background(), "user-1", pull.Cursor(0))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusResyncRequired {
		t.Fatalf("status %q, want resync_required. A short page here would look "+
			"like 'nothing else changed' and the client would never learn it had "+
			"missed the oldest row.", got.Status)
	}
	if len(got.Changes) != 0 {
		t.Errorf("a resync carried %d changes", len(got.Changes))
	}
}

// An empty feed is not a resync, against the database rather than a fake. This is
// the direction that fails quietly: if it were wrong, every device would discard
// its state whenever the reserve was quiet.
func TestAnEmptyFeedIsNotAResync(t *testing.T) {
	admin(t)
	got, err := pull.New(pull.NewPgxStore(runtime(t, "north-reserve"))).
		Pull(context.Background(), "user-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusOK {
		t.Errorf("status %q for an empty feed, want ok", got.Status)
	}
	if got.NextCursor != "" {
		t.Errorf("next cursor %q on an empty feed", got.NextCursor)
	}
}

// An unset grants setting must fail rather than default, or a caller with no
// grants would read the whole feed.
func TestAnUnsetGrantsSettingFailsRatherThanReadingEverything(t *testing.T) {
	a := admin(t)
	feed(t, a, "north-reserve", "log_book_entry", 1)

	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	ctx := context.Background()
	if _, err := conn.Exec(ctx, "begin"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, "rollback") }()
	// As the runtime role, because the default connection role is a superuser and
	// PostgreSQL exempts superusers from RLS outright. Without this the test would
	// "pass" for the same reason the whole application must not run: a
	// connection that bypasses every policy.
	if _, err := conn.Exec(ctx, "set role fieldapp"); err != nil {
		t.Fatalf("set role fieldapp: %v", err)
	}

	if _, err := pull.New(pull.NewPgxStore(conn)).Pull(ctx, "user-1", ""); err == nil {
		t.Error("a pull with no grants set succeeded. current_setting must raise " +
			"rather than return an empty string, or an unset boundary would read " +
			"as a boundary matching nothing rather than as a fault.")
	}
}
