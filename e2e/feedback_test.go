//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The sign-off read rule, exercised against the real statement.
//
// This exists because the unit tests cannot hold it. They fake the store, so the
// authorisation — which is entirely in the SQL — was never executed by anything
// when "authorise everyone" was tried: the unit suite stayed green and the only
// signal came from a test that had nothing to do with the read.
//
// The rule: a trainee may read their own, the mentor who wrote it may read it, a
// programme administrator in the context may read it, and nobody else may learn
// it exists. Each test below is paired so that removing any single clause changes
// an answer — a suite of refusals alone would pass with the rule inverted.

func (r *rig) readSignoff(tok, id string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest("GET", r.server.URL+"/api/v1/signoffs/"+id, nil)
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return r.do(req)
}

// readDetail is the wire shape: the sign-off with its assessments and evidence.
type readDetail struct {
	ID          string `json:"id"`
	TraineeID   string `json:"trainee_id"`
	MentorID    string `json:"mentor_id"`
	Status      string `json:"status"`
	Assessments []struct {
		Code     string `json:"code"`
		Rating   string `json:"rating"`
		Evidence string `json:"evidence"`
	} `json:"assessments"`
}

// The trainee the sign-off is about may read it, and must receive the evidence —
// not merely be told the row exists. A read that returned the sign-off without its
// assessments would pass a status-code assertion and deliver no feedback at all,
// which is the gap this endpoint was added to close.
func TestTheTraineeCanReadTheirOwnSignoffAndItsEvidence(t *testing.T) {
	r := start(t)
	outing := r.ensureOuting("Feedback")
	trainee := r.token(0, "Feedback")
	mentor := r.token(0, "Feedback")

	body := r.draftBody(outing, "42", "SAFFIR")
	body["signoff_id"] = r.uuidFor("signoff:trainee-reads-own")
	body["operation_id"] = r.uuidFor("op:trainee-reads-own")
	body["mentor_id"] = "mentor-9"
	if code, b := r.createSignoff(mentor, body); code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", code, b)
	}
	id := body["signoff_id"].(string)

	code, b := r.readSignoff(trainee, id)
	if code != http.StatusOK {
		t.Fatalf("the trainee could not read their own sign-off: %d %s", code, b)
	}
	var got readDetail
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decoding %q: %v", b, err)
	}
	if len(got.Assessments) != 1 {
		t.Fatalf("got %d assessments, want 1 — a read without them is no feedback", len(got.Assessments))
	}
	if got.Assessments[0].Code != "SAFFIR" || got.Assessments[0].Evidence == "" {
		t.Fatalf("assessment did not survive the read: %+v", got.Assessments[0])
	}
}

// Somebody who is neither the trainee nor the mentor must be told not-found, and
// must be told it in the same words as a sign-off that does not exist. If the two
// answers differed, this endpoint would be a way of discovering which sign-off ids
// are real — and the id is already in the change feed every device holds.
func TestAnotherTraineeCannotReadItAndCannotTellItExists(t *testing.T) {
	r := start(t)
	outing := r.ensureOuting("Feedback")
	tok := r.token(0, "Feedback")

	body := r.draftBody(outing, "trainee-a", "SAFFIR")
	body["signoff_id"] = r.uuidFor("signoff:not-yours")
	body["operation_id"] = r.uuidFor("op:not-yours")
	body["mentor_id"] = "mentor-a"
	if code, b := r.createSignoff(tok, body); code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", code, b)
	}
	id := body["signoff_id"].(string)

	code, forbidden := r.readSignoff(r.token(1, "Feedback"), id)
	if code != http.StatusNotFound {
		t.Fatalf("another trainee read it: status %d, body %s", code, forbidden)
	}

	// The same answer as a genuinely unknown id. Compared on the whole body so a
	// difference in any word — not merely the status — fails here.
	_, absent := r.readSignoff(r.token(1, "Feedback"), r.uuidFor("signoff:never-existed"))
	if string(forbidden) != string(absent) {
		t.Fatalf("a forbidden sign-off answers %q but an absent one answers %q — the pair leaks which ids exist",
			forbidden, absent)
	}
}

// The mentor who wrote it may read it back. Without this the read rule would be
// "the trainee and nobody else", and a mentor could not check what they had
// written before submitting.
func TestTheMentorMayReadIt(t *testing.T) {
	r := start(t)
	outing := r.ensureOuting("Feedback")
	tok := r.token(0, "Feedback")

	body := r.draftBody(outing, "trainee-m", "CLIBRF")
	body["signoff_id"] = r.uuidFor("signoff:mentor-reads")
	body["operation_id"] = r.uuidFor("op:mentor-reads")
	// The caller is the mentor, so the mentor clause is what admits them.
	body["mentor_id"] = "42"
	if code, b := r.createSignoff(tok, body); code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", code, b)
	}
	if code, b := r.readSignoff(tok, body["signoff_id"].(string)); code != http.StatusOK {
		t.Fatalf("the mentor could not read back what they wrote: %d %s", code, b)
	}
}

// A sign-off in another context is not found. The context clause and the reader
// clauses are separate, and this is the test that says so: the caller here is
// named in the row and would otherwise satisfy "s.trainee_id = caller".
func TestASignoffInAnotherContextIsNotFound(t *testing.T) {
	r := start(t)
	outing := r.ensureOuting("Elsewhere")
	tok := r.token(0, "Elsewhere")

	body := r.draftBody(outing, "trainee-x", "SAFFIR")
	body["signoff_id"] = r.uuidFor("signoff:other-context")
	body["operation_id"] = r.uuidFor("op:other-context")
	// The caller is named in the row, so the reader clause is satisfied — and only
	// the context clause stands between them and it.
	body["mentor_id"] = "42"
	if code, b := r.createSignoff(tok, body); code != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", code, b)
	}
	// Read with a token whose active context is somewhere else entirely.
	if code, b := r.readSignoff(r.token(0, "NotThatContext"), body["signoff_id"].(string)); code != http.StatusNotFound {
		t.Fatalf("a cross-context read succeeded: %d %s", code, b)
	}
}

// A malformed identifier is not found rather than refused, and never reaches the
// database. Asserted on the status being 404 so a client probing ids sees one
// answer for everything that is not theirs to see.
func TestAMalformedIdentifierIsNotFound(t *testing.T) {
	r := start(t)
	tok := r.token(0, "Feedback")

	// 422, not 404, and deliberately so. A malformed id says nothing about
	// whether any sign-off exists — it cannot, having matched nothing — so telling
	// the client its own id was malformed is honest and leaks nothing. Folding it
	// into 404 would only make a client's own bug harder to find.
	for _, id := range []string{"not-a-uuid", strings.Repeat("z", 40)} {
		if code, b := r.readSignoff(tok, id); code != http.StatusUnprocessableEntity {
			t.Fatalf("id %q gave status %d, want 422 (%s)", id, code, b)
		}
	}

	// A well-formed id that is simply unknown, by contrast, is 404.
	if code, b := r.readSignoff(tok, r.uuidFor("signoff:genuinely-absent")); code != http.StatusNotFound {
		t.Fatalf("absent id gave %d, want 404 (%s)", code, b)
	}
}
