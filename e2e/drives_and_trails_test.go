//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Drives, hikes and trails, plus dangerous game encounters, tested against the
// live service and live database. The tests mirror the contract the client
// relies on: each new field lands in its own table, updates are revision-gated
// and sparse, refusals carry precise codes, and the change feed names the
// entity's own row.

func TestDriveCreateCarriesExtendedFields(t *testing.T) {
	r := start(t)
	ctx := "e2e-drive-extend"
	tok := r.token(0, ctx)

	id := r.uuidFor("drive-extended")
	op := r.anOuting("op-drive-ext", id, "drive", map[string]any{
		"duration_hours":      2.5,
		"guest_count":         3,
		"guide_id":            "guide-1",
		"vehicle_id":          "GB-4521",
		"inspection_oil_ok":   true,
		"inspection_water_ok": true,
		"inspection_tyres_ok": false,
		"daylight_hours":      2.5,
		"night_hours":         0,
		"off_road_seconds":    1800,
		"off_track_used":      true,
	})

	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res := results(t, body)
	if len(res) != 1 || res[0].Outcome != "applied" {
		t.Fatalf("want applied, got %+v: %s", res, body)
	}
	if res[0].NewRevision != 1 {
		t.Errorf("new_revision = %d, want 1", res[0].NewRevision)
	}

	var n int
	if err := r.pool.QueryRow(context.Background(),
		`select count(*) from drive_detail where outing_id = $1`, id).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("drive_detail rows = %d, want 1", n)
	}

	var vehicle string
	var tyres bool
	var daylight, night float64
	var offSec int
	var offTrack bool
	if err := r.pool.QueryRow(context.Background(),
		`select vehicle_id, inspection_tyres_ok, daylight_hours, night_hours, off_road_seconds, off_track_used
		   from drive_detail where outing_id = $1`, id).
		Scan(&vehicle, &tyres, &daylight, &night, &offSec, &offTrack); err != nil {
		t.Fatal(err)
	}
	if vehicle != "GB-4521" {
		t.Errorf("vehicle_id = %q", vehicle)
	}
	if tyres {
		t.Errorf("inspection_tyres_ok should be false")
	}
	if daylight != 2.5 {
		t.Errorf("daylight_hours = %g", daylight)
	}
	if night != 0 {
		t.Errorf("night_hours = %g", night)
	}
	if offSec != 1800 {
		t.Errorf("off_road_seconds = %d", offSec)
	}
	if !offTrack {
		t.Errorf("off_track_used should be true")
	}

	var ent string
	var b map[string]any
	if err := r.pool.QueryRow(context.Background(),
		`select entity_type, body from change_feed where entity_id = $1`, id).
		Scan(&ent, &b); err != nil {
		t.Fatal(err)
	}
	if ent != "outing" {
		t.Errorf("entity_type = %q", ent)
	}
	if b["id"] != id {
		t.Errorf("feed body id mismatch")
	}
}

func TestDriveUpdateAppliesRollupsAndBumpsRevision(t *testing.T) {
	r := start(t)
	ctx := "e2e-drive-update"
	tok := r.token(0, ctx)

	id := r.uuidFor("drive-update")
	op := r.anOuting("op-drive-up", id, "drive", map[string]any{
		"duration_hours": 2,
		"guest_count":    2,
		"guide_id":       "guide-1",
	})

	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res := results(t, body)
	if len(res) != 1 || res[0].Outcome != "applied" {
		t.Fatalf("want applied, got %+v: %s", res, body)
	}
	base := res[0].NewRevision

	up := map[string]any{
		"operation_id":  r.uuidFor("op-drive-upd"),
		"entity":        "outing",
		"kind":          "update",
		"entity_id":     id,
		"base_revision": base,
		"payload": map[string]any{
			"status":              "completed",
			"end_time":            "2026-10-05T15:30:00Z",
			"duration_hours":      3.5,
			"guest_count":         5,
			"vehicle_id":          "GB-4521",
			"daylight_hours":      2.5,
			"night_hours":         1.5,
			"off_road_seconds":    1200,
			"off_track_used":      true,
			"inspection_oil_ok":   true,
			"inspection_water_ok": true,
			"inspection_tyres_ok": true,
		},
	}

	status, body = r.push(tok, batch(up))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res = results(t, body)
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d: %s", len(res), body)
	}
		if res[0].Outcome != "applied" {
		t.Fatalf("outcome %q code %q", res[0].Outcome, res[0].ErrorCode)
	}
	if res[0].NewRevision != base+1 {
		t.Errorf("new_revision = %d, want %d", res[0].NewRevision, base+1)
	}

	var statusOut string
	var duration float64
	var vehicle string
	if err := r.pool.QueryRow(context.Background(),
		`select o.status, d.duration_hours, d.vehicle_id
		   from outing o join drive_detail d on d.outing_id = o.id
		  where o.id = $1`, id).
		Scan(&statusOut, &duration, &vehicle); err != nil {
		t.Fatal(err)
	}
	if statusOut != "completed" {
		t.Errorf("status = %q", statusOut)
	}
	if duration != 3.5 {
		t.Errorf("duration = %g", duration)
	}
	if vehicle != "GB-4521" {
		t.Errorf("vehicle = %q", vehicle)
	}

	var ent2 string
	var b2 map[string]any
	if err := r.pool.QueryRow(context.Background(),
		`select entity_type, body from change_feed where entity_id = $1 and revision = $2`, id, res[0].NewRevision).
		Scan(&ent2, &b2); err != nil {
		t.Fatal(err)
	}
	if ent2 != "outing" {
		t.Errorf("entity_type = %q", ent2)
	}
	if rev, _ := b2["revision"].(float64); int64(rev) != int64(res[0].NewRevision) {
		t.Errorf("feed body revision = %v", b2["revision"])
	}
	if st, _ := b2["status"].(string); st != "completed" {
		t.Errorf("feed body status = %q", st)
	}
}

func TestHikeCreateCarriesGuideAndRifleDetails(t *testing.T) {
	r := start(t)
	ctx := "e2e-hike-extra"
	tok := r.token(0, ctx)

	id := r.uuidFor("hike-extra")
	op := r.anOuting("op-hike-ext", id, "hike", map[string]any{
		"rifle_role":      "first",
		"walk_length_km":  8.25,
		"hours_walked":    4,
		"guide_id":        "guide-1",
		"guide_role":      "lead",
		"rifle_details":   "375 H&H",
		"lessons_learned": "Track discipline",
	})

	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res := results(t, body)
	if len(res) != 1 || res[0].Outcome != "applied" {
		t.Fatalf("want applied, got %+v: %s", res, body)
	}

	var guideRole, rifleDetails string
	if err := r.pool.QueryRow(context.Background(),
		`select guide_role, rifle_details from hike_detail where outing_id = $1`, id).
		Scan(&guideRole, &rifleDetails); err != nil {
		t.Fatal(err)
	}
	if guideRole != "lead" {
		t.Errorf("guide_role = %q", guideRole)
	}
	if rifleDetails != "375 H&H" {
		t.Errorf("rifle_details = %q", rifleDetails)
	}
}

func TestDangerousGameEncounterCreate(t *testing.T) {
	r := start(t)
	ctx := "e2e-enc"
	tok := r.token(0, ctx)

	outingID := r.ensureOuting(ctx)

	id := r.uuidFor("enc-create")
	op := map[string]any{
		"operation_id": r.uuidFor("op-enc-1"),
		"entity":       "dangerous_game_encounter",
		"kind":         "create",
		"entity_id":    id,
		"payload": map[string]any{
			"outing_id":        outingID,
			"species_code":     "LEOP",
			"distance_m":       80,
			"animal_behaviour": "charging",
			"action_taken":     "warning shot",
			"longitude":        36.8219,
			"latitude":         -1.2921,
			"captured_at":      "2026-10-05T08:15:00Z",
			"note":             "close approach",
		},
	}

	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res := results(t, body)
	if len(res) != 1 {
		t.Fatalf("want 1 result, got %d: %s", len(res), body)
	}
		if res[0].Outcome != "applied" {
		t.Fatalf("outcome %q code %q", res[0].Outcome, res[0].ErrorCode)
	}
	if res[0].NewRevision != 1 {
		t.Errorf("new_revision = %d", res[0].NewRevision)
	}

	var n int
	if err := r.pool.QueryRow(context.Background(),
		`select count(*) from dangerous_game_encounter where id = $1`, id).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("rows = %d", n)
	}

	var ent string
	var b map[string]any
	if err := r.pool.QueryRow(context.Background(),
		`select entity_type, body from change_feed where entity_id = $1`, id).
		Scan(&ent, &b); err != nil {
		t.Fatal(err)
	}
	if ent != "dangerous_game_encounter" {
		t.Errorf("entity_type = %q", ent)
	}
	if sp, _ := b["species_code"].(string); sp != "LEOP" {
		t.Errorf("species_code = %q", sp)
	}
}

func TestTrailWaypointCreate(t *testing.T) {
	r := start(t)
	ctx := "e2e-wpt"
	tok := r.token(0, ctx)

	outingID := r.ensureOuting(ctx)

	id := r.uuidFor("wpt-create")
	op := map[string]any{
		"operation_id": r.uuidFor("op-wpt-1"),
		"entity":       "trail_waypoint",
		"kind":         "create",
		"entity_id":    id,
		"payload": map[string]any{
			"outing_id":   outingID,
			"ordinal":     0,
			"longitude":   36.8219,
			"latitude":    -1.2921,
			"elevation_m": 1450.5,
			"accuracy_m":  2.5,
			"captured_at": "2026-10-05T09:00:00Z",
			"note":        "track fork",
		},
	}

	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	res := results(t, body)
	if len(res) != 1 || res[0].Outcome != "applied" {
		t.Fatalf("want applied, got %+v: %s", res, body)
	}
	if res[0].NewRevision != 1 {
		t.Errorf("new_revision = %d", res[0].NewRevision)
	}

	var ord int
	var elev float64
	if err := r.pool.QueryRow(context.Background(),
		`select ordinal, elevation_m from trail_waypoint where id = $1`, id).
		Scan(&ord, &elev); err != nil {
		t.Fatal(err)
	}
	if ord != 0 {
		t.Errorf("ordinal = %d", ord)
	}
	if elev != 1450.5 {
		t.Errorf("elevation_m = %g", elev)
	}

	var ent string
	var b map[string]any
	if err := r.pool.QueryRow(context.Background(),
		`select entity_type, body from change_feed where entity_id = $1`, id).
		Scan(&ent, &b); err != nil {
		t.Fatal(err)
	}
	if ent != "trail_waypoint" {
		t.Errorf("entity_type = %q", ent)
	}
	if oid, _ := b["outing_id"].(string); oid != outingID {
		t.Errorf("outing_id = %q", oid)
	}
}

func TestUpdateRefusals(t *testing.T) {
	r := start(t)
	ctx := "e2e-up-ref"
	tok := r.token(0, ctx)

	id := r.uuidFor("up-ref")
	op := r.anOuting("op-up-ref", id, "drive", map[string]any{
		"duration_hours": 1,
		"guest_count":    1,
		"guide_id":       "guide-1",
	})
	if status, body := r.push(tok, batch(op)); status != http.StatusOK {
		t.Fatalf("setup: %d %s", status, body)
	}

	t.Run("update_without_base_revision", func(t *testing.T) {
		up := map[string]any{
			"operation_id": r.uuidFor("up-no-base"),
			"entity":       "outing",
			"kind":         "update",
			"entity_id":    id,
			"payload":      map[string]any{"status": "completed"},
		}
		oneRefused(t, r, tok, up, "update_without_base_revision")
	})

	t.Run("update_wrong_revision", func(t *testing.T) {
		up := map[string]any{
			"operation_id":  r.uuidFor("up-wrong"),
			"entity":        "outing",
			"kind":          "update",
			"entity_id":     id,
			"base_revision": 9,
			"payload":       map[string]any{"status": "completed"},
		}
		status, body := r.push(tok, batch(up))
		if status != http.StatusOK {
			t.Fatalf("status %d: %s", status, body)
		}
		res := results(t, body)
		if len(res) != 1 || res[0].Outcome != "refused" || res[0].ErrorCode != "revision_conflict" {
			t.Fatalf("want revision_conflict, got %+v: %s", res, body)
		}
	})

	t.Run("update_unknown_outing", func(t *testing.T) {
		up := map[string]any{
			"operation_id":  r.uuidFor("up-unk"),
			"entity":        "outing",
			"kind":          "update",
			"entity_id":     "00000000-0000-4000-8000-000000000000",
			"base_revision": 1,
			"payload":       map[string]any{"status": "completed"},
		}
		oneRefused(t, r, tok, up, "update_unknown_outing")
	})
}

func TestEncounterAndWaypointRefusalsAndPull(t *testing.T) {
	r := start(t)
	ctx := "e2e-ew-ref"
	tok := r.token(0, ctx)

	outingID := r.ensureOuting(ctx)

	t.Run("encounter_unknown_outing", func(t *testing.T) {
		id := r.uuidFor("enc-unk")
		op := map[string]any{
			"operation_id": r.uuidFor("enc-unk"),
			"entity":       "dangerous_game_encounter",
			"kind":         "create",
			"entity_id":    id,
			"payload": map[string]any{
				"outing_id":    "00000000-0000-4000-8000-000000000000",
				"species_code": "LEOP",
				"longitude":    36.82,
				"latitude":     -1.29,
			},
		}
		oneRefused(t, r, tok, op, "unknown_outing")
		var n int
		if err := r.pool.QueryRow(context.Background(),
			`select count(*) from dangerous_game_encounter where id = $1`, id).
			Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("created %d rows for refused encounter", n)
		}
	})

	t.Run("waypoint_ordinal_missing", func(t *testing.T) {
		id := r.uuidFor("wpt-bad")
		op := map[string]any{
			"operation_id": r.uuidFor("wpt-bad"),
			"entity":       "trail_waypoint",
			"kind":         "create",
			"entity_id":    id,
			"payload": map[string]any{
				"outing_id": outingID,
				"longitude": 36.82,
				"latitude":  -1.29,
			},
		}
		oneRefused(t, r, tok, op, "trail_waypoint_without_ordinal")
	})

	t.Run("pull_roundtrip_encounter_and_waypoint", func(t *testing.T) {
		encID := r.uuidFor("enc-rt")
		enc := map[string]any{
			"operation_id": r.uuidFor("enc-rt"),
			"entity":       "dangerous_game_encounter",
			"kind":         "create",
			"entity_id":    encID,
			"payload": map[string]any{
				"outing_id":    outingID,
				"species_code": "LEOP",
				"longitude":    36.8219,
				"latitude":     -1.2921,
			},
		}
		wptID := r.uuidFor("wpt-rt")
		wpt := map[string]any{
			"operation_id": r.uuidFor("wpt-rt"),
			"entity":       "trail_waypoint",
			"kind":         "create",
			"entity_id":    wptID,
			"payload": map[string]any{
				"outing_id":   outingID,
				"ordinal":     5,
				"longitude":   36.8220,
				"latitude":    -1.2920,
				"elevation_m": 1445.0,
			},
		}
		status, pbody := r.push(tok, batch(enc, wpt))
		if status != http.StatusOK {
			t.Fatalf("push: %d %s", status, pbody)
		}
		res := results(t, pbody)
		if len(res) != 2 || res[0].Outcome != "applied" || res[1].Outcome != "applied" {
			t.Fatalf("want both applied, got %+v", res)
		}

		status, body := r.pull(tok, "")
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
			t.Fatalf("decode: %v %s", err, body)
		}
		if page.Status != "ok" {
			t.Errorf("status %q", page.Status)
		}

		foundEnc := false
		foundWpt := false
		for _, c := range page.Changes {
			if c.EntityID == encID {
				if sp, _ := c.Body["species_code"].(string); sp == "LEOP" {
					foundEnc = true
				}
			}
			if c.EntityID == wptID {
				if ord, ok := c.Body["ordinal"].(float64); ok && int(ord) == 5 {
					foundWpt = true
				}
			}
		}
		if !foundEnc || !foundWpt {
			t.Errorf("did not find both pushed entities in pull: enc=%t wpt=%t, changes=%d", foundEnc, foundWpt, len(page.Changes))
		}
	})
}
