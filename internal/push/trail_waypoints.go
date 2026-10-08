package push

import (
	"context"
)

// entityTrailWaypoint is the wire name for one point on a walk.
const entityTrailWaypoint = "trail_waypoint"

// createTrailWaypoint appends one point to a walk.
//
// A trail is a sequence of points, and this operation appends one point to that
// sequence. Each point is its own operation, so a batch can carry a whole walk
// and a device out of coverage can replay the points it recorded in order; the
// ordinal is the client's own ordering — the preserved sequence is the content,
// not an attribute of it.
//
// There is deliberately no update or delete path for a waypoint. A walk is
// recorded as it happened; moving a point after the fact would rewrite what
// actually occurred, which is the one thing this table exists to preserve. The
// grants enforce that: fieldapp holds only select and insert on trail_waypoint.
func (t *pgxTx) createTrailWaypoint(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "trail_waypoint_without_entity_id"), nil
	}
	if op.BaseRevision != nil {
		return refuse(op, "create_with_base_revision"), nil
	}

	outingID := payloadUUID(op.Payload, "outing_id")
	if !isUUID(outingID) {
		return refuse(op, "trail_waypoint_without_outing"), nil
	}
	known, err := t.outingInContext(ctx, outingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		// One code for both cases, as everywhere else in this package: an outing
		// the caller cannot see is indistinguishable from one that does not
		// exist.
		return refuse(op, "unknown_outing"), nil
	}

	ordinal, ok := payloadInt(op.Payload, "ordinal").(int)
	if !ok || ordinal < 0 {
		return refuse(op, "trail_waypoint_without_ordinal"), nil
	}

	longitude := payloadFloatPtr(op.Payload, "longitude")
	latitude := payloadFloatPtr(op.Payload, "latitude")
	if longitude == nil || latitude == nil {
		return refuse(op, "trail_waypoint_without_location"), nil
	}

	const q = `
		insert into trail_waypoint
		  (id, outing_id, context_code, ordinal, point,
		   captured_at, accuracy_m, elevation_m, note)
		values ($1,$2,$3,$4, st_setsrid(st_makepoint($5,$6),4326),
		        $7,$8,$9,$10)`
	_, err = t.tx.Exec(ctx, q,
		op.EntityID, outingID, caller.WriteContext, ordinal,
		*longitude, *latitude,
		op.CapturedAt,
		payloadFloatPtr(op.Payload, "accuracy_m"),
		payloadFloatPtr(op.Payload, "elevation_m"),
		payloadTextPtr(op.Payload, "note"))
	if isUniqueViolation(err) {
		// Either the id or the (outing_id, ordinal) pair already exists — a
		// retry of a point the client already pushed, or a number collision.
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
