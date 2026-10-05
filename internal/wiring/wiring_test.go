package wiring

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"crypto/rsa"

	"field-service/internal/authn"
	"field-service/internal/httpapi"
	"field-service/internal/revocation"
)

// The wiring is the one place the three pieces meet, so these tests assert that
// each is reached with the values it was given rather than that each works alone.
// The pieces have their own suites; what could go wrong here is a value arriving
// in the wrong unit, or the read boundary being set from the wrong claim.

// --- fakes -----------------------------------------------------------------

type fakeTx struct {
	statements []string
	args       [][]any
	committed  bool
	rolledBack bool
	execErr    error
	commitErr  error
	// onExec runs while a statement is being recorded, which is how a test
	// observes the transaction-local setting from inside fn.
	onExec func(sql string, args []any)
}

func (t *fakeTx) Exec(_ context.Context, sql string, args ...any) (PgxCommandTag, error) {
	t.statements = append(t.statements, sql)
	t.args = append(t.args, args)
	if t.onExec != nil {
		t.onExec(sql, args)
	}
	return zeroTag{}, t.execErr
}

func (t *fakeTx) Query(context.Context, string, ...any) (PgxRows, error) {
	return nil, nil
}
func (t *fakeTx) QueryRow(context.Context, string, ...any) PgxRow { return nil }
func (t *fakeTx) Commit(context.Context) error {
	t.committed = true
	return t.commitErr
}

// Rollback models pgx: rolling back a transaction that has already committed is
// a no-op that reports the transaction is closed. Recording it as a rollback made
// a correctly committed transaction look rolled back, which is a false failure
// caused by the fake being more dramatic than the thing it stands in for.
func (t *fakeTx) Rollback(context.Context) error {
	if t.committed {
		return errTxClosed
	}
	t.rolledBack = true
	return nil
}

// zeroTag is a PgxCommandTag. The interface cannot be composite-literalised, so
// the fake needs a concrete value to hand back.
// errTxClosed mirrors pgx.ErrTxClosed without importing pgx into a test that
// does not otherwise need it.
var errTxClosed = errors.New("tx is closed")

type zeroTag struct{}

func (zeroTag) RowsAffected() int64 { return 0 }

type fakePool struct {
	tx  *fakeTx
	err error
}

func (p *fakePool) Begin(context.Context) (PgxTx, error) { return p.tx, p.err }

// --- the adapter -----------------------------------------------------------

// The revocation rule consumes the token's own epoch and issue time. If the
// adapter got either wrong the denylist would be asked about a different token
// than the one presented, and every check would still pass.
func TestTheAdapterPassesTheTokensOwnEpochAndIssueTime(t *testing.T) {
	issued := time.Now().Add(-2 * time.Hour).Truncate(time.Second)

	// The adapter is what the guard actually holds, so exercise it through the
	// guard's own interface rather than reaching into it.
	var got httpapi.Principal = claims{authn.Principal{Claims: authn.Claims{
		Subject:  "guide-7",
		Epoch:    42,
		IssuedAt: issued.Unix(),
	}}}

	if got.Epoch() != 42 {
		t.Errorf("epoch = %d, want 42", got.Epoch())
	}
	at, ok := got.IssuedAt()
	if !ok {
		t.Fatal("an issued-at claim was reported as absent")
	}
	if !at.Equal(issued) {
		t.Errorf("IssuedAt = %v, want %v", at, issued)
	}
}

// A token with no issue time must report absent, not the zero time. The zero time
// is before every revocation, so returning it would make the denylist refuse the
// token for a reason that looks correct and is not.
func TestAMissingIssueTimeIsReportedAbsentNotAsTheEpoch(t *testing.T) {
	for _, issuedAt := range []int64{0, -1} {
		var got httpapi.Principal = claims{authn.Principal{Claims: authn.Claims{
			IssuedAt: issuedAt,
		}}}
		at, ok := got.IssuedAt()
		if ok {
			t.Errorf("iat=%d reported as %v; a missing claim must be absent so "+
				"the caller can refuse rather than compare against the epoch", issuedAt, at)
		}
		if !at.IsZero() {
			t.Errorf("iat=%d returned a non-zero time alongside ok=false", issuedAt)
		}
	}
}

// The grant set and the active context are different claims and the whole
// isolation depends on the right one reaching the database.
func TestTheGrantSetAndTheActiveContextAreNotConfused(t *testing.T) {
	var got httpapi.Principal = claims{authn.Principal{Claims: authn.Claims{
		ActiveContext: "southern",
		Grants:        []string{"northern", "southern"},
	}}}

	if len(got.Grants()) != 2 {
		t.Fatalf("grants = %v, want two contexts", got.Grants())
	}
	if got.ActiveContext() != "southern" {
		t.Errorf("active context = %q", got.ActiveContext())
	}
}

// --- the session -----------------------------------------------------------

func TestTheGrantsAreSetInsideTheTransactionAndBeforeTheHandlerRuns(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	ranAfterSetting := false
	err := s.InTx(context.Background(), "user-1", []string{"northern", "southern"},
		func(context.Context) error {
			ranAfterSetting = len(tx.statements) > 0 &&
				strings.Contains(tx.statements[0], "set_config")
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !ranAfterSetting {
		t.Error("the handler ran before the read boundary was set")
	}
	if !tx.committed {
		t.Error("the transaction was not committed")
	}
	if tx.rolledBack {
		t.Error("the transaction was rolled back after a successful commit")
	}
}

// The values are bound, not interpolated. This is the injection point: the grants
// come from a token, and SET takes no bind parameters, which is why set_config is
// used at all.
func TestTheGrantsAreBoundRatherThanInterpolated(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	if err := s.InTx(context.Background(), "user-1", []string{"northern"}, func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if len(tx.args) == 0 {
		t.Fatal("the setting was executed with no bound arguments")
	}
	statement := tx.statements[0]
	if strings.Contains(statement, "northern") {
		t.Errorf("the grant appears in the statement text: %s", statement)
	}
	args := tx.args[0]
	if len(args) != 2 {
		t.Fatalf("expected two bound arguments, got %d", len(args))
	}
	if args[0] != "app.context_grants" {
		t.Errorf("first bound argument = %v, want the setting name", args[0])
	}
	if args[1] != "northern" {
		t.Errorf("second bound argument = %v, want the grant set", args[1])
	}
	// The third argument to set_config is what makes it local, and the pooling
	// safety depends on it.
	if !strings.Contains(statement, "true") {
		t.Errorf("set_config was not called with a transaction-local flag: %s", statement)
	}
}

// A grant containing a comma cannot be represented in this format. Dropping it
// would widen the set rather than narrow it, so it is refused.
func TestAMalformedContextCodeIsRefusedRatherThanSilentlyDropped(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	for _, grants := range [][]string{
		{"northern,southern"},
		{"northern", ""},
	} {
		err := s.InTx(context.Background(), "user-1", grants, func(context.Context) error {
			t.Error("the handler ran despite a malformed grant set")
			return nil
		})
		if err == nil {
			t.Errorf("grants %v were accepted", grants)
		}
		if len(tx.statements) != 0 {
			t.Errorf("grants %v reached the database as %v", grants, tx.statements)
		}
	}
}

// An empty grant set is refused rather than run. Setting it empty makes the
// policies match nothing, which is safe but converts a caller's mistake into an
// empty answer that looks like a correct one.
func TestAnEmptyGrantSetIsRefusedBeforeAnyStatementRuns(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	err := s.InTx(context.Background(), "user-1", nil, func(context.Context) error {
		t.Error("the handler ran with no grants")
		return nil
	})
	if !errors.Is(err, ErrNoGrants) {
		t.Errorf("expected ErrNoGrants, got %v", err)
	}
	if len(tx.statements) != 0 {
		t.Errorf("statements ran anyway: %v", tx.statements)
	}
}

// A handler that fails must not commit. A committed change that the handler then
// reported as failed is the worst outcome available.
func TestAFailedHandlerRollsBackRatherThanCommits(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	sentinel := errors.New("write failed")
	err := s.InTx(context.Background(), "user-1", []string{"northern"},
		func(context.Context) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("the handler's error did not come back: %v", err)
	}
	if tx.committed {
		t.Error("a transaction whose handler failed was committed")
	}
	if !tx.rolledBack {
		t.Error("a failed transaction was not rolled back")
	}
}

// --- the shapes meet --------------------------------------------------------

// The point of the whole exercise: a real validator, the real denylist and this
// session all satisfy the guard's interfaces without an adapter that could get a
// unit or a claim wrong.
func TestTheRealImplementationsSatisfyTheGuardsInterfaces(t *testing.T) {
	var (
		_ httpapi.Verifier          = NewVerifier(nil)
		_ httpapi.RevocationChecker = revocation.Checker(nil)
		_ httpapi.Session           = NewSession(nil)
	)
}

// The revocation checker is satisfied without an adapter, which is deliberate:
// the alternative is an adapter converting a time to an int64 somewhere, and a
// unit error there would refuse or admit every request by a factor of a thousand.
func TestRevocationCheckerNeedsNoAdapter(t *testing.T) {
	var checker httpapi.RevocationChecker = revocation.Static{}
	revoked, err := checker.Revoked(context.Background(), "u", time.Now(), 1)
	if err != nil || revoked {
		t.Errorf("Static via the guard's interface: revoked=%v err=%v", revoked, err)
	}
}

// staticKeys stands in for a JWKS cache. Only the shape matters here: the
// adapter is exercised through the guard's interface, not through a validator.
type staticKeys struct{}

func (staticKeys) ResolveKey(string) (*rsa.PublicKey, error) { return nil, nil }

// The caller setting is not a nicety. operation_outcome's policy compares its
// caller column against this identifier, so a transaction that does not set it
// cannot read or write that table at all — and the failure presents as an
// operation outcome that is mysteriously never found, which is a very quiet
// wrong answer.
func TestTheCallersOwnIdentityIsSetInsideTheTransaction(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	if err := s.InTx(context.Background(), "user-42", []string{"northern"},
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("InTx: %v", err)
	}

	found := false
	for i, stmt := range tx.statements {
		if !strings.Contains(stmt, "set_config") {
			continue
		}
		args := tx.args[i]
		if len(args) != 2 {
			t.Fatalf("set_config executed with %d bound arguments", len(args))
		}
		if args[0] == callerSetting {
			found = true
			if args[1] != "user-42" {
				t.Errorf("%s = %v, want the caller's own id", callerSetting, args[1])
			}
			if !strings.Contains(stmt, "true") {
				t.Errorf("%s was not set transaction-locally: %s", callerSetting, stmt)
			}
		}
	}
	if !found {
		t.Fatalf("%s was never set, so operation_outcome is unreachable", callerSetting)
	}
}

// Bound, not interpolated, for the same reason the grants are: the value comes
// from a token and this is the place that decides whose outcomes are readable.
func TestTheCallerSettingIsBoundRatherThanInterpolated(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	nasty := "user-42'; drop table sighting; --"
	if err := s.InTx(context.Background(), nasty, []string{"northern"},
		func(context.Context) error { return nil }); err != nil {
		t.Fatalf("InTx: %v", err)
	}

	passedIntact := false
	for _, stmt := range tx.statements {
		if strings.Contains(stmt, "drop table") {
			t.Fatalf("the caller id reached the statement text: %s", stmt)
		}
	}
	for i, args := range tx.args {
		if len(args) == 2 && args[0] == callerSetting && args[1] == nasty {
			passedIntact = true
			_ = tx.statements[i]
		}
	}
	if !passedIntact {
		t.Error("the caller id did not arrive intact as a bound argument, so it " +
			"was either interpolated or dropped")
	}
}

// A transaction opened for nobody is refused up front. Left to the first query
// touching operation_outcome it would fail with an obscure error instead, and
// the caller would have no way to tell which of their two inputs was at fault.
func TestATransactionForNobodyIsRefused(t *testing.T) {
	tx := &fakeTx{}
	s := NewSession(&fakePool{tx: tx})

	err := s.InTx(context.Background(), "   ", []string{"northern"},
		func(context.Context) error {
			t.Error("the handler ran for a transaction with no caller")
			return nil
		})
	if !errors.Is(err, ErrNoCaller) {
		t.Errorf("err = %v, want ErrNoCaller", err)
	}
	if len(tx.statements) != 0 {
		t.Errorf("%d statements ran for a transaction with no caller, which is "+
			"what the check exists to prevent", len(tx.statements))
	}
}
