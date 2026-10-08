package push_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"field-service/internal/push"
)

// These tests exist because the four rules in push.go are the ones whose
// obvious implementation is wrong. Each test names the rule rather than the
// mechanism, so a refactor that keeps the behaviour keeps the test.

// fakeTx records what it was asked to do, and can be made to fail in the ways
// the real transaction can.
type fakeTx struct {
	// Keyed by caller AND operation id, because that is the real primary key:
	// operation_outcome is (caller_user_id, operation_id). A fake keyed by
	// operation id alone would hand one caller's outcome to another and report a
	// bug the product does not have.
	recorded map[string]push.Result
	applied  []push.Operation
	commits  int
	rolls    int

	// applyResult is what Apply returns for an operation with no recorded
	// outcome. Left nil, Apply synthesises an applied outcome.
	applyResult *push.Result

	beginErr    error
	commitErr   error
	readErr     error
	applyErr    error
	applyCalls  int
	commitCalls int
}

func newTx() *fakeTx {
	return &fakeTx{recorded: map[string]push.Result{}}
}

// aCaller is the caller every test uses unless it is testing the caller itself.
func aCaller() push.Caller {
	return push.Caller{ID: "user-1", WriteContext: "reserve-north", Grants: []string{"reserve-north"}}
}

func (f *fakeTx) Begin(context.Context) (push.Tx, error) {
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	return f, nil
}

func (f *fakeTx) RecordedOutcome(_ context.Context, callerID, operationID string) (push.Result, bool, error) {
	if f.readErr != nil {
		return push.Result{}, false, f.readErr
	}
	r, ok := f.recorded[callerID+"\x00"+operationID]
	return r, ok, nil
}

func (f *fakeTx) Apply(_ context.Context, caller push.Caller, op push.Operation) (push.Result, error) {
	f.applyCalls++
	if f.applyErr != nil {
		return push.Result{}, f.applyErr
	}
	f.applied = append(f.applied, op)

	res := push.Result{
		OperationID: op.OperationID,
		Entity:      op.Entity,
		EntityID:    op.EntityID,
		Outcome:     push.OutcomeApplied,
		NewRevision: 1,
	}
	if f.applyResult != nil {
		res = *f.applyResult
	}

	// A real store records the outcome here, in this transaction. Reproducing
	// that is the point of the fake: without it, a test could pass while the
	// replay rule was never exercised.
	f.recorded[caller.ID+"\x00"+op.OperationID] = res
	return res, nil
}

func (f *fakeTx) Commit(context.Context) error {
	f.commits++
	return f.commitErr
}
func (f *fakeTx) Rollback(context.Context) error { f.rolls++; return nil }

func (f *fakeTx) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

// fakeStore hands Begin a tx it already holds.
type fakeStore struct{ tx *fakeTx }

func (s fakeStore) Begin(context.Context) (push.Tx, error) { return s.tx.Begin(context.Background()) }

func anOperation(id string) push.Operation {
	rev := int64(3)
	return push.Operation{
		OperationID:  id,
		Entity:       "log_book_entry",
		Kind:         "create",
		EntityID:     "entity-" + id,
		BaseRevision: &rev,
		CapturedAt:   time.Date(2026, 10, 4, 6, 14, 0, 0, time.UTC),
		Payload:      map[string]any{"species_code": "LEOP", "count": 2},
	}
}

func aService(tx *fakeTx) *push.Service { return push.New(fakeStore{tx: tx}) }

// A successful push returns one result per operation, in the order sent.
func TestAPushReturnsOneResultPerOperationInOrder(t *testing.T) {
	tx := newTx()
	got, err := aService(tx).Push(context.Background(), aCaller(), []push.Operation{
		anOperation("op-1"), anOperation("op-2"), anOperation("op-3"),
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d results for 3 operations", len(got))
	}
	for i, want := range []string{"op-1", "op-2", "op-3"} {
		if got[i].OperationID != want {
			t.Errorf("result %d is %q, want %q. A client matching by position "+
				"would attribute the wrong outcome to the wrong change.", i, got[i].OperationID, want)
		}
		if got[i].Outcome != push.OutcomeApplied {
			t.Errorf("%s: outcome %q", want, got[i].Outcome)
		}
	}
	if tx.commits != 1 {
		t.Errorf("%d commits for one batch", tx.commits)
	}
}

// Rule 7: idempotency is keyed by operation id, never entity id. The retry here
// carries a deliberately DIFFERENT payload, and the point is that it does not
// matter.
func TestARetryReturnsTheOriginallyRecordedOutcomeNotAFreshEvaluation(t *testing.T) {
	tx := newTx()
	svc := aService(tx)
	ctx := context.Background()

	first, err := svc.Push(ctx, aCaller(), []push.Operation{anOperation("op-1")})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Outcome != push.OutcomeApplied {
		t.Fatalf("first attempt: %q", first[0].Outcome)
	}

	// The client crashed before it saw the answer and retries, having since
	// changed its mind about the count. Re-evaluating would apply the new
	// payload, which is not what "retry" means.
	retry := anOperation("op-1")
	retry.Payload = map[string]any{"species_code": "LION", "count": 9}

	second, err := svc.Push(ctx, aCaller(), []push.Operation{retry})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Outcome != push.OutcomeApplied {
		t.Errorf("retry outcome %q, want applied", second[0].Outcome)
	}
	if tx.applyCalls != 1 {
		t.Errorf("the retry called Apply %d times in total. A retry must not "+
			"reach the entity change at all.", tx.applyCalls)
	}
}

// The same rule, from the other side: a retry of something that was REFUSED
// must get the refusal back, not a second chance.
func TestARetryOfARefusalIsRefusedAgainRatherThanReevaluated(t *testing.T) {
	tx := newTx()
	refused := push.Result{
		OperationID: "op-1", Entity: "log_book_entry", EntityID: "entity-op-1",
		Outcome: push.OutcomeRefused, ErrorCode: "revision_conflict",
		ServerState: map[string]any{"count": 4},
	}
	tx.recorded["user-1\x00op-1"] = refused

	got, err := aService(tx).Push(context.Background(), aCaller(),
		[]push.Operation{anOperation("op-1")})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("a retry of a refused operation came back %q. It must come "+
			"back refused, or a client can turn a rejection into an acceptance by "+
			"changing the payload and resending.", got[0].Outcome)
	}
	if tx.applyCalls != 0 {
		t.Errorf("a retried refusal reached Apply %d times", tx.applyCalls)
	}
}

// An operation id is a string somebody chose, so a recorded outcome belongs to
// the caller who made it and to nobody else.
func TestARecordedOutcomeBelongsToTheCallerWhoEarnedIt(t *testing.T) {
	tx := newTx()
	svc := aService(tx)
	ctx := context.Background()

	if _, err := svc.Push(ctx, aCaller(), []push.Operation{anOperation("op-1")}); err != nil {
		t.Fatal(err)
	}

	// A different caller presenting the same operation id. If the lookup were
	// not scoped by caller, this would be told the operation had been applied and
	// would silently drop its own change.
	got, err := svc.Push(ctx, push.Caller{ID: "user-2", WriteContext: "reserve-north", Grants: []string{"reserve-north"}}, []push.Operation{anOperation("op-1")})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeApplied {
		t.Errorf("the second caller got %q; it is applying this for the first "+
			"time, so the first caller's outcome must not have been handed to it.",
			got[0].Outcome)
	}
	if tx.applyCalls != 2 {
		t.Errorf("Apply ran %d times across two callers; want 2. The recorded "+
			"outcome of one caller must not short-circuit another's.", tx.applyCalls)
	}
}

// The batch is not all-or-nothing: one refusal must not discard the rest.
func TestARefusedOperationDoesNotDiscardTheRestOfTheBatch(t *testing.T) {
	tx := newTx()
	tx.recorded["user-1\x00op-2"] = push.Result{
		OperationID: "op-2", Entity: "log_book_entry", EntityID: "entity-op-2",
		Outcome: push.OutcomeRefused, ErrorCode: "revision_conflict",
	}

	got, err := aService(tx).Push(context.Background(), aCaller(), []push.Operation{
		anOperation("op-1"), anOperation("op-2"), anOperation("op-3"),
	})
	if err != nil {
		t.Fatalf("a refusal inside a batch must not fail the batch: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("%d results for 3 operations", len(got))
	}
	if got[1].Outcome != push.OutcomeRefused {
		t.Errorf("op-2 came back %q", got[1].Outcome)
	}
	if got[0].Outcome != push.OutcomeApplied || got[2].Outcome != push.OutcomeApplied {
		t.Errorf("the operations either side of a refusal came back %q and %q. A "+
			"device that recorded three things and had one refused needs the "+
			"other two.", got[0].Outcome, got[2].Outcome)
	}
	if tx.commits != 1 {
		t.Errorf("%d commits; the batch commits once", tx.commits)
	}
}

// If the commit fails, nothing is applied and the caller must be told the whole
// batch failed rather than handed results that look like successes.
func TestAFailedCommitReportsTheWholeBatchFailed(t *testing.T) {
	tx := newTx()
	tx.commitErr = errors.New("connection lost")

	got, err := aService(tx).Push(context.Background(), aCaller(),
		[]push.Operation{anOperation("op-1")})
	if err == nil {
		t.Fatal("a failed commit returned no error")
	}
	if got != nil {
		t.Errorf("results were returned alongside a failed commit: %+v. They "+
			"describe writes that did not happen.", got)
	}
	if tx.rolls == 0 {
		t.Error("no rollback after a failed commit")
	}
}

// An unreadable outcome table means the operation may already have been
// applied. Deferring is the honest answer; refusing would claim the change was
// rejected when it might already be committed.
func TestAnUnreadableOutcomeTableDefersRatherThanRefuses(t *testing.T) {
	tx := newTx()
	tx.readErr = errors.New("permission denied for table operation_outcome")

	got, err := aService(tx).Push(context.Background(), aCaller(),
		[]push.Operation{anOperation("op-1")})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeDeferred {
		t.Errorf("outcome %q, want deferred. Refusing here would tell the client "+
			"its change was rejected when it may already be applied.", got[0].Outcome)
	}
	if tx.applyCalls != 0 {
		t.Errorf("Apply ran %d times with the outcome table unreadable", tx.applyCalls)
	}
}

// An operation with no id cannot be retried safely, so it is refused rather
// than applied-and-forgotten.
func TestAnOperationWithNoIDIsRefusedRatherThanAppliedAndForgotten(t *testing.T) {
	tx := newTx()
	op := anOperation("")
	op.OperationID = ""

	got, err := aService(tx).Push(context.Background(), aCaller(), []push.Operation{op})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Outcome != push.OutcomeRefused {
		t.Errorf("outcome %q, want refused", got[0].Outcome)
	}
	if got[0].ErrorCode != "missing_operation_id" {
		t.Errorf("error code %q", got[0].ErrorCode)
	}
	if tx.applyCalls != 0 {
		t.Errorf("an unidentifiable operation reached Apply %d times", tx.applyCalls)
	}
}

// The outcome table is keyed by caller, so a batch with no caller cannot be
// recorded at all.
func TestABatchWithNoCallerIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	tx := newTx()
	_, err := aService(tx).Push(context.Background(), push.Caller{WriteContext: "reserve-north"},
		[]push.Operation{anOperation("op-1")})
	if !errors.Is(err, push.ErrNoCaller) {
		t.Errorf("got %v, want ErrNoCaller", err)
	}
	if tx.commits != 0 || tx.applied != nil {
		t.Error("a batch with no caller reached the database")
	}
}

// A token naming no active context cannot be granted a record. Defaulting would
// mean inventing a context, and a record written into an invented context is
// invisible to every guide who holds the grant that actually matters.
func TestACallerWithNoActiveContextIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	tx := newTx()
	_, err := aService(tx).Push(context.Background(), push.Caller{ID: "user-1"},
		[]push.Operation{anOperation("op-1")})
	if !errors.Is(err, push.ErrNoWriteContext) {
		t.Errorf("got %v, want ErrNoWriteContext", err)
	}
	if tx.commits != 0 || tx.applied != nil {
		t.Error("a caller with no active context reached the database")
	}
}

// An empty batch means the client has mis-built its queue, and an empty success
// would hide that until the data had aged out.
func TestAnEmptyBatchIsRefusedRatherThanAnsweredWithAnEmptySuccess(t *testing.T) {
	tx := newTx()
	_, err := aService(tx).Push(context.Background(), aCaller(), nil)
	if !errors.Is(err, push.ErrNoOperations) {
		t.Errorf("got %v, want ErrNoOperations", err)
	}
	if tx.commits != 0 {
		t.Error("an empty batch committed a transaction")
	}
}

// The transaction is opened once for the batch, not per operation.
func TestTheBatchRunsInOneTransaction(t *testing.T) {
	tx := newTx()
	if _, err := aService(tx).Push(context.Background(), aCaller(), []push.Operation{
		anOperation("op-1"), anOperation("op-2"),
	}); err != nil {
		t.Fatal(err)
	}
	if tx.commits != 1 {
		t.Errorf("%d commits for a batch of 2; want 1", tx.commits)
	}
	if tx.rolls != 1 {
		t.Errorf("%d rollbacks; the deferred rollback after a commit should "+
			"always run once", tx.rolls)
	}
}
