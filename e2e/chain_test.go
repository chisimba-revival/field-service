//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every test here is about a boundary. The assertions that matter are the ones
// that would still pass if the service were wrong in a way that looks correct.

// TestAValidCallerIsNotRefused is the redis.Nil fault, pinned.
//
// The denylist tells "no entry" from "an entry that cannot be read" by the error,
// and treats the second as revoked. An adapter that converted the first into the
// second refused every caller, on an empty Redis, and answered with a perfectly
// correct 401 — so a test asserting "the request is refused" passed for the whole
// time the service was refusing everything.
func TestAValidCallerIsNotRefused(t *testing.T) {
	r := start(t)
	// Nothing has been published, so this is the empty-denylist case exactly.
	status, body := r.push(r.token(0, "e2e-valid"), batch(r.sighting("e2e-valid")))
	if status != http.StatusOK {
		t.Fatalf("a valid caller was refused: %d %s", status, body)
	}
	if !strings.Contains(string(body), `"applied"`) {
		t.Errorf("expected the sighting to be applied, got %s", body)
	}
}

// TestAStaleEpochIsRefusedAndTheCurrentOneIsNot proves the boundary in both
// directions.
//
// The second half is not redundant. Without a token at the current epoch, a
// service refusing everything would satisfy the first half perfectly.
func TestAStaleEpochIsRefusedAndTheCurrentOneIsNot(t *testing.T) {
	r := start(t)
	ctx := context.Background()

	// The revocation is timestamped an hour in the PAST, so the issue-time half
	// of the denylist check says "this token is newer than the revocation" and
	// the epoch is the only thing that can refuse. Two other choices both fail to
	// isolate it: publishing "now" races the two rules on the same second, and
	// publishing a FUTURE timestamp revokes everything including the token meant
	// to be accepted — which is correct behaviour, and makes for a test that
	// passes for entirely the wrong reason.
	const subject = "42"
	if err := r.redis.Set(ctx, "revoked:"+subject,
		fmt.Sprintf("%d:%d", time.Now().Add(-time.Hour).Unix(), 7), 24*time.Hour).Err(); err != nil {
		t.Fatalf("publish epoch: %v", err)
	}
	t.Cleanup(func() { _ = r.redis.Del(context.Background(), "revoked:"+subject).Err() })

	if status, body := r.push(r.token(3, "e2e-stale"), batch(r.sighting("e2e-stale"))); status != http.StatusUnauthorized {
		t.Errorf("a token below the published epoch must be refused: %d %s", status, body)
	}
	if status, body := r.push(r.token(7, "e2e-current"), batch(r.sighting("e2e-current"))); status != http.StatusOK {
		t.Errorf("a token at the published epoch must be accepted: %d %s", status, body)
	}
}

// TestTheWriteContextComesFromTheToken is rule 2, over the wire.
//
// The body has no context field and the token names one, so a sighting cannot
// land anywhere else. Asserting the column rather than the response, because the
// response reports success either way.
func TestTheWriteContextComesFromTheToken(t *testing.T) {
	r := start(t)
	const ctxCode = "e2e-token-context"
	op := r.sighting(ctxCode)
	// A field that is not on the wire at all. If the decoder were lax this would
	// be ignored, and if it were strict the request would be refused — neither is
	// the same as the sighting going somewhere the caller did not ask for.
	if status, body := r.push(r.token(0, ctxCode), batch(op)); status != http.StatusOK {
		t.Fatalf("push: %d %s", status, body)
	}
	assertRow(t, r, op["entity_id"].(string), ctxCode, "42")
}

// TestOneResponseAndNoMore is the second-response fault, pinned.
//
// The refusal was correct — 415 — and then the handler carried on and wrote
// another body. A status assertion passed throughout; only counting the body
// notices.
func TestOneResponseAndNoMore(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-mediatype")
	body, _ := json.Marshal(batch(r.sighting("e2e-mediatype")))

	status, raw := r.pushRaw(tok, "text/plain", string(body))
	if status != http.StatusUnsupportedMediaType {
		t.Errorf("expected 415, got %d", status)
	}
	// Two JSON documents in one response is the signature of a handler that kept
	// going, and it is what a client actually receives.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if err := dec.Decode(new(any)); err != nil {
		t.Fatalf("first document did not decode: %v (%s)", err, raw)
	}
	if err := dec.Decode(new(any)); err == nil {
		t.Errorf("the handler answered twice: %s", raw)
	}
}

// TestCapturedAndRecordedAreDifferentFacts is the recorded_at defect, pinned.
//
// Both columns came from op.CapturedAt and the operation had no recorded_at at
// all, so no client could say otherwise. Conflating them makes an offline entry
// look late when it is not.
func TestCapturedAndRecordedAreDifferentFacts(t *testing.T) {
	r := start(t)
	op := r.sighting("e2e-times")
	if status, body := r.push(r.token(0, "e2e-times"), batch(op)); status != http.StatusOK {
		t.Fatalf("push: %d %s", status, body)
	}
	captured, recorded := r.timestamps(t, op["entity_id"].(string))
	if captured.Equal(recorded) {
		t.Fatalf("captured_at and recorded_at are the same instant (%s); an offline entry would look late", captured)
	}
	if want := time.Date(2026, 10, 5, 6, 14, 0, 0, time.UTC); !captured.Equal(want) {
		t.Errorf("captured_at = %s, want the client's %s", captured, want)
	}
	if want := time.Date(2026, 10, 5, 18, 2, 0, 0, time.UTC); !recorded.Equal(want) {
		t.Errorf("recorded_at = %s, want the client's %s", recorded, want)
	}
}

// TestTheSameOperationTwiceIsOneRow is rule 7 over the wire.
//
// A device retries when the connection drops, which is the normal case rather
// than the exceptional one, and a retried sighting counted twice would corrupt
// the dataset nobody can audit their way out of.
func TestTheSameOperationTwiceIsOneRow(t *testing.T) {
	r := start(t)
	op := r.sighting("e2e-retry")
	first, body := r.push(r.token(0, "e2e-retry"), batch(op))
	if first != http.StatusOK {
		t.Fatalf("first push: %d %s", first, body)
	}
	second, body := r.push(r.token(0, "e2e-retry"), batch(op))
	if second != http.StatusOK {
		t.Fatalf("retried push: %d %s", second, body)
	}
	if got := r.countRows(t, op["entity_id"].(string)); got != 1 {
		t.Errorf("a retried batch produced %d rows, want 1", got)
	}
	if a, b := results(t, body)[0].NewRevision, 1; a != b {
		t.Errorf("retried batch reported revision %d, want %d", a, b)
	}
}

// TestOneBadOperationDoesNotLoseTheGoodOnes is the fourth fault, pinned.
//
// The contract says a batch is not all-or-nothing. A create with no drive_id sent
// "" to a uuid column, the statement failed, and three good operations went with
// it — every other client mistake is a refusal, so this one behaved differently
// for no reason anybody chose.
func TestOneBadOperationDoesNotLoseTheGoodOnes(t *testing.T) {
	r := start(t)
	good := r.sighting("e2e-partial")
	var broken map[string]any
	raw, _ := json.Marshal(r.sighting("e2e-partial"))
	_ = json.Unmarshal(raw, &broken)
	delete(broken["payload"].(map[string]any), "drive_id")

	status, body := r.push(r.token(0, "e2e-partial"), batch(good, broken))
	if status != http.StatusOK {
		t.Fatalf("a partial batch must not fail the request: %d %s", status, body)
	}
	assertRow(t, r, good["entity_id"].(string), "e2e-partial", "42")

	var applied, refused int
	for _, res := range results(t, body) {
		switch res.Outcome {
		case "applied":
			applied++
		case "refused":
			refused++
		}
	}
	if applied != 1 || refused != 1 {
		t.Errorf("want one applied and one refused, got %d applied and %d refused: %s", applied, refused, body)
	}
}

// TestAMissingScopeIsRefused checks the guard's whole reason for existing.
//
// It asserts 401 rather than 403 because every refusal in the guard is written
// through one call, and a caller told to re-authenticate when re-authenticating
// cannot help is a caller that will try for ever. That is worth raising with
// whoever owns the contract; it is not this harness's decision to reverse.
func TestAMissingScopeIsRefused(t *testing.T) {
	r := start(t)
	if status, _ := r.push(r.token(0, "e2e-noscope", "something:else"),
		batch(r.sighting("e2e-noscope"))); status == http.StatusOK {
		t.Error("a token without the required scope was accepted")
	}
}

// TestAnUnreadableTokenIsRefusedWithoutDescription checks that the refusal says
// nothing useful to whoever made it.
//
// A verifier is somewhere else entirely, and the reason a request was refused can
// name tables, columns and constraint names. The status is the contract; the body
// is a sentence.
func TestAnUnreadableTokenIsRefusedWithoutDescription(t *testing.T) {
	r := start(t)
	status, body := r.push("not.a.token", batch(r.sighting("e2e-junk")))
	if status != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", status)
	}
	for _, leak := range []string{"signature", "RSA", "kid", "expired", "audience", "issuer", "sql", "SELECT"} {
		if strings.Contains(strings.ToLower(string(body)), strings.ToLower(leak)) {
			t.Errorf("the refusal describes why (%q): %s", leak, body)
		}
	}
}

// TestPullReturnsWhatWasPushed closes the loop the other direction.
//
// A push that succeeds and a pull that returns nothing is a service that looks
// correct from one end and is broken from the other, which is the shape that
// survives longest because each test only checks its own half.
func TestPullReturnsWhatWasPushed(t *testing.T) {
	r := start(t)
	const ctxCode = "e2e-roundtrip"
	op := r.sighting(ctxCode)
	if status, body := r.push(r.token(0, ctxCode), batch(op)); status != http.StatusOK {
		t.Fatalf("push: %d %s", status, body)
	}

	status, body := r.pull(r.token(0, ctxCode), "")
	if status != http.StatusOK {
		t.Fatalf("pull: %d %s", status, body)
	}
	var page struct {
		Status  string `json:"status"`
		Changes []struct {
			EntityID string         `json:"entity_id"`
			Body     map[string]any `json:"body"`
		} `json:"changes"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("pull response did not decode: %v (%s)", err, body)
	}
	if page.Status != "ok" {
		t.Errorf("pull status = %q, want ok: %s", page.Status, body)
	}
	for _, change := range page.Changes {
		if change.EntityID == op["entity_id"].(string) {
			if got, _ := change.Body["context_code"].(string); got != ctxCode {
				t.Errorf("pulled change reports context %q, want %q", got, ctxCode)
			}
			return
		}
	}
	t.Errorf("the sighting just pushed did not come back in the pull: %s", body)
}

// batch wraps operations in the envelope the route expects.
func batch(ops ...map[string]any) map[string]any {
	return map[string]any{"operations": ops}
}

// results decodes the push envelope.
//
// Both this and the old revisionOf assumed a bare array, which meant the "one
// applied, one refused" assertion silently skipped itself on a decode error
// rather than failing — a test that cannot fail is worse than no test, because
// it reports coverage it did not provide.
func results(t *testing.T, body []byte) []pushResult {
	t.Helper()
	var envelope struct {
		Results []pushResult `json:"results"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("push response did not decode: %v (%s)", err, body)
	}
	if len(envelope.Results) == 0 {
		t.Fatalf("no results in %s", body)
	}
	return envelope.Results
}

type pushResult struct {
	OperationID string `json:"operation_id"`
	Outcome     string `json:"outcome"`
	NewRevision int    `json:"new_revision"`
	ErrorCode   string `json:"error_code"`
}

// TestADriveOutsideTheCallersContextIsRefused covers the refusal that the
// foreign key cannot make for us.
//
// Three things are asserted deliberately, and the middle one is the reason the
// test exists at all.
//
// A drive that is not there is refused as a client mistake, not as a server
// fault. It used to arrive as a 500, which is the one status that tells a client
// nothing useful and takes a whole batch down with the operation that caused it.
//
// A drive belonging to a *different* context is refused too, and this is the
// case the database was never going to catch: push opens its own transaction
// and never sets app.context_grants, and the deployed role is a superuser, so
// row-level security was never going to run on this path. The comparison happens
// in this code or it does not happen.
//
// Both refusals carry the same code. Distinguishing them would turn the response
// into an oracle: a caller could walk the id space and learn which drives other
// reserves have planned, which is what the grant set exists to withhold.
//
// A drive in the caller's own context still lands, so the refusal is not simply
// refusing everything.
func TestADriveOutsideTheCallersContextIsRefused(t *testing.T) {
	r := start(t)

	t.Run("a drive that is not there", func(t *testing.T) {
		op := r.sighting("e2e-nodrive")
		op["payload"].(map[string]any)["drive_id"] = "00000000-0000-4000-8000-000000000000"

		status, body := r.push(r.token(0, "e2e-nodrive"), batch(op))
		// The status is the point. A 500 would tell the client to retry, and a
		// retry of a sighting naming a drive that does not exist can never work.
		if status != http.StatusOK {
			t.Fatalf("want the batch accepted with a refusal inside it, got %d: %s", status, body)
		}
		got := results(t, body)
		if len(got) != 1 || got[0].Outcome != "refused" {
			t.Fatalf("want one refused operation, got %s", body)
		}
		if got[0].ErrorCode != "unknown_drive" {
			t.Errorf("error_code = %q, want unknown_drive: %s", got[0].ErrorCode, body)
		}
		if n := r.countRows(t, op["entity_id"].(string)); n != 0 {
			t.Errorf("%d rows written for a sighting on a drive that does not exist, want 0", n)
		}
	})

	t.Run("a drive in another context", func(t *testing.T) {
		// The drive genuinely exists, in a context this caller is not granted.
		other := r.ensureDrive("e2e-other-context")
		op := r.sighting("e2e-mine")
		op["payload"].(map[string]any)["drive_id"] = other

		status, body := r.push(r.token(0, "e2e-mine"), batch(op))
		if status != http.StatusOK {
			t.Fatalf("push: %d %s", status, body)
		}
		got := results(t, body)
		if len(got) != 1 || got[0].Outcome != "refused" {
			t.Fatalf("want a refusal for a drive in another context, got %s", body)
		}
		if n := r.countRows(t, op["entity_id"].(string)); n != 0 {
			t.Errorf("%d rows written against another context's drive, want 0", n)
		}
	})

	t.Run("a drive in the caller's own context", func(t *testing.T) {
		op := r.sighting("e2e-owncontext")
		status, body := r.push(r.token(0, "e2e-owncontext"), batch(op))
		if status != http.StatusOK {
			t.Fatalf("push: %d %s", status, body)
		}
		got := results(t, body)
		if len(got) != 1 || got[0].Outcome != "applied" {
			t.Fatalf("a drive in the caller's own context must be accepted, got %s", body)
		}
		assertRow(t, r, op["entity_id"].(string), "e2e-owncontext", "42")
	})
}
