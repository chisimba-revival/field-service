package push

import (
	"context"
)

// entityDangerousGameEncounter is the wire name for one dangerous-game
// encounter.
//
// It lives beside the log book rather than in it: sighting a dangerous animal
// is a different act from logging one, and the report reader treats the two
// differently — an encounter is about the distance and the reaction, a log
// entry is about the count and the behaviour. Same outing, same context, same
// client-minted id; different table.
const entityDangerousGameEncounter = "dangerous_game_encounter"

// createEncounter records one dangerous-game encounter.
//
// The rules are the same ones the sighting path lives by: the context comes
// from the caller's token, the outing is read within that context, and every
// field the database would reject is validated here, in Go, so a bad operation
// refuses itself instead of taking the batch down with it.
func (t *pgxTx) createEncounter(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "encounter_without_entity_id"), nil
	}
	if op.BaseRevision != nil {
		return refuse(op, "create_with_base_revision"), nil
	}

	outingID := payloadUUID(op.Payload, "outing_id")
	if !isUUID(outingID) {
		return refuse(op, "encounter_without_outing"), nil
	}
	known, err := t.outingInContext(ctx, outingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		// One code for both cases, exactly as the sighting path does: an outing
		// in somebody else's context is indistinguishable from a nonexistent
		// one, which is the whole point of the grant set.
		return refuse(op, "unknown_outing"), nil
	}

	// A location is the one thing an encounter cannot be about without. Read
	// through the pointer forms so that an absent (0, 0) is not mistaken for a
	// real coordinate at the prime meridian's equator.
	longitude := payloadFloatPtr(op.Payload, "longitude")
	latitude := payloadFloatPtr(op.Payload, "latitude")
	if longitude == nil || latitude == nil {
		return refuse(op, "encounter_without_location"), nil
	}
	if species := payloadText(op.Payload, "species_code"); species == "" {
		return refuse(op, "encounter_without_species"), nil
	}
	if d := payloadFloatPtr(op.Payload, "distance_m"); d != nil && *d < 0 {
		return refuse(op, "encounter_negative_distance"), nil
	}

	const q = `
		insert into dangerous_game_encounter
		  (id, context_code, outing_id, species_code, distance_m, animal_behaviour,
		   action_taken, location, captured_at, recorded_at, created_by, revision, note)
		values ($1,$2,$3,$4,$5,$6,$7, st_setsrid(st_makepoint($8,$9),4326),
		        $10,$11,$12,1,$13)`
	_, err = t.tx.Exec(ctx, q,
		op.EntityID, caller.WriteContext, outingID,
		payloadText(op.Payload, "species_code"),
		payloadFloatPtr(op.Payload, "distance_m"),
		payloadTextPtr(op.Payload, "animal_behaviour"),
		payloadTextPtr(op.Payload, "action_taken"),
		*longitude, *latitude,
		op.CapturedAt, recordedAt(op, t.now()), caller.ID,
		payloadTextPtr(op.Payload, "note"))
	if isUniqueViolation(err) {
		return refuse(op, "already_exists"), nil
	}
	if err != nil {
		return Result{}, err
	}

	if err := t.feed(ctx, caller, op, 1); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: 1,
	}, nil
}
