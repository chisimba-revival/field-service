package verify

import (
	"context"
	"errors"
	"testing"
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }
func rev(n int64) *int64   { return &n }

// A well-formed request, varied one field at a time by the tests below.
func good() Request {
	return Request{
		OperationID:  "0f8fad5b-d9cb-469f-a165-70867728950e",
		EntryID:      "7c9e6679-7425-40de-944b-e07fc1f90ae7",
		Context:      "Big 5 Reserve",
		Mentor:       "mentor-1",
		Outcome:      OutcomeVerified,
		BaseRevision: rev(1),
	}
}

type recordingStore struct {
	calls  int
	last   Request
	result Result
	fails  error
}

func (r *recordingStore) Verify(_ context.Context, req Request) (Result, error) {
	r.calls++
	r.last = req
	return r.result, r.fails
}

// The store must not be reached for a malformed request.
//
// Asserted on the call count rather than on the returned code, because a store
// that returns the right code by accident would satisfy a test asserting only
// the code — and a verification that writes a status and then fails on a missing
// reason leaves an entry verified by a mentor who never gave one.
func TestARefusedRequestNeverReachesTheStore(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Request)
		want string
	}{
		{"no operation id", func(r *Request) { r.OperationID = "" }, "verify_without_operation_id"},
		{"operation id is not a uuid", func(r *Request) { r.OperationID = "op-1" }, "verify_without_operation_id"},
		{"no entry id", func(r *Request) { r.EntryID = "" }, "verify_without_entry_id"},
		{"no context", func(r *Request) { r.Context = "" }, "verify_without_context"},
		{"blank mentor", func(r *Request) { r.Mentor = "   " }, "verify_without_mentor"},
		{"unknown outcome", func(r *Request) { r.Outcome = "approved" }, "unknown_outcome"},
		{"pending is not an answer", func(r *Request) { r.Outcome = "pending" }, "unknown_outcome"},
		{"no base revision", func(r *Request) { r.BaseRevision = nil }, "verify_without_base_revision"},
		{"correcting species with no reason", func(r *Request) { r.SpeciesCode = str("LION") }, "correction_without_reason"},
		{"correcting count with no reason", func(r *Request) { r.Count = num(5) }, "correction_without_reason"},
	}
	for _, c := range cases {
		req := good()
		c.mut(&req)
		st := &recordingStore{}
		got, err := New(st).Verify(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Outcome != "refused" || got.ErrorCode != c.want {
			t.Errorf("%s: got %s/%s, want refused/%s", c.name, got.Outcome, got.ErrorCode, c.want)
		}
		if st.calls != 0 {
			t.Errorf("%s: the store was called %d times for a request that should never reach it", c.name, st.calls)
		}
	}
}

// An unknown outcome must not become needs_review.
//
// This is the rule with the most expensive failure mode. Defaulting an
// unrecognised outcome to needs_review would make a typo read as a mentor being
// cautious — a different statement about a trainee's work than the one that was
// actually made — and it would do it silently.
func TestAnUnknownOutcomeIsRefusedRatherThanDefaulted(t *testing.T) {
	for _, o := range []Outcome{"", "VERIFIED", "Verified", "approve", "needs review", "rejected "} {
		req := good()
		req.Outcome = o
		got, _ := New(&recordingStore{}).Verify(context.Background(), req)
		if got.ErrorCode != "unknown_outcome" {
			t.Errorf("outcome %q gave %q, want refused as unknown_outcome", o, got.ErrorCode)
		}
	}
}

// The three real answers are all accepted, so the rule above is discriminating
// rather than refusing everything.
func TestEveryRealOutcomeIsAccepted(t *testing.T) {
	for _, o := range []Outcome{OutcomeVerified, OutcomeRejected, OutcomeNeedsReview} {
		st := &recordingStore{result: Result{Outcome: string(o)}}
		got, err := New(st).Verify(context.Background(), Request{
			OperationID: "0f8fad5b-d9cb-469f-a165-70867728950e",
			EntryID:     "7c9e6679-7425-40de-944b-e07fc1f90ae7",
			Context:     "Big 5 Reserve", Mentor: "mentor-1",
			Outcome: o, BaseRevision: rev(1),
		})
		if err != nil {
			t.Fatalf("%s: %v", o, err)
		}
		if got.Outcome != string(o) || st.calls != 1 {
			t.Errorf("%s: got %+v after %d store calls", o, got, st.calls)
		}
	}
}

// A correction with a reason reaches the store, and arrives with both fields
// intact rather than one having been consumed on the way.
func TestACorrectionWithAReasonIsApplied(t *testing.T) {
	req := good()
	req.Outcome = OutcomeVerified
	req.SpeciesCode = str("LION")
	req.Count = num(5)
	req.CorrectionReason = "Leopard; the coat pattern was rosettes and it was stocky."
	req.Notes = "Watched for four minutes from the vehicle."

	st := &recordingStore{result: Result{Outcome: "verified"}}
	got, err := New(st).Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.ErrorCode != "" || st.calls != 1 {
		t.Fatalf("refused a valid correction: %+v", got)
	}
	if st.last.SpeciesCode == nil || *st.last.SpeciesCode != "LION" {
		t.Error("the corrected species did not reach the store")
	}
	if st.last.Count == nil || *st.last.Count != 5 {
		t.Error("the corrected count did not reach the store")
	}
	if st.last.CorrectionReason == "" {
		t.Error("the reason did not reach the store")
	}
	if st.last.Context != "Big 5 Reserve" || st.last.Mentor != "mentor-1" {
		t.Error("context or mentor was not passed through as given")
	}
}

// Whitespace-only is not a reason. Trimmed before the check because a mentor
// pressing space and then return has given no explanation, and accepting it would
// satisfy the rule with nothing in it.
func TestAWhitespaceOnlyReasonIsNoReason(t *testing.T) {
	for _, reason := range []string{"", " ", "\t\n  "} {
		req := good()
		req.SpeciesCode = str("LION")
		req.CorrectionReason = reason
		st := &recordingStore{}
		got, _ := New(st).Verify(context.Background(), req)
		if got.ErrorCode != "correction_without_reason" {
			t.Errorf("reason %q gave %q, want correction_without_reason", reason, got.ErrorCode)
		}
		if st.calls != 0 {
			t.Errorf("reason %q reached the store", reason)
		}
	}
}

// A store failure is a fault, not a refusal.
//
// Returning it as a refusal would tell the caller its verification was declined
// when nothing was written, and a client acting on that would report the mentor's
// decision as rejected.
func TestAStoreFailureIsNotRefused(t *testing.T) {
	st := &recordingStore{fails: errors.New("connection refused")}
	got, err := New(st).Verify(context.Background(), good())
	if err == nil {
		t.Fatal("a store failure was reported as success")
	}
	if got.Outcome == "refused" {
		t.Error("a store failure was reported to the caller as a refusal")
	}
}

// A canonical uuid is accepted; the shapes a client actually sends by mistake
// are not.
func TestUUIDChecking(t *testing.T) {
	good36 := "0f8fad5b-d9cb-469f-a165-70867728950e"
	if !isUUID(good36) {
		t.Error("a canonical uuid was rejected")
	}
	bad := []string{
		"", "op-1", "0f8fad5bd9cb469fa16570867728950e", // no hyphens
		"0f8fad5b-d9cb-469f-a165-70867728950",   // one short
		"0f8fad5b-d9cb-469f-a165-70867728950ee", // one long
		"0f8fad5b_d9cb_469f_a165_70867728950e",  // underscores
		"0f8fad5b-d9cb-469f-a165-70867728950g",  // not hex
	}
	for _, s := range bad {
		if isUUID(s) {
			t.Errorf("isUUID(%q) = true", s)
		}
	}
}
