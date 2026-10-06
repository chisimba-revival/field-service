package signoff

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeStore struct {
	called   int
	record   Record
	err      error
	gotCtx   string
	gotAsses []Assessment
}

func (f *fakeStore) Create(_ context.Context, callerID string, v Validated) (Record, error) {
	f.called++
	f.gotCtx = v.Context
	f.gotAsses = v.Assessments
	return f.record, f.err
}

type fakeCatalogue struct {
	known map[string]bool
	err   error
	asked []string
}

func (f *fakeCatalogue) Assessable(_ context.Context, codes []string) (map[string]bool, error) {
	f.asked = codes
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]bool{}
	for _, c := range codes {
		out[c] = f.known[c]
	}
	return out, nil
}

func goodRequest() Request {
	return Request{
		OperationID: "11111111-1111-4111-8111-111111111111",
		EntryID:     "22222222-2222-4222-8222-222222222222",
		OutingID:    "33333333-3333-4333-8333-333333333333",
		TraineeID:   "42",
		MentorID:    "7",
		Assessments: []Assessment{{Code: "NAVMAP", Rating: Competent, Evidence: "Navigated to three waypoints unaided."}},
	}
}

func harness(t *testing.T) (*Service, *fakeStore, *fakeCatalogue) {
	t.Helper()
	st := &fakeStore{record: Record{ID: "x", Status: StatusDraft, Revision: 1}}
	cat := &fakeCatalogue{known: map[string]bool{"NAVMAP": true, "SAFFIR": true}}
	outing := func(_ context.Context, _, _ string) (bool, error) { return true, nil }
	return New(st, cat, outing), st, cat
}

// The rules that make a sign-off worth reading. Each is asserted on the refusal
// code rather than the message, because the code is what a client branches on
// and the message is what a person reads.
func TestRefusals(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*Request)
		codes string
	}{
		{"no operation id", func(r *Request) { r.OperationID = "" }, "signoff_without_operation_id"},
		{"no id", func(r *Request) { r.EntryID = "" }, "signoff_without_id"},
		{"malformed id", func(r *Request) { r.EntryID = "not-a-uuid" }, "signoff_id_malformed"},
		{"no outing", func(r *Request) { r.OutingID = "" }, "signoff_without_outing_id"},
		{"no trainee", func(r *Request) { r.TraineeID = "" }, "signoff_without_trainee"},
		{"no mentor", func(r *Request) { r.MentorID = "" }, "signoff_without_mentor"},
		{"no assessments", func(r *Request) { r.Assessments = nil }, "signoff_without_assessments"},
		{"too many", func(r *Request) {
			var many []Assessment
			for i := 0; i < 21; i++ {
				many = append(many, Assessment{Code: "NAVMAP", Rating: Competent, Evidence: "e"})
			}
			r.Assessments = many
		}, "signoff_without_assessments"},
		{"no code", func(r *Request) { r.Assessments[0].Code = "" }, "signoff_assessment_without_code"},
		{"duplicate code", func(r *Request) {
			r.Assessments = append(r.Assessments, Assessment{Code: "NAVMAP", Rating: Developing, Evidence: "e"})
		}, "signoff_assessment_duplicate"},
		{"unknown rating", func(r *Request) { r.Assessments[0].Rating = "expert_plus" }, "unknown_rating"},
		{"no evidence", func(r *Request) { r.Assessments[0].Evidence = "  " }, "signoff_assessment_without_evidence"},
		{"unknown competency", func(r *Request) { r.Assessments[0].Code = "NOPE1" }, "unknown_competency"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, st, _ := harness(t)
			req := goodRequest()
			c.mut(&req)
			_, err := svc.Create(context.Background(), "42", "Alpha", req)
			if err == nil {
				t.Fatalf("expected %s, got a stored record", c.codes)
			}
			var ref *Refusal
			if !errors.As(err, &ref) {
				t.Fatalf("expected a Refusal, got %T: %v", err, err)
			}
			if ref.Code != c.codes {
				t.Fatalf("got %s, want %s", ref.Code, c.codes)
			}
			if st.called != 0 {
				t.Fatal("the store was reached despite a refusal")
			}
		})
	}
}

// not_observed carries evidence like every other rating. It is the rating most
// likely to be filed bare — a mentor who saw nothing has little to write — and it
// is precisely the one where a bare row says nothing about why.
func TestNotObservedStillRequiresEvidence(t *testing.T) {
	svc, _, _ := harness(t)
	req := goodRequest()
	req.Assessments[0].Rating = NotObserved
	req.Assessments[0].Evidence = ""

	_, err := svc.Create(context.Background(), "42", "Alpha", req)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "signoff_assessment_without_evidence" {
		t.Fatalf("not_observed with no evidence gave %v, want the evidence refusal", err)
	}
}

// A retired competency is not assessable, while it stays readable. Offering it on
// a new sign-off would invite a judgement against a withdrawn standard.
func TestARetiredCompetencyIsNotAssessable(t *testing.T) {
	svc, _, cat := harness(t)
	req := goodRequest()
	req.Assessments[0].Code = "OLDCO"

	_, err := svc.Create(context.Background(), "42", "Alpha", req)
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "unknown_competency" {
		t.Fatalf("got %v, want unknown_competency", err)
	}
	if len(cat.asked) == 0 || cat.asked[0] != "OLDCO" {
		t.Fatalf("the catalogue was asked about %v", cat.asked)
	}
}

// A caller with no active context has nowhere to write, and that is 403 rather
// than 422 — there is nothing wrong with what they asked for.
func TestNoWriteContextIsRefused(t *testing.T) {
	svc, st, _ := harness(t)
	for _, ctx := range []string{"", "   "} {
		_, err := svc.Create(context.Background(), "42", ctx, goodRequest())
		var ref *Refusal
		if !errors.As(err, &ref) || ref.Code != "no_write_context" {
			t.Fatalf("context %q gave %v, want no_write_context", ctx, err)
		}
	}
	if st.called != 0 {
		t.Fatal("the store was reached with no context")
	}
}

// The write context is the caller's, never the request's. Request has no context
// field at all, so there is nothing to ignore.
func TestTheWriteContextIsTheCallersOwn(t *testing.T) {
	svc, st, _ := harness(t)
	if _, err := svc.Create(context.Background(), "42", "Alpha", goodRequest()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.gotCtx != "Alpha" {
		t.Fatalf("stored in %q, want Alpha", st.gotCtx)
	}
}

// A code is normalised once, in validation, so the store cannot write a form that
// differs from the one the catalogue approved.
func TestCodesAreNormalisedBeforeTheStore(t *testing.T) {
	svc, st, cat := harness(t)
	req := goodRequest()
	req.Assessments[0].Code = "  navmap  "

	if _, err := svc.Create(context.Background(), "42", "Alpha", req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if cat.asked[0] != "NAVMAP" {
		t.Fatalf("catalogue asked about %q", cat.asked[0])
	}
	if st.gotAsses[0].Code != "NAVMAP" {
		t.Fatalf("stored %q", st.gotAsses[0].Code)
	}
}

// The mentor's order survives, because the position column exists for it.
func TestAssessmentOrderIsPreserved(t *testing.T) {
	svc, st, cat := harness(t)
	req := goodRequest()
	cat.known["SAFFIR"] = true
	req.Assessments = []Assessment{
		{Code: "SAFFIR", Rating: Expert, Evidence: "handled a rifle correctly throughout"},
		{Code: "NAVMAP", Rating: Developing, Evidence: "needed a bearing twice"},
	}
	if _, err := svc.Create(context.Background(), "42", "Alpha", req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if st.gotAsses[0].Code != "SAFFIR" || st.gotAsses[1].Code != "NAVMAP" {
		t.Fatalf("order changed: %v", st.gotAsses)
	}
}

// A catalogue fault is not a refusal. Swallowing it as one would tell a mentor
// their competency code is wrong when the answer is that the database was
// unreachable.
func TestACatalogueFaultIsNotARefusal(t *testing.T) {
	st := &fakeStore{}
	cat := &fakeCatalogue{err: errors.New("connection refused")}
	svc := New(st, cat, func(context.Context, string, string) (bool, error) { return true, nil })

	_, err := svc.Create(context.Background(), "42", "Alpha", goodRequest())
	if err == nil {
		t.Fatal("expected the fault to surface")
	}
	var ref *Refusal
	if errors.As(err, &ref) {
		t.Fatalf("a database fault was reported as the refusal %s", ref.Code)
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("the original fault was lost: %v", err)
	}
	if st.called != 0 {
		t.Fatal("the store was reached despite a catalogue fault")
	}
}

// An outing in another context is refused exactly as one that does not exist.
// A message that distinguished them would be an oracle for walking the id space.
func TestAnOutingInAnotherContextIsIndistinguishableFromAnAbsentOne(t *testing.T) {
	var seen []string
	svc := New(&fakeStore{}, &fakeCatalogue{known: map[string]bool{"NAVMAP": true}},
		func(_ context.Context, id, _ string) (bool, error) {
			seen = append(seen, id)
			return false, nil
		})
	_, err := svc.Create(context.Background(), "42", "Alpha", goodRequest())
	var ref *Refusal
	if !errors.As(err, &ref) || ref.Code != "unknown_outing" {
		t.Fatalf("got %v, want unknown_outing", err)
	}
	// The context is passed to the check, so a store that answered only on the id
	// could not have told the two apart either.
	if len(seen) != 1 || seen[0] != goodRequest().OutingID {
		t.Fatalf("outing check saw %v", seen)
	}
}

func TestIsUUID(t *testing.T) {
	good := []string{
		"22222222-2222-4222-8222-222222222222",
		"AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE",
	}
	for _, s := range good {
		if !isUUID(s) {
			t.Errorf("isUUID(%q) = false", s)
		}
	}
	bad := []string{
		"", "short", "22222222222242228222 222222222222",
		"22222222-2222-4222-8222-22222222222",   // one short
		"22222222-2222-4222-8222-2222222222222", // one long
		"22222222x2222-4222-8222-222222222222",  // wrong separator
		"2222222g-2222-4222-8222-222222222222",  // not hex
	}
	for _, s := range bad {
		if isUUID(s) {
			t.Errorf("isUUID(%q) = true", s)
		}
	}
}
