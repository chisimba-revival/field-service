package push_test

// These are integration tests against a real Postgres, because every rule in
// this package is enforced by the database rather than by Go code: revisions
// live in a column, the correction's reason lives in a check constraint, and the
// outcome's uniqueness lives in a primary key. A fake would pass while the real
// arrangement did nothing, which is exactly how the RLS policies were found to
// be inert.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"field-service/internal/push"
	"field-service/internal/store"
)

var dsn = "postgres://fieldsvc:fieldsvc@127.0.0.1:55433/fieldlog?sslmode=disable"

const (
	ctxNorth = "north-reserve"
	ctxSouth = "south-reserve"

	driveNorth = "11111111-1111-1111-1111-111111111111"
)

// admin connects as the migration role to arrange fixtures. It may assume the
// runtime role, because the runtime role deliberately cannot see rows it holds
// no grant for and so cannot set one up.
func admin(t *testing.T) *pgx.Conn {
	t.Helper()
	if d := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_ADMIN_DSN")); d != "" {
		dsn = d
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database at %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// asRuntime runs fn impersonating the application role, with the grants a
// caller in ctxNorth would have.
func asRuntime(t *testing.T, grants string, fn func(context.Context, *pgx.Conn)) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database at %s: %v", dsn, err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	c := context.Background()
	if _, err := conn.Exec(c, "set role "+store.MigrationRuntimeRole); err != nil {
		t.Fatalf("assuming %s: %v", store.MigrationRuntimeRole, err)
	}
	if _, err := conn.Exec(c, "select set_config('app.context_grants', $1, false)", grants); err != nil {
		t.Fatalf("setting grants: %v", err)
	}
	if _, err := conn.Exec(c, "select set_config('app.caller_user_id', $1, false)", "user-1"); err != nil {
		t.Fatalf("setting caller: %v", err)
	}
	fn(c, conn)
}

// reset clears the tables these tests write, leaving the drives.
func reset(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	_, err := conn.Exec(context.Background(),
		`truncate change_feed, operation_outcome, media, trail_waypoint, trail_log, sighting, drive cascade`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

func seedDrive(t *testing.T, conn *pgx.Conn, id, ctxCode string) {
	t.Helper()
	_, err := conn.Exec(context.Background(),
		`insert into drive (id, context_code, guide_id, status) values ($1,$2,'guide-1','active')
		 on conflict (id) do nothing`, id, ctxCode)
	if err != nil {
		t.Fatalf("seeding drive: %v", err)
	}
}

// service returns a push service whose transaction is opened on a runtime-role
// connection with the given grants.
func service(t *testing.T, grants string) (*push.Service, *pgx.Conn) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Skipf("no database at %s: %v", dsn, err)
	}
	if _, err := conn.Exec(context.Background(), "set role "+store.MigrationRuntimeRole); err != nil {
		t.Fatalf("assuming role: %v", err)
	}
	set := func(ctxCode string) {
		if _, err := conn.Exec(context.Background(),
			"select set_config('app.context_grants', $1, false)", grants); err != nil {
			t.Fatalf("grants: %v", err)
		}
		if _, err := conn.Exec(context.Background(),
			"select set_config('app.caller_user_id', $1, false)", "user-1"); err != nil {
			t.Fatalf("caller: %v", err)
		}
	}
	set(ctxNorth)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return push.New(push.NewPgxStore(poolOf(conn))), conn
}

// poolOf adapts a single connection to the one method PgxStore needs. A pool
// would be the production shape; a connection is enough to prove the SQL and
// the rules, and using one means the tests cannot accidentally depend on
// concurrent sessions.
type oneConn struct{ c *pgx.Conn }

func (o oneConn) Begin(ctx context.Context) (pgx.Tx, error) { return o.c.Begin(ctx) }

func poolOf(c *pgx.Conn) *oneConn { return &oneConn{c} }

// uuidFor derives a stable uuid from a name, so a test can talk about
// "op-batch-1" and still hand the database a well-formed id.
//
// The first version built ids by pasting the drive and the operation name
// together, which is not a uuid, and every test that used it failed on a type
// error that had nothing to do with what it was testing.
func uuidFor(name string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	v := h.Sum64()
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(v>>32), uint16(v>>16), uint16(v&0xffff), uint16(v>>48), v&0xffffffffffff)
}

func aCreate(id, drive, species string, count int) push.Operation {
	return push.Operation{
		OperationID: uuidFor(id),
		Entity:      "sighting",
		Kind:        "create",
		EntityID:    uuidFor(id),
		CapturedAt:  time.Date(2026, 10, 4, 6, 14, 0, 0, time.UTC),
		Payload: map[string]any{
			"drive_id": drive, "species_code": species, "count": count,
			"longitude": 36.8219, "latitude": -1.2921, "notes": "moving east",
		},
	}
}

func northCaller() push.Caller { return push.Caller{ID: "user-1", WriteContext: ctxNorth} }

// A create is applied, and lands in the caller's context rather than anywhere
// the client chose.
func TestACreateIsAppliedIntoTheCallersContext(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)
	op := aCreate("op-create-1", driveNorth, "LEOP", 2)

	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{op})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if got[0].Outcome != push.OutcomeApplied {
		t.Fatalf("outcome %q, code %q", got[0].Outcome, got[0].ErrorCode)
	}
	if got[0].NewRevision != 1 {
		t.Errorf("revision %d on a new record, want 1", got[0].NewRevision)
	}

	var ctxCode, species string
	if err := conn.QueryRow(context.Background(),
		`select context_code, species_code from sighting where id = $1`,
		op.EntityID).Scan(&ctxCode, &species); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if ctxCode != ctxNorth {
		t.Errorf("context %q, want %q", ctxCode, ctxNorth)
	}
	if species != "LEOP" {
		t.Errorf("species %q", species)
	}
}

// A client cannot write into a context it was not granted: the write context is
// taken from the token, and a payload naming another context is ignored.
func TestACreateCannotBeDirectedIntoAnotherContextByItsPayload(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)
	seedDrive(t, adm, "22222222-2222-2222-2222-222222222222", ctxSouth)

	svc, conn := service(t, ctxNorth)
	op := aCreate("op-ctx-1", driveNorth, "LEOP", 1)
	// Every field a client might try to smuggle a context through.
	op.Payload["context_code"] = ctxSouth
	op.Payload["contextCode"] = ctxSouth
	op.Payload["ctx"] = ctxSouth

	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{op}); err != nil {
		t.Fatal(err)
	}

	var ctxCode string
	if err := conn.QueryRow(context.Background(),
		`select context_code from sighting where id = $1`, op.EntityID).Scan(&ctxCode); err != nil {
		t.Fatal(err)
	}
	if ctxCode != ctxNorth {
		t.Errorf("the record landed in %q. The write context comes from the "+
			"token; a client naming a context in its payload changes nothing.",
			ctxCode)
	}
}

// The change feed carries the row, so one page is one round trip.
func TestACreateWritesAChangeFeedEntryCarryingTheRow(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, _ := service(t, ctxNorth)
	op := aCreate("op-feed-1", driveNorth, "LEOP", 2)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{op}); err != nil {
		t.Fatal(err)
	}

	var body []byte
	if err := adm.QueryRow(context.Background(),
		`select body from change_feed where entity_id = $1`, op.EntityID).Scan(&body); err != nil {
		t.Fatalf("no feed entry: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("feed body is not json: %v", err)
	}
	if decoded["species_code"] != "LEOP" {
		t.Errorf("the feed carried %v, not the row. A feed naming only what "+
			"changed turns one page into a round trip per record.", decoded["species_code"])
	}
}

// Rule 7: a retry returns the recorded outcome and never reaches the row again.
func TestARetryOfACreateIsAppliedExactlyOnce(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)
	op := aCreate("op-retry-1", driveNorth, "LEOP", 2)

	first, err := svc.Push(context.Background(), northCaller(), []push.Operation{op})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Push(context.Background(), northCaller(), []push.Operation{op})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Outcome != push.OutcomeApplied || second[0].Outcome != push.OutcomeApplied {
		t.Fatalf("first %q, second %q", first[0].Outcome, second[0].Outcome)
	}

	var sightings, feeds, outcomes int
	_ = conn.QueryRow(context.Background(),
		`select count(*) from sighting where id = $1`, op.EntityID).Scan(&sightings)
	_ = adm.QueryRow(context.Background(),
		`select count(*) from change_feed where entity_id = $1`, op.EntityID).Scan(&feeds)
	_ = adm.QueryRow(context.Background(),
		`select count(*) from operation_outcome where operation_id = $1`, op.OperationID).Scan(&outcomes)
	if sightings != 1 || feeds != 1 || outcomes != 1 {
		t.Errorf("%d sightings, %d feed entries, %d outcomes for one retried operation; want 1 each",
			sightings, feeds, outcomes)
	}
}

// The outcome row and the entity change commit together. Dropping the outcome
// table mid-batch is how that is proved rather than asserted.
func TestTheEntityChangeAndItsOutcomeCommitTogetherOrNotAtAll(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)

	// Make the outcome write fail after the sighting has been inserted.
	atomicOp := aCreate("op-atomic-1", driveNorth, "LEOP", 1)
	if _, err := adm.Exec(context.Background(), "alter table operation_outcome rename to operation_outcome_hidden"); err != nil {
		t.Skipf("cannot rename: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adm.Exec(context.Background(), "alter table operation_outcome_hidden rename to operation_outcome")
	})

	_, err := svc.Push(context.Background(), northCaller(),
		[]push.Operation{atomicOp})
	if err == nil {
		t.Error("the push reported success while its outcome table did not exist")
	}

	if _, err := adm.Exec(context.Background(), "alter table operation_outcome_hidden rename to operation_outcome"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(context.Background(),
		`select count(*) from sighting where id = $1`, atomicOp.EntityID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d sightings survived a push that could not record its outcome. "+
			"The trainee's observation would exist with no trace of how it got here, "+
			"and a retry would have nothing to be idempotent against.", n)
	}
}

// Rule 15, the reason the service exists: a correction changing species or count
// requires a reason, enforced by a table constraint so no code path can skip it.
func TestACorrectionChangingACountWithoutAReasonIsRefused(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, _ := service(t, ctxNorth)
	create := aCreate("op-c-1", driveNorth, "LEOP", 7)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{create}); err != nil {
		t.Fatal(err)
	}

	rev := int64(1)
	correct := push.Operation{
		OperationID: uuidFor("op-correct-noreason"), Entity: "sighting", Kind: "correct",
		EntityID:     create.EntityID,
		BaseRevision: &rev,
		Payload:      map[string]any{"count": 4}, // no reason
	}
	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{correct})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("outcome %q, want refused", got[0].Outcome)
	}
	if got[0].ErrorCode != "correction_requires_a_reason" {
		t.Errorf("error code %q. The client must be able to tell this from a "+
			"generic failure, and the reason is the whole point.", got[0].ErrorCode)
	}
}

// A correction with a reason is applied, and the original is retained forever.
func TestACorrectionRetainsTheOriginalClaimBesideTheCorrection(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)
	create := aCreate("op-c-2", driveNorth, "LEOP", 7)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{create}); err != nil {
		t.Fatal(err)
	}

	rev := int64(1)
	correct := push.Operation{
		OperationID: uuidFor("op-correct-ok"), Entity: "sighting", Kind: "correct",
		EntityID: create.EntityID, BaseRevision: &rev,
		Payload: map[string]any{
			"count": 4, "correction_reason": "Seven on the first count, four on the second.",
			"verification_notes": "A mentor recounted them.",
		},
	}
	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{correct})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeApplied {
		t.Fatalf("outcome %q, code %q", got[0].Outcome, got[0].ErrorCode)
	}
	if got[0].NewRevision != 2 {
		t.Errorf("revision %d after one correction, want 2", got[0].NewRevision)
	}

	var count int
	var recordedCount pgtype.Int8
	var species, recordedSpecies, reason *string
	if err := conn.QueryRow(context.Background(),
		`select count, recorded_count, species_code, recorded_species_code, correction_reason
		   from sighting where id = $1`, create.EntityID).
		Scan(&count, &recordedCount, &species, &recordedSpecies, &reason); err != nil {
		t.Fatal(err)
	}
	if count != 4 {
		t.Errorf("count is %d; the correction did not apply", count)
	}
	if !recordedCount.Valid || recordedCount.Int64 != 7 {
		t.Errorf("recorded_count %v, want 7. The original is retained "+
			"permanently — a trainee can only learn from a correction if they "+
			"can see what was changed.", recordedCount)
	}
	if reason == nil || *reason == "" {
		t.Error("correction_reason is empty after a correction that carried one")
	}
}

// Rule 6: conflicts are decided by revision, never by comparing timestamps.
func TestACorrectionAgainstAStaleRevisionIsRefusedWithBothSidesKept(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, _ := service(t, ctxNorth)
	create := aCreate("op-c-3", driveNorth, "LEOP", 2)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{create}); err != nil {
		t.Fatal(err)
	}

	// Somebody else corrects it first.
	first := int64(1)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{{
		OperationID: uuidFor("op-other-1"), Entity: "sighting", Kind: "correct",
		EntityID: create.EntityID, BaseRevision: &first,
		Payload: map[string]any{"count": 3, "correction_reason": "Recounted."},
	}}); err != nil {
		t.Fatal(err)
	}

	// Our device was offline and still believes revision 1.
	stale := int64(1)
	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{{
		OperationID: uuidFor("op-stale-1"), Entity: "sighting", Kind: "correct",
		EntityID: create.EntityID, BaseRevision: &stale,
		Payload: map[string]any{"count": 9, "correction_reason": "I counted nine."},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Fatalf("outcome %q; a stale revision must be refused", got[0].Outcome)
	}
	if got[0].ErrorCode != "revision_conflict" {
		t.Errorf("error code %q", got[0].ErrorCode)
	}
	if got[0].ServerRevision != 2 {
		t.Errorf("server revision %d, want 2. The client needs to know what it "+
			"was actually up against.", got[0].ServerRevision)
	}
	if got[0].ServerState == nil {
		t.Error("no server state; the contract requires both sides so the client decides")
	}
	if got[0].ClientState["count"] != 9 {
		t.Errorf("client state %v; the client's own values must be preserved", got[0].ClientState)
	}
}

// A correction of a record that does not exist is not turned into a create.
func TestACorrectionOfAnAbsentRecordIsRefusedRatherThanCreated(t *testing.T) {
	adm := admin(t)
	reset(t, adm)

	svc, _ := service(t, ctxNorth)
	rev := int64(0)
	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{{
		OperationID: uuidFor("op-ghost-1"), Entity: "sighting", Kind: "correct",
		EntityID: uuidFor("ghost"), BaseRevision: &rev,
		Payload: map[string]any{"count": 4, "correction_reason": "x"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("outcome %q, want refused. A correction names what is wrong; "+
			"with no record there is nothing to be wrong about, and inventing one "+
			"would put a sighting nobody saw into the logbook.", got[0].Outcome)
	}
	if got[0].ErrorCode != "correct_of_absent_record" {
		t.Errorf("error code %q", got[0].ErrorCode)
	}
}

// A create carrying a base revision is refused: the client believes it is
// changing something, and creating over it would overwrite without noticing.
func TestACreateCarryingABaseRevisionIsRefused(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, _ := service(t, ctxNorth)
	op := aCreate("op-create-rev", driveNorth, "LEOP", 1)
	rev := int64(4)
	op.BaseRevision = &rev

	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{op})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused || got[0].ErrorCode != "create_with_base_revision" {
		t.Errorf("outcome %q code %q, want refused/create_with_base_revision",
			got[0].Outcome, got[0].ErrorCode)
	}
}

// An unknown kind is refused rather than deferred: it will not become known by
// waiting, and deferring keeps a client retrying something that cannot succeed.
func TestAnUnknownKindIsRefusedRatherThanDeferredForever(t *testing.T) {
	adm := admin(t)
	reset(t, adm)

	svc, _ := service(t, ctxNorth)
	got, err := svc.Push(context.Background(), northCaller(), []push.Operation{{
		OperationID: uuidFor("op-unknown-1"), Entity: "sighting", Kind: "obliterate",
		EntityID: uuidFor("obliterate"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("outcome %q, want refused. Deferred would mean retry forever.",
			got[0].Outcome)
	}
	if got[0].ErrorCode != "unsupported_kind" {
		t.Errorf("error code %q", got[0].ErrorCode)
	}
}

// Isolation still holds on the push path: a caller granted one context cannot
// read another's rows through the change feed it writes.
func TestTheChangeFeedHidesRowsFromContextsTheCallerWasNotGranted(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxSouth)

	svc, _ := service(t, ctxNorth) // granted north, writing into north
	op := aCreate("op-iso-1", driveNorth, "LEOP", 1)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{op}); err != nil {
		t.Fatal(err)
	}

	// The feed row is in ctxNorth, so a reader holding only ctxSouth sees none.
	asRuntime(t, ctxSouth, func(c context.Context, conn *pgx.Conn) {
		var n int
		if err := conn.QueryRow(c,
			`select count(*) from change_feed where entity_id = $1`, op.EntityID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("a caller granted only %s read %d feed rows from %s. The "+
				"change feed is the one place a client sees everything that "+
				"happened, so a hole here is a hole everywhere.", ctxSouth, n, ctxNorth)
		}
	})
}

// A batch is not all-or-nothing: a refusal among the operations leaves the
// others applied, and they all commit together.
func TestARefusalInABatchDoesNotUndoTheOthers(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)
	good := aCreate("op-batch-1", driveNorth, "LEOP", 1)
	ghost := push.Operation{
		OperationID: uuidFor("op-batch-ghost"), Entity: "sighting", Kind: "correct",
		EntityID: uuidFor("ghost"), BaseRevision: ptr(int64(1)),
		Payload: map[string]any{"count": 4, "correction_reason": "x"},
	}
	later := aCreate("op-batch-2", driveNorth, "LION", 1)

	got, err := svc.Push(context.Background(), northCaller(),
		[]push.Operation{good, ghost, later})
	if err != nil {
		t.Fatal(err)
	}
	if got[1].Outcome != push.OutcomeRefused {
		t.Errorf("the middle operation came back %q", got[1].Outcome)
	}
	for _, op := range []push.Operation{good, later} {
		var n int
		if err := conn.QueryRow(context.Background(),
			`select count(*) from sighting where id = $1`, op.EntityID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s survived as %d rows; a refusal in the batch discarded "+
				"the observations either side of it", op.OperationID, n)
		}
	}
}

// A sparse correction leaves the fields it did not mention alone.
func TestASparseCorrectionLeavesUnmentionedFieldsAlone(t *testing.T) {
	adm := admin(t)
	reset(t, adm)
	seedDrive(t, adm, driveNorth, ctxNorth)

	svc, conn := service(t, ctxNorth)
	create := aCreate("op-sparse-1", driveNorth, "LEOP", 5)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{create}); err != nil {
		t.Fatal(err)
	}

	rev := int64(1)
	if _, err := svc.Push(context.Background(), northCaller(), []push.Operation{{
		OperationID: uuidFor("op-sparse-corr"), Entity: "sighting", Kind: "correct",
		EntityID: create.EntityID, BaseRevision: &rev,
		Payload: map[string]any{"count": 3, "correction_reason": "Three, not five."},
	}}); err != nil {
		t.Fatal(err)
	}

	var species, notes string
	if err := conn.QueryRow(context.Background(),
		`select species_code, notes from sighting where id = $1`, create.EntityID).
		Scan(&species, &notes); err != nil {
		t.Fatal(err)
	}
	if species != "LEOP" {
		t.Errorf("species became %q; a correction that did not mention it must "+
			"not blank it. A client sending a sparse correction and finding the "+
			"rest of the record wiped would stop trusting it.", species)
	}
	if notes != "moving east" {
		t.Errorf("notes became %q", notes)
	}
}

func ptr[T any](v T) *T { return &v }

// integrationTestLock is the key this suite and the store suite both take.
//
// The same value in both, which is the whole point: two suites holding
// different keys exclude nobody, and the first version of this file made that
// mistake in both directions at once by giving each package its own key.
//
// Declared here rather than imported because the store's copy lives in a _test
// file, and a test file of one package is not part of another package's build.
// One int64 repeated in two test files, with the reason in both, beats moving a
// test-only constant into production code.
const integrationTestLock int64 = 821004001

// TestMain serialises this suite against the other integration package, which
// shares the same database and would otherwise truncate its fixtures out from
// under these tests.
func TestMain(m *testing.M) {
	if d := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_ADMIN_DSN")); d != "" {
		dsn = d
	}
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		// No database. The tests skip themselves; nothing to serialise against.
		os.Exit(m.Run())
	}
	if _, err := conn.Exec(context.Background(), "select pg_advisory_lock($1)", integrationTestLock); err != nil {
		panic("push: taking the integration-test lock: " + err.Error())
	}
	code := m.Run()
	_, _ = conn.Exec(context.Background(), "select pg_advisory_unlock($1)", integrationTestLock)
	_ = conn.Close(context.Background())
	os.Exit(code)
}

// A payload missing a field it must carry is refused, not attempted.
//
// Found by sending a real HTTP request with no drive_id: it reached the insert
// as the empty string, the database refused it with a type error, and the whole
// batch came back as a 500. Every other client mistake here is a refusal, so the
// inconsistency mattered as much as the fault — and one bad operation taking
// three good ones with it is exactly what the contract forbids.
func TestACreateMissingItsDriveIsRefusedRatherThanFailingTheBatch(t *testing.T) {
	svc, conn := service(t, ctxNorth)
	defer func() { _ = conn.Close(context.Background()) }()
	seedDrive(t, conn, driveNorth, ctxNorth)

	op := aCreate("op-no-drive", driveNorth, "LEOP", 2)
	delete(op.Payload, "drive_id")

	got, err := svc.Push(context.Background(),
		push.Caller{ID: "user-1", WriteContext: ctxNorth}, []push.Operation{op})
	if err != nil {
		t.Fatalf("one bad operation failed the batch: %v", err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("outcome %q, want refused", got[0].Outcome)
	}
	if got[0].ErrorCode != "create_without_drive_id" {
		t.Errorf("code %q", got[0].ErrorCode)
	}
}

// The other two in the same batch must still land.
func TestAGoodOperationInTheSameBatchStillLands(t *testing.T) {
	svc, conn := service(t, ctxNorth)
	defer func() { _ = conn.Close(context.Background()) }()
	seedDrive(t, conn, driveNorth, ctxNorth)

	bad := aCreate("op-no-drive", driveNorth, "LEOP", 2)
	delete(bad.Payload, "drive_id")
	good := aCreate("op-fine", driveNorth, "LION", 1)

	got, err := svc.Push(context.Background(),
		push.Caller{ID: "user-1", WriteContext: ctxNorth},
		[]push.Operation{bad, good})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("the bad one came back %q", got[0].Outcome)
	}
	if got[1].Outcome != push.OutcomeApplied {
		t.Errorf("the good one came back %q. A device that recorded three things "+
			"and had one malformed needs the other two.", got[1].Outcome)
	}
}
