package push

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Updating a drive, a hike or a camp.
//
// A drive's end cannot be known when the drive is created. The duration, the
// hours driven in daylight and after dark, the off-road rollup — all of it is a
// fact of the finished drive, and a drive is created at its start. That second
// half arrives as an update against the revision the create returned. The same
// shape serves a hike's walk length and hours, and the fields a camp only
// learns when it is over.
//
// An update is sparse: only the fields named in the payload change, and a field
// the client does not send is left exactly as it was. Everything that is
// present is validated first, in Go, so one bad field refuses that operation
// instead of killing the batch with a constraint violation — the same rule the
// create path lives by, and for the same reason: a batch is not all-or-nothing.
//
// The gate is the base revision. An update names the revision it saw, and if
// the record has moved on, the update is refused with the same code a
// correction uses. Nobody may write to a revision they have not read.

func (t *pgxTx) updateOuting(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "update_without_entity_id"), nil
	}

	// A base revision is what makes an update an update rather than a create
	// wearing a different name. It is also what makes a retry safe: the client
	// sends the revision it saw, and the refusal or success is recorded against
	// the operation id either way.
	if op.BaseRevision == nil {
		return refuse(op, "update_without_base_revision"), nil
	}

	// The outing is read within the caller's context, as the create path reads
	// its outing. Reporting "no such outing" for one that exists in somebody
	// else's context would make this an oracle, so both cases share the code —
	// the same choice correct() makes, and made for the same reason: the grant
	// set exists to withhold exactly this knowledge.
	var kind string
	var serverRevision int64
	var serverState []byte
	err := t.tx.QueryRow(ctx,
		`select kind, revision, to_jsonb(o) - 'route' - 'trainee_ids'
		   from outing o where o.id = $1 and o.context_code = $2`,
		op.EntityID, caller.WriteContext).
		Scan(&kind, &serverRevision, &serverState)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "update_unknown_outing"), nil
	}
	if err != nil {
		return Result{}, err
	}

	// Everything is validated before anything is written, and everything the
	// database's constraints would reject is rejected here. A constraint
	// violation fails the transaction, which from here is a 500 for the whole
	// batch; an update that names a negative off-road time is a refusal of that
	// operation, not a dead batch.
	if err := validateUpdateFields(op, kind); err != nil {
		return refusalFor(err, op)
	}
	if kind == "camp" && payloadRoute(op.Payload) != nil {
		return refuse(op, "camp_with_route"), nil
	}
	if status := payloadText(op.Payload, "status"); status != "" && !outingStatuses[status] {
		return refuse(op, "unknown_outing_status"), nil
	}

	if serverRevision != *op.BaseRevision {
		// Revisions, never timestamps. A device offline for three days submits
		// an update captured this morning; comparing clocks would have it win.
		var existing map[string]any
		_ = json.Unmarshal(serverState, &existing)
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision, ServerState: existing,
			ClientState: op.Payload,
		}, nil
	}

	// The outing row first, then the detail row, then the feed — all in the one
	// transaction that Apply holds open, so a failure anywhere rolls back all of
	// it and the outcome is never recorded for a change that did not happen.
	const q = `
		update outing set
			revision           = revision + 1,
			status             = coalesce($2::text, status),
			planned_start_time = coalesce($3::timestamptz, planned_start_time),
			start_time         = coalesce($4::timestamptz, start_time),
			end_time           = coalesce($5::timestamptz, end_time),
			weather            = coalesce($6::text, weather),
			notes              = coalesce($7::text, notes),
			route              = coalesce($8::geometry, route)
		where id = $1
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID,
		payloadTextPtr(op.Payload, "status"),
		payloadTimePtr(op.Payload, "planned_start_time"),
		payloadTimePtr(op.Payload, "start_time"),
		payloadTimePtr(op.Payload, "end_time"),
		payloadTextPtr(op.Payload, "weather"),
		payloadTextPtr(op.Payload, "notes"),
		payloadRoute(op.Payload),
	).Scan(&revision)
	if err != nil {
		return Result{}, err
	}

	// The detail row for this outing's kind. The coalesce makes it sparse the
	// same way the outing update is: a field the client did not send is left as
	// it was, and a field sent as null is not a request to blank it.
	//
	// The row exists because a create wrote it, or the drive backfill in 0007
	// did; this is an update, never an insert, and nothing in this path may
	// invent a detail row for an outing that never had one.
	switch kind {
	case "drive":
		_, err := t.tx.Exec(ctx,
			`update drive_detail set
				vehicle_id          = coalesce($2::text, vehicle_id),
				inspection_oil_ok   = coalesce($3::boolean, inspection_oil_ok),
				inspection_water_ok = coalesce($4::boolean, inspection_water_ok),
				inspection_tyres_ok = coalesce($5::boolean, inspection_tyres_ok),
				daylight_hours      = coalesce($6::numeric, daylight_hours),
				night_hours         = coalesce($7::numeric, night_hours),
				off_road_seconds    = coalesce($8::integer, off_road_seconds),
				off_track_used      = coalesce($9::boolean, off_track_used),
				duration_hours      = coalesce($10::numeric, duration_hours),
				guest_count         = coalesce($11::integer, guest_count)
			where outing_id = $1`,
			op.EntityID,
			payloadTextPtr(op.Payload, "vehicle_id"),
			payloadBoolPtr(op.Payload, "inspection_oil_ok"),
			payloadBoolPtr(op.Payload, "inspection_water_ok"),
			payloadBoolPtr(op.Payload, "inspection_tyres_ok"),
			payloadFloatPtr(op.Payload, "daylight_hours"),
			payloadFloatPtr(op.Payload, "night_hours"),
			payloadIntPtr(op.Payload, "off_road_seconds"),
			payloadBoolPtr(op.Payload, "off_track_used"),
			payloadFloatPtr(op.Payload, "duration_hours"),
			payloadIntPtr(op.Payload, "guest_count"))
		if err != nil {
			return Result{}, err
		}

	case "hike":
		_, err := t.tx.Exec(ctx,
			`update hike_detail set
				guide_role      = coalesce($2::text, guide_role),
				rifle_details   = coalesce($3::text, rifle_details),
				rifle_role      = coalesce($4::text, rifle_role),
				walk_length_km  = coalesce($5::numeric, walk_length_km),
				hours_walked    = coalesce($6::numeric, hours_walked),
				description     = coalesce($7::text, description),
				lessons_learned = coalesce($8::text, lessons_learned)
			where outing_id = $1`,
			op.EntityID,
			payloadTextPtr(op.Payload, "guide_role"),
			payloadTextPtr(op.Payload, "rifle_details"),
			payloadTextPtr(op.Payload, "rifle_role"),
			payloadFloatPtr(op.Payload, "walk_length_km"),
			payloadFloatPtr(op.Payload, "hours_walked"),
			payloadTextPtr(op.Payload, "description"),
			payloadTextPtr(op.Payload, "lessons_learned"))
		if err != nil {
			return Result{}, err
		}

	case "camp":
		_, err := t.tx.Exec(ctx,
			`update camp_detail set
				site_name  = coalesce($2::text, site_name),
				facilities = coalesce($3::text, facilities)
			where outing_id = $1`,
			op.EntityID,
			payloadTextPtr(op.Payload, "site_name"),
			payloadTextPtr(op.Payload, "facilities"))
		if err != nil {
			return Result{}, err
		}
	}

	// The feed is written for the new revision, so a device that pulls learns of
	// the update and of the revision it should now hold against the outing id.
	if err := t.feed(ctx, caller, op, revision); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: revision,
	}, nil
}
