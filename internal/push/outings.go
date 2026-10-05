package push

import (
	"context"
	"strings"
	"time"
)

// Creating a drive, a hike or a camp.
//
// The schema splits what each kind requires into its own detail table, because a
// drive needs a guest count and a duration, a trail needs a rifle role and a
// walk length, and neither pair means anything for the other. This file is where
// that split is actually enforced: a request naming a kind is checked against
// only that kind's required fields, and the outing and its detail row are written
// in one transaction or neither is.
//
// Every field below arrives from the payload except the context, which comes from
// the caller's token, and the id, which the client minted so a retry can be
// recognised as the same attempt.

// outingKinds is what a client may name. It is a closed list rather than whatever
// the database's check constraint will accept, so an unrecognised kind is refused
// with a code the client has heard of rather than becoming a 500.
var outingKinds = map[string]bool{"drive": true, "hike": true, "camp": true}

// outingStatuses mirrors the database's own check. Duplicated because the
// database cannot be consulted without a connection, and an unrecognised status
// has to be refused here rather than passed down to fail as a constraint
// violation.
var outingStatuses = map[string]bool{
	"planned": true, "active": true, "completed": true, "cancelled": true,
}

// createOuting creates a drive, hike or camp.
//
// Refusals name the field that is missing rather than reporting one blanket
// "rejected" code, because the client can only prompt for something specific if
// it is told which something: a device holding this response has to be able to
// say "how many guests?" rather than "the hike was rejected".
func (t *pgxTx) createOuting(ctx context.Context, caller Caller, op Operation) (Result, error) {
	// The client mints the id so a retry after a crash is recognisably the same
	// attempt; the unique constraint on operation_id is what makes that work. This
	// check exists so a client that sent no id is told, rather than the database
	// rejecting it as a malformed uuid.
	if !isUUID(op.EntityID) {
		return refuse(op, "create_without_entity_id"), nil
	}
	if op.BaseRevision != nil {
		return refuse(op, "create_with_base_revision"), nil
	}

	kind := payloadText(op.Payload, "outing_kind")
	if !outingKinds[kind] {
		return refuse(op, "unknown_outing_kind"), nil
	}

	// A camp has no route because a camp does not move. Refusing one that carries
	// a route is not pedantry: a LineString on a stationary record is a route
	// somebody drove or walked that is now attributed to the camp, and a report
	// reading it back would place the camp somewhere it never was.
	if kind == "camp" && payloadRoute(op.Payload) != nil {
		return refuse(op, "camp_with_route"), nil
	}

	// Everything is validated before anything is written.
	//
	// Writing the outing first and validating as the detail was inserted left an
	// outing behind every time a required field was missing: a hike with no rifle
	// role was refused, the client was told so, and an outing with no
	// hike_detail row sat in the table claiming to be a hike that never got its
	// required field. Nothing in the response said so, and the refusal carried the
	// client state as though nothing had been written.
	//
	// Validating first also keeps the validation and the insert in one place
	// without a second pass to keep in step: checkOutingDetail is what both
	// requireFields and insertOutingDetail read, so a field that becomes required
	// later is enforced by adding it in one function rather than by remembering
	// to update a check that has drifted away from the write.
	if err := requireFields(op, kind); err != nil {
		return refusalFor(err, op)
	}
	if err := t.insertOuting(ctx, caller, op, kind); err != nil {
		return refusalFor(err, op)
	}
	if err := t.insertOutingDetail(ctx, op, kind); err != nil {
		return refusalFor(err, op)
	}

	// The change feed is written for an outing exactly as it is for a sighting.
	//
	// Omitting it looked harmless — the row was in the table — and was not. The
	// feed is how another device learns a record exists; without an entry, an
	// outing a mentor created in the office is invisible to a device that pulls,
	// and the sightings recorded against it arrive with nothing to hang them on.
	// The test that caught this had to assert the feed row's body, because every
	// other assertion here passed while the feed was missing.
	if err := t.feed(ctx, caller, op, 1); err != nil {
		return Result{}, err
	}

	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: 1,
	}, nil
}

// insertOuting writes the row all three kinds share.
//
// The context is the caller's write context, never a value from the payload: there
// is no context field on Operation for a client to fill in, and the value is
// bound rather than interpolated. Status is validated rather than trusted,
// because a client able to set 'sealed' would be closing a drive the mentor has
// not finished.
func (t *pgxTx) insertOuting(ctx context.Context, caller Caller, op Operation, kind string) error {
	status := payloadText(op.Payload, "status")
	if status == "" {
		status = "planned"
	}
	if !outingStatuses[status] {
		return missingField("unknown_outing_status")
	}

	const q = `
		insert into outing
		  (id, context_code, kind, guide_id, trainee_ids, status,
		   planned_start_time, start_time, end_time, route, weather, notes)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`

	_, err := t.tx.Exec(ctx, q,
		op.EntityID, caller.WriteContext, kind,
		payloadText(op.Payload, "guide_id"),
		// An empty list rather than a nil slice when the client sent none.
		// trainee_ids is not null with a default of '{}', and passing Go's nil
		// writes NULL rather than falling back to the default — so an outing
		// planned before the trainee list was known, which is ordinary, arrived
		// as a constraint violation and poisoned the whole batch.
		nonNilStrings(payloadStrings(op.Payload, "trainee_ids")),
		status,
		payloadTimePtr(op.Payload, "planned_start_time"),
		payloadTimePtr(op.Payload, "start_time"),
		payloadTimePtr(op.Payload, "end_time"),
		payloadRoute(op.Payload),
		payloadTextPtr(op.Payload, "weather"),
		payloadTextPtr(op.Payload, "notes"),
	)
	return err
}

// insertOutingDetail writes the row that makes this kind of outing what it is.
//
// The required fields are checked here, in Go, rather than left to NOT NULL
// constraints. That is a deliberate choice against the database's opinion, and
// the reason is behavioural rather than stylistic: a constraint violation fails
// the transaction, which from here is a 500 for the whole batch — and one
// malformed operation taking three good ones down with it is the exact failure
// this package already had once, with a missing drive_id, and fixed.
//
// The schema does not make these NOT NULL for the same reason: eighteen drives
// predate the requirement and recorded neither value, so the column has to allow
// absence for history while the newest records are held to the rule.
func (t *pgxTx) insertOutingDetail(ctx context.Context, op Operation, kind string) error {
	switch kind {
	case "drive":
		duration := payloadFloatPtr(op.Payload, "duration_hours")
		guests := payloadIntPtr(op.Payload, "guest_count")
		_, err := t.tx.Exec(ctx,
			`insert into drive_detail (outing_id, duration_hours, guest_count)
			 values ($1, $2, $3)`,
			op.EntityID, *duration, *guests)
		return err

	case "hike":
		role := payloadText(op.Payload, "rifle_role")
		length := payloadFloatPtr(op.Payload, "walk_length_km")
		_, err := t.tx.Exec(ctx,
			`insert into hike_detail
			   (outing_id, rifle_role, walk_length_km, hours_walked, description, lessons_learned)
			 values ($1, $2, $3, $4, $5, $6)`,
			op.EntityID, role, *length,
			payloadFloatPtr(op.Payload, "hours_walked"),
			payloadTextPtr(op.Payload, "description"),
			payloadTextPtr(op.Payload, "lessons_learned"))
		return err

	case "camp":
		_, err := t.tx.Exec(ctx,
			`insert into camp_detail (outing_id, site_name, facilities)
			 values ($1, $2, $3)`,
			op.EntityID,
			payloadTextPtr(op.Payload, "site_name"),
			payloadTextPtr(op.Payload, "facilities"))
		return err
	}
	return missingField("unknown_outing_kind")
}

// requireFields checks the fields this kind of outing cannot do without.
//
// It reads nothing from the database and writes nothing, which is what lets it
// run before any insert. Returning the same error type the insert path uses means
// a refusal has one shape whichever check produced it.
func requireFields(op Operation, kind string) error {
	switch kind {
	case "drive":
		if d := payloadFloatPtr(op.Payload, "duration_hours"); d == nil || *d <= 0 {
			return missingField("drive_without_duration")
		}
		if g := payloadIntPtr(op.Payload, "guest_count"); g == nil || *g < 0 {
			return missingField("drive_without_guest_count")
		}

	case "hike":
		// One condition over three values rather than a presence check, because
		// "neither" is a real answer and a blank is not. A participant who was on
		// neither rifle is a fact the programme counts separately; a participant
		// the client forgot about is a hole.
		switch role := payloadText(op.Payload, "rifle_role"); role {
		case "first", "second", "neither":
		default:
			return missingField("hike_without_rifle_role")
		}
		if l := payloadFloatPtr(op.Payload, "walk_length_km"); l == nil || *l <= 0 {
			return missingField("hike_without_walk_length")
		}

	case "camp":
		// Nothing required. A camp is a place and a duration and both are on the
		// outing; the detail row exists so all three kinds are asked the same
		// question, and so a camp can carry a site name when there is one.
	}
	return nil
}

// missingField is a refusal carrying a code the client can act on.
//
// It is an error type rather than a Result so each check reads as a single
// condition, and so the three of them can share one conversion point. It never
// leaves this file as a plain error: resultFor turns it into a refusal before
// anything is written, and anything else is a genuine fault the caller must see.
type missingField string

func (e missingField) Error() string { return "push: " + string(e) }

// refusalFor converts an error into either a refusal or a fault.
//
// Only missingField becomes a refusal. Anything else is returned as an error, and
// that is the distinction that lets an operator tell a bad request from a broken
// service.
//
// The first version returned a zero Result for everything else. A database error
// was therefore swallowed into a Result with no outcome, and the caller carried
// on to commit a transaction the failed statement had already poisoned. What
// reached the client was "commit unexpectedly resulted in rollback" on an
// operation that had partly succeeded — a 500 telling it to retry a write that
// had happened, which is the failure mode this file exists to avoid. An error
// here is never a refusal, so the zero Result is gone rather than defaulted.
func refusalFor(err error, op Operation) (Result, error) {
	if code, ok := err.(missingField); ok {
		return refuse(op, string(code)), nil
	}
	return Result{}, err
}

// refuse builds the refusal Result every check in this file returns. ClientState
// echoes the payload so a device can reconcile against what it believes it sent.
func refuse(op Operation, code string) Result {
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeRefused, ErrorCode: code,
		ClientState: op.Payload,
	}
}

// ---------------------------------------------------------------------------
// Payload readers used only by this file
//
// These sit beside the others in pgstore.go rather than there because they are
// specific to outings, and pgstore.go is already long. Each returns nil for an
// absent or wrongly typed value so the caller's check reads as "did the client
// send this?" rather than repeating a type assertion at every call site.

// payloadStrings reads a list of strings, treating anything else as absent.
//
// A single string where a list was expected is treated as absent rather than as a
// one-element list: a client that meant one trainee and sent "7" has sent the
// wrong shape, and coercing it would create an outing whose trainee list is a
// numeric id where the column expects text.
func payloadStrings(m map[string]any, k string) []string {
	raw, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// nonNilStrings makes an absent list into an empty one.
//
// The distinction is invisible in Go and decisive in SQL: a nil slice binds as
// NULL and an empty slice binds as an empty array, and a column that is not null
// with a default rejects the first and accepts the second.
func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// payloadTimePtr reads a timestamp. A string is accepted and parsed, because JSON
// has no date type and every client sends one; an unparseable one is absent
// rather than the zero time, so a malformed date is refused as a missing field
// instead of becoming 0001-01-01 in a report.
func payloadTimePtr(m map[string]any, k string) *time.Time {
	switch v := m[k].(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil
		}
		u := parsed.UTC()
		return &u
	case time.Time:
		u := v.UTC()
		return &u
	default:
		return nil
	}
}

// payloadRoute reads a LineString as WKT, or nil.
//
// A camp has no route, and an outing whose route was sent as something this
// cannot read is treated as having no route rather than as having a broken one.
// That is the wrong answer for a drive — the route is the thing a report draws —
// so it is stated here rather than left to be discovered: a route that cannot be
// parsed is not silently discarded, it is refused by the caller's own check, and
// only the camp path treats nil as acceptable.
func payloadRoute(m map[string]any) *string {
	s, ok := m["route"].(string)
	if !ok {
		return nil
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}
