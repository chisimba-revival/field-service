//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Verification, driven through the real service, the real database and the real
// service door.
//
// Each test below exists because the corresponding rule could survive without it.
// A verification that is correct and complete is the one thing the system exists
// to produce, and a rule that is never violated is a rule that was never tested.

// newPendingEntry creates an outing and a log book entry on it, and returns the
// entry's id and the context both live in.
func (r *rig) newPendingEntry(t *testing.T, ctx, species string) string {
	t.Helper()
	outing := r.ensureOuting(ctx)
	entry := map[string]any{"operations": []any{map[string]any{
		"operation_id": r.uuidFor("op:" + t.Name() + ":verify-seed"),
		"entity":       "log_book_entry",
		"kind":         "create",
		"entity_id":    r.uuidFor("entry:" + t.Name()),
		"captured_at":  "2026-10-05T06:14:00Z",
		"payload": map[string]any{
			"outing_id":    outing,
			"species_code": species,
			"count":        2,
			"notes":        "spotted from the vehicle",
			"location":     map[string]any{"type": "Point", "coordinates": []float64{36.8219, -1.2921}},
		},
	}}}
	// Seeded through the person door, because that is how a real entry arrives.
	code, body := r.push(personToken(r, ctx), entry)
	if code != http.StatusOK || strings.Contains(string(body), `"refused"`) {
		t.Fatalf("seeding the entry: status %d, body %s", code, body)
	}
	return r.uuidFor("entry:" + t.Name())
}

// personToken is the person door's token. Separate from the service token
// because the two doors being disjoint is what several tests below check.
func personToken(r *rig, ctx string) string { return r.token(0, ctx, scope) }

// entryState reads what the database holds, so a test asserts the row rather than
// the response. A response can be right while the row is wrong.
func (r *rig) entryState(t *testing.T, id string) (status, verifiedBy, recordedSpecies string, recordedCount *int, revision int64) {
	t.Helper()
	var count any
	err := r.pool.QueryRow(context.Background(),
		`select status::text, coalesce(verified_by,''), coalesce(recorded_species_code,''),
		        recorded_count, revision
		   from log_book_entry where id = $1`, id).
		Scan(&status, &verifiedBy, &recordedSpecies, &count, &revision)
	if err != nil {
		t.Fatalf("reading entry %s: %v", id, err)
	}
	// pgx scans an int4 into `any` as int32, and a bigint as int64. Handling only
	// int64 left the pointer nil for every ordinary count, which read as "the
	// trainee's count was not kept" — the test reporting a defect in the code
	// that was a defect in the test.
	switch c := count.(type) {
	case int32:
		v := int(c)
		recordedCount = &v
	case int64:
		v := int(c)
		recordedCount = &v
	case nil:
	default:
		t.Fatalf("count scanned as %T, which the test does not know how to read", count)
	}
	return
}

// A mentor's answer is applied, and the row says so.
func TestAMentorsDecisionIsApplied(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")
	svc := r.serviceToken()

	code, body := r.verifyOnce(svc, map[string]any{
		"operation_id":  r.uuidFor("op:verify-applied"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}

	status, verifiedBy, _, _, revision := r.entryState(t, entry)
	if status != "verified" {
		t.Errorf("status is %q, want verified", status)
	}
	// The mentor must be recorded. A decision nobody made cannot be appealed,
	// reviewed, or learned from.
	if verifiedBy != "mentor-1" {
		t.Errorf("verified_by is %q, want mentor-1", verifiedBy)
	}
	if revision != 2 {
		t.Errorf("revision is %d, want 2", revision)
	}
}

// A correction keeps what the trainee recorded, permanently, beside the
// correction.
//
// This is the test the whole feature rests on. A client that shows only the
// corrected value has erased the trainee's reasoning along with their mistake,
// and there is nothing left to learn from.
func TestACorrectionRetainsWhatTheTraineeRecorded(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	code, body := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":       r.uuidFor("op:verify-correct"),
		"entry_id":           entry,
		"context":            "Alpha",
		"mentor":             "mentor-1",
		"outcome":            "verified",
		"species_code":       "LION",
		"count":              4,
		"correction_reason":  "Rosettes and a stocky build; I called it a leopard.",
		"verification_notes": "Four animals, one large male.",
		"base_revision":      1,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}

	var species string
	var count int
	err := r.pool.QueryRow(context.Background(),
		`select species_code, count from log_book_entry where id = $1`, entry).
		Scan(&species, &count)
	if err != nil {
		t.Fatal(err)
	}
	if species != "LION" || count != 4 {
		t.Errorf("the corrected values did not land: %s/%d", species, count)
	}

	_, _, recordedSpecies, recordedCount, _ := r.entryState(t, entry)
	// Both halves asserted. A test checking only that the correction landed would
	// pass against code that overwrote the original, which is the failure this
	// feature exists to prevent.
	if recordedSpecies != "LEOP" {
		t.Errorf("recorded_species_code is %q, want LEOP: the trainee's value was not kept", recordedSpecies)
	}
	if recordedCount == nil || *recordedCount != 2 {
		t.Errorf("recorded_count is %v, want 2: the trainee's count was not kept", recordedCount)
	}
}

// A correction with no reason is refused, and the row is untouched.
//
// Asserted on the row as well as the response, because a store that wrote the
// correction and then returned a refusal would produce a refusal that looks
// correct over an entry that is not.
func TestACorrectionWithNoReasonIsRefusedAndChangesNothing(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	code, body := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":  r.uuidFor("op:verify-noreason"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"species_code":  "LION",
		"base_revision": 1,
	})
	if code == http.StatusOK {
		t.Fatalf("a correction with no reason was accepted: %s", body)
	}

	var species string
	var status string
	if err := r.pool.QueryRow(context.Background(),
		`select species_code, status::text from log_book_entry where id = $1`, entry).
		Scan(&species, &status); err != nil {
		t.Fatal(err)
	}
	if species != "LEOP" || status != "pending" {
		t.Errorf("the entry changed despite the refusal: %s/%s", species, status)
	}
}

// needs_review exists so a mentor can be honest about uncertainty instead of
// being pushed into a verdict. It must be reachable, or the field is decorative.
func TestNeedsReviewIsAReachableAnswer(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	code, body := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":       r.uuidFor("op:verify-review"),
		"entry_id":           entry,
		"context":            "Alpha",
		"mentor":             "mentor-1",
		"outcome":            "needs_review",
		"verification_notes": "Too far to identify with confidence.",
		"base_revision":      1,
	})
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	status, _, _, _, _ := r.entryState(t, entry)
	if status != "needs_review" {
		t.Errorf("status is %q, want needs_review", status)
	}
}

// An entry in another context must be indistinguishable from one that does not
// exist.
//
// Asserted on the error code and not on the status alone, because the code is
// what stops this endpoint from being an oracle for discovering which ids exist
// in another reserve.
func TestAnEntryInAnotherContextIsNotFound(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	code, body := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":  r.uuidFor("op:verify-foreign"),
		"entry_id":      entry,
		"context":       foreignContext,
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, body %s", code, body)
	}
	if !strings.Contains(string(body), "unknown_log_book_entry") {
		t.Errorf("a foreign-context entry gave %s, want unknown_log_book_entry", body)
	}
	// The same answer as an id that exists nowhere at all.
	//
	// Compared with the operation_id stripped, because a response echoes the
	// caller's own ids and those differ between two calls that are otherwise
	// identical. Comparing them whole would have compared two operation ids and
	// reported a difference in the thing under test.
	code2, raw2 := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":  r.uuidFor("op:verify-absent"),
		"entry_id":      r.uuidFor("entry:does-not-exist"),
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	})
	if strip(raw2) != strip(body) {
		t.Errorf("a foreign entry and an absent entry answered differently:\n%s\n%s", body, raw2)
	}
	if code2 != code {
		t.Errorf("status %d for an absent entry and %d for a foreign one", code2, code)
	}

	status, _, _, _, _ := r.entryState(t, entry)
	if status != "pending" {
		t.Errorf("the entry was verified by a cross-context request: %s", status)
	}
}

// A stale revision is refused rather than applied on top.
//
// This is an authority decision about a record the mentor believed they were
// reading. If the record moved since, they did not read this one.
func TestAStaleRevisionIsRefused(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	code, body := r.verifyOnce(r.serviceToken(), map[string]any{
		"operation_id":  r.uuidFor("op:verify-stale"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 99,
	})
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "stale_revision") {
		t.Fatalf("a stale revision gave %d %s", code, body)
	}
	status, _, _, _, revision := r.entryState(t, entry)
	if status != "pending" || revision != 1 {
		t.Errorf("the row moved despite the refusal: %s at revision %d", status, revision)
	}
}

// The same operation id twice must change the row once.
//
// A retry after a dropped response is the ordinary case, not an edge case: a
// mentor's device on a marginal connection is exactly where this happens, and
// re-applying would give the entry a second revision for one decision.
func TestTheSameOperationIdAppliesOnce(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	body := map[string]any{
		"operation_id":  r.uuidFor("op:verify-retry"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	}
	code1, first := r.verifyOnce(r.serviceToken(), body)
	code2, second := r.verifyOnce(r.serviceToken(), body)
	if code1 != http.StatusOK || code2 != http.StatusOK {
		t.Fatalf("first %d %s, second %d %s", code1, first, code2, second)
	}
	if string(first) != string(second) {
		t.Errorf("a retry answered differently:\n%s\n%s", first, second)
	}
	_, _, _, _, revision := r.entryState(t, entry)
	if revision != 2 {
		t.Errorf("revision is %d after one decision applied twice, want 2", revision)
	}
}

// The two doors are disjoint, in both directions.
//
// These are the assertions the whole two-guard design rests on, and they are the
// ones most likely to be quietly broken by a later change: adding a scope to a
// service token, or a type check to the person door.
func TestTheDoorsAreDisjoint(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")
	body := map[string]any{
		"operation_id":  r.uuidFor("op:verify-doors"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	}

	// A person's token, even one carrying the verification scope, is refused.
	person := r.token(0, "Alpha", verificationScope)
	if code, _ := r.verifyOnce(person, body); code != http.StatusUnauthorized {
		t.Errorf("a person token reached the verification route: status %d", code)
	}
	// And the entry is untouched, which is what makes the refusal mean something.
	status, _, _, _, revision := r.entryState(t, entry)
	if status != "pending" || revision != 1 {
		t.Errorf("the row moved despite the door refusing: %s at revision %d", status, revision)
	}
}

// A service token without the verification scope is refused.
func TestTheServiceDoorInsistsOnItsScope(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")
	if code, _ := r.verifyOnce(r.serviceToken(scope), map[string]any{
		"operation_id":  r.uuidFor("op:verify-scope"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-1",
		"outcome":       "verified",
		"base_revision": 1,
	}); code != http.StatusUnauthorized {
		t.Errorf("a service token with the wrong scope reached the route: status %d", code)
	}
}

// An entry already decided is not re-decided, or the first mentor's explanation
// is lost under a second mentor's.
func TestAnEntryAlreadyDecidedIsNotReverified(t *testing.T) {
	r := start(t)
	entry := r.newPendingEntry(t, "Alpha", "LEOP")

	first := map[string]any{
		"operation_id":       r.uuidFor("op:verify-first"),
		"entry_id":           entry,
		"context":            "Alpha",
		"mentor":             "mentor-1",
		"outcome":            "verified",
		"verification_notes": "Confirmed, clear view.",
		"base_revision":      1,
	}
	if code, body := r.verifyOnce(r.serviceToken(), first); code != http.StatusOK {
		t.Fatalf("first verification: %d %s", code, body)
	}
	second := map[string]any{
		"operation_id":  r.uuidFor("op:verify-second"),
		"entry_id":      entry,
		"context":       "Alpha",
		"mentor":        "mentor-2",
		"outcome":       "rejected",
		"base_revision": 2,
	}
	code, body := r.verifyOnce(r.serviceToken(), second)
	if code != http.StatusUnprocessableEntity || !strings.Contains(string(body), "already_decided") {
		t.Fatalf("re-verifying gave %d %s", code, body)
	}
	status, verifiedBy, _, _, revision := r.entryState(t, entry)
	if status != "verified" || verifiedBy != "mentor-1" || revision != 2 {
		t.Errorf("the second decision overwrote the first: %s by %s at revision %d",
			status, verifiedBy, revision)
	}
}

// strip removes the operation_id and entry_id from a verification response, so
// two answers can be compared on what they say rather than on what the caller
// happened to send.
func strip(body []byte) string {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return string(body)
	}
	delete(m, "operation_id")
	delete(m, "entry_id")
	out, _ := json.Marshal(m)
	return string(out)
}
