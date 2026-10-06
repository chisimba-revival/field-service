//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"field-service/internal/signoff"
)

// Retirement, against the real catalogue and the real query that decides it.
//
// This exists because the rule was unguarded and the commit said so. Every unit
// test for sign-off creation fakes the competency catalogue, so the rule
// "a retired competency cannot be assessed" was asserted on the fake and never on
// the query that enforces it. Removing `and retired_at is null` from
// PgCatalogue.Assessable changed nothing any test could see — which is the same
// gap the species catalogue had, where a search asserted through a fake store
// passed while the SQL did something else entirely.
//
// So: this test retires a real row and asks the real service to judge it.

// retireCompetency marks a catalogue entry retired and puts it back afterwards.
//
// The put-back matters. These tests share one database, and leaving a competency
// retired would make every later test that happens to use it fail for a reason
// that has nothing to do with what it is testing.
func (r *rig) retireCompetency(code string, retired bool) {
	r.t.Helper()
	_, err := r.pool.Exec(r.t.Context(),
		`update competency set retired_at = case when $2 then now() else null end where code = $1`,
		code, retired)
	if err != nil {
		r.t.Fatalf("setting retired_at on %s: %v", code, err)
	}
}

// draftBody builds a sign-off draft naming the given competencies, each rated
// competent with evidence, so the only thing under test is whether the code is
// acceptable to assess at all.
func (r *rig) draftBody(outing string, trainee string, codes ...string) map[string]any {
	assessments := make([]map[string]any, 0, len(codes))
	for _, c := range codes {
		assessments = append(assessments, map[string]any{
			"code":     c,
			"rating":   "competent",
			"evidence": "Held the rifle safely and called the species correctly throughout.",
		})
	}
	return map[string]any{
		"operation_id": r.uuidFor("op:signoff:" + strings.Join(codes, "+")),
		"signoff_id":   r.uuidFor("signoff:" + strings.Join(codes, "+")),
		"outing_id":    outing,
		"trainee_id":   trainee,
		"mentor_id":    "mentor-1",
		"competencies": assessments,
	}
}

func (r *rig) createSignoff(tok string, body map[string]any) (int, []byte) {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest("POST", r.server.URL+"/api/v1/signoffs", strings.NewReader(string(encoded)))
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	return r.do(req)
}

// refusalCode reads the code out of a refusal body.
//
// The envelope is {"error":"<code>","message":"<prose>"} — error is a string, not
// an object holding a code. My first helper assumed the nested shape and returned
// "" for every refusal, which would have made the assertion below pass vacuously
// on any refusal at all rather than only on the one intended.
func refusalCode(body []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	return env.Error
}

// A retired competency cannot be assessed on a new sign-off, while the same
// competency stays readable in the catalogue.
//
// The two halves are asserted together deliberately. Retiring a row without also
// hiding it would satisfy the first half and quietly remove a mentor's ability to
// see what the curriculum used to ask for; the reason retirement is not deletion
// is that the assessment history still refers to it.
func TestARetiredCompetencyCannotBeAssessedButIsStillReadable(t *testing.T) {
	r := start(t)
	// The context is the NAME and the outing is a separate call. Passing
	// ensureOuting's return value where a context belongs type-checks, because
	// both are strings, and it fails as unknown_outing rather than as anything
	// that names the mistake. The species tests carry the same shape and get away
	// with it because the catalogue is not context-scoped.
	outing := r.ensureOuting("Retirement")
	tok := r.token(0, "Retirement")

	r.retireCompetency("SAFFIR", true)
	defer r.retireCompetency("SAFFIR", false)

	// Readable. Asserted first because if retirement also hid the row, the
	// refusal below would pass for the wrong reason.
	code, body := r.catalogueGET(tok, "/api/v1/competencies?q=SAFFIR")
	if code != http.StatusOK {
		t.Fatalf("a retired competency stopped being readable: status %d, body %s", code, body)
	}
	if !strings.Contains(string(body), "SAFFIR") {
		t.Fatalf("the catalogue no longer lists a retired competency: %s", body)
	}

	// And not assessable.
	code, body = r.createSignoff(tok, r.draftBody(outing, "trainee-1", "SAFFIR"))
	if code == http.StatusCreated || code == http.StatusOK {
		t.Fatalf("a retired competency was assessed: status %d, body %s", code, body)
	}
	if got := refusalCode(body); got != "unknown_competency" {
		t.Fatalf("retired competency gave %q, want unknown_competency: %s", got, body)
	}
}

// The same code is accepted while it is current, so the refusal above is about
// retirement and not about the code being unknown for some other reason.
//
// Without this a test asserting only the refusal would pass even if every
// competency in the catalogue were unassessable.
func TestACurrentCompetencyIsAssessed(t *testing.T) {
	r := start(t)
	outing := r.ensureOuting("Current")
	tok := r.token(0, "Current")

	r.retireCompetency("SAFFIR", false)
	defer r.retireCompetency("SAFFIR", false)

	code, body := r.createSignoff(tok, r.draftBody(outing, "trainee-1", "SAFFIR"))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("a current competency was refused: status %d, body %s", code, body)
	}

	// And the row really carries it, rather than the response merely claiming so.
	var count int
	err := r.pool.QueryRow(r.t.Context(),
		`select count(*) from signoff_competency sc
		 join signoff s on s.id = sc.signoff_id
		 where sc.competency_code = 'SAFFIR' and s.outing_id = $1`, outing).Scan(&count)
	if err != nil {
		t.Fatalf("counting the assessment: %v", err)
	}
	if count != 1 {
		t.Fatalf("found %d SAFFIR assessments, want 1", count)
	}
}

// Retirement is the only thing that changed between the two cases above.
//
// Asserted by reading the flag back rather than trusting the update, because a
// helper that silently did nothing would leave both tests agreeing for the wrong
// reason — which is how the first version of the competency search looked like it
// worked.
func TestRetirementIsWhatTheQueryReads(t *testing.T) {
	r := start(t)
	r.retireCompetency("TRKSPR", true)
	defer r.retireCompetency("TRKSPR", false)

	var retired bool
	err := r.pool.QueryRow(r.t.Context(),
		`select retired_at is not null from competency where code = 'TRKSPR'`).Scan(&retired)
	if err != nil {
		t.Fatalf("reading retired_at: %v", err)
	}
	if !retired {
		t.Fatal("the helper did not retire the row, so the refusal tests prove nothing")
	}
}

// compile-time use keeps the signoff import honest if the helpers above change.
var _ = signoff.MinAssessments
