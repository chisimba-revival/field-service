package httpapi

// The verification door.
//
// It is registered only through Guard.ServeService, so there is no way to reach
// it with a person's token: not by forgetting the guard, and not by choosing a
// different handler. The two doors were built disjoint in both directions for
// this reason.

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"field-service/internal/verify"
)

// VerificationMaxBodyBytes bounds a verification request.
//
// Small, because a verification names one entry and carries a few fields. The
// bound is here rather than left to the server's defaults because an unbounded
// read is a memory lever held by anyone who can reach the port, and this port is
// reachable by a service on the internal network.
const VerificationMaxBodyBytes = 16 << 10

// Verification holds what the verification routes need.
type Verification struct {
	Verify *verify.Service
	Log    Logger
}

// NewVerificationRoutes builds the verification routes, already wrapped in the
// service door.
func NewVerificationRoutes(g *Guard, v *Verification) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/log-book/verify", g.ServeService("field:verify")(v.verifyHandler))
	return mux
}

func (v *Verification) verifyHandler(w http.ResponseWriter, r *http.Request, _ Caller) error {
	body := http.MaxBytesReader(w, r.Body, VerificationMaxBodyBytes)
	dec := json.NewDecoder(body)
	// An unknown field is refused rather than ignored. A client sending
	// mentorName rather than mentor has made a mistake the response must report:
	// ignoring it would produce a verification attributed to nobody that still
	// looked complete.
	dec.DisallowUnknownFields()

	var req verify.Request
	if err := dec.Decode(&req); err != nil {
		return problem(w, http.StatusBadRequest, "unreadable_body",
			"That request could not be read.")
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		// A second document in one body. Two verifications in one request would
		// mean one operation_id and one answer for two decisions.
		return problem(w, http.StatusBadRequest, "unreadable_body",
			"That request held more than one verification.")
	}

	res, err := v.Verify.Verify(r.Context(), req)
	if err != nil {
		// A fault, not a refusal. Reported to the log and described to the
		// client as nothing more specific: a database error names tables,
		// columns and constraint names, and that is the schema handed to whoever
		// can read the response.
		if v.Log != nil {
			v.Log.Refused(r, "handler_failed", err)
		}
		return problem(w, http.StatusInternalServerError, "internal_error",
			"That did not go through. Nothing was changed.")
	}

	w.Header().Set("Cache-Control", "no-store")
	if res.Outcome == "refused" {
		// A refusal is a client mistake and is described, because unlike an
		// authentication failure there is nothing here to hide: the caller
		// presented a well-formed token and asked a question that has an answer.
		return writeJSON(w, http.StatusUnprocessableEntity, res)
	}
	return writeJSON(w, http.StatusOK, res)
}
