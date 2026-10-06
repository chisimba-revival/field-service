package signoff

import (
	"context"
	"errors"
	"testing"
)

// The read rule, tested where it is decided.
//
// The rule is three readers and nobody else: the trainee assessed, the mentor
// who wrote it, and a programme administrator in the same context. Every test
// below is shaped by a way the rule could be broken without any of the obvious
// assertions noticing.

// A trainee may read their own sign-off. Asserted as a positive case, because a
// test suite of refusals would pass with the rule inverted — every refusal still
// refused, and the one thing a trainee is entitled to silently stopped working.
func TestATraineeMayReadTheirOwnSignoff(t *testing.T) {
	st := &fakeStore{record: Record{ID: "11111111-1111-4111-8111-111111111111", TraineeID: "42"}}
	svc := New(st, nil, nil)

	rec, err := svc.Get(context.Background(), "42", "North", false, "11111111-1111-4111-8111-111111111111")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rec.ID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("got %+v", rec)
	}
	if st.gotCtx != "North" {
		t.Fatalf("read context %q, want the caller's own", st.gotCtx)
	}
	if st.gotAdmin {
		t.Fatal("a plain caller was treated as an administrator")
	}
}

// What the store is asked matters as much as what it answers. The administrator
// bit must arrive as a boolean: a role name passed down would have the store
// compare a database column against a string meaning something else entirely,
// which cannot be caught by asserting the response.
func TestTheReadRuleReachesTheStoreAsABoolean(t *testing.T) {
	st := &fakeStore{record: Record{ID: "11111111-1111-4111-8111-111111111111"}}
	svc := New(st, nil, nil)

	if _, err := svc.Get(context.Background(), "42", "North", true, "11111111-1111-4111-8111-111111111111"); err != nil {
		t.Fatal(err)
	}
	if !st.gotAdmin {
		t.Fatal("an administrator reached the store as an ordinary caller, so the rule was not applied")
	}
	if st.gotCaller != "42" || st.gotID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("store asked for %q/%q", st.gotCaller, st.gotID)
	}
}

// A sign-off this caller may not see must be indistinguishable from one that does
// not exist. Asserted as NotFound rather than as a refusal, because the two are
// different answers and the difference between them is what makes an endpoint a
// way of discovering which ids exist.
func TestASignoffTheCallerMayNotSeeIsNotFound(t *testing.T) {
	st := &fakeStore{} // no record: the store found nothing this caller may see
	svc := New(st, nil, nil)

	_, err := svc.Get(context.Background(), "99", "North", false, "11111111-1111-4111-8111-111111111111")
	if !NotFound(err) {
		t.Fatalf("got %v, want NotFound — a forbidden answer would leak which ids exist", err)
	}
	if errors.Is(err, ErrNotFound) == false {
		t.Fatal("NotFound must be recognisable through errors.Is")
	}
}

// Without a context there is nobody to decide who the caller is relative to, and
// a rule that matched everyone would be the wrong answer. Refused rather than
// answered, so a caller with no active context is told why instead of receiving a
// sign-off.
func TestAReadWithoutAContextIsRefused(t *testing.T) {
	svc := New(&fakeStore{}, nil, nil)

	for _, caller := range []string{"42", ""} {
		_, err := svc.Get(context.Background(), caller, "", false, "11111111-1111-4111-8111-111111111111")
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != "no_read_context" {
			t.Fatalf("caller %q got %v, want no_read_context", caller, err)
		}
	}
}

// A malformed id is refused before the store is reached. An id that is not a
// uuid cannot match anything, and passing it through would make a query whose
// only outcome is "nothing" indistinguishable from a real miss.
func TestAMalformedIdentifierIsRefusedBeforeTheStore(t *testing.T) {
	st := &fakeStore{record: Record{ID: "whatever"}}
	svc := New(st, nil, nil)

	for _, id := range []string{"", "not-a-uuid", "11111111-1111-4111-8111"} {
		_, err := svc.Get(context.Background(), "42", "North", false, id)
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != "signoff_id_malformed" {
			t.Fatalf("id %q got %v, want signoff_id_malformed", id, err)
		}
	}
	if st.gotID != "" {
		t.Fatalf("the store was reached with %q", st.gotID)
	}
}

// A store fault is a fault, not an answer. If it were folded into NotFound a
// database outage would read as "you have no sign-offs", which is the one thing a
// trainee must never be told about their own training.
func TestAStoreFaultIsNotReportedAsNotFound(t *testing.T) {
	svc := New(&fakeStore{err: errors.New("connection refused")}, nil, nil)

	_, err := svc.Get(context.Background(), "42", "North", false, "11111111-1111-4111-8111-111111111111")
	if NotFound(err) {
		t.Fatal("a database fault was reported as 'no sign-off', which would read as 'you have not been assessed'")
	}
	if err == nil {
		t.Fatal("a store fault produced no error at all")
	}
}
