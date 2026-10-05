//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"
)

// Creating a drive, a hike or a camp, against the real service and the real
// database.
//
// The point of this file is that each kind is asked only for what that kind
// requires. The schema keeps those requirements in three separate detail tables
// precisely so they cannot all become optional, and these tests are what would
// notice if that stopped being true: a hike carrying a guest count instead of a
// rifle role has to be refused, and a drive carrying a rifle role has to have it
// ignored rather than stored.
//
// The refusals are asserted by code and not only by outcome, because "refused" on
// its own would be satisfied by refusing everything, and this suite already has
// a test proving an outing in the caller's own context still lands.

// anOuting builds an outing create operation. The caller supplies the fields, so
// each test says exactly which ones it is providing and which it is leaving out.
//
// The operation id is derived from the readable slug rather than sent as one.
// operation_outcome.operation_id is a uuid column, so a slug is a syntax error
// there, and it surfaces as "commit unexpectedly resulted in rollback" rather
// than as the error it is — the transaction is already poisoned by the time
// anything tries to report it.
func (r *rig) anOuting(opID, entityID, kind string, fields map[string]any) map[string]any {
	payload := map[string]any{"outing_kind": kind}
	for k, v := range fields {
		payload[k] = v
	}
	return map[string]any{
		"operation_id": r.uuidFor("op:" + opID),
		"entity":       "outing",
		"kind":         "create",
		"entity_id":    entityID,
		"payload":      payload,
	}
}

// oneRefused pushes a single-operation batch and returns the one result.
//
// The *testing.T is passed rather than read from the rig, because a helper that
// fails through r.t reports against the setup rather than against the assertion
// that found the fault.
func oneRefused(t *testing.T, r *rig, tok string, op map[string]any, wantCode string) []byte {
	t.Helper()
	status, body := r.push(tok, batch(op))
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", status, body)
	}
	got := results(t, body)
	if len(got) != 1 {
		t.Fatalf("want one result, got %d: %s", len(got), body)
	}
	if got[0].Outcome != "refused" {
		t.Fatalf("outcome = %q, want refused: %s", got[0].Outcome, body)
	}
	if got[0].ErrorCode != wantCode {
		t.Fatalf("error_code = %q, want %q: %s", got[0].ErrorCode, wantCode, body)
	}
	return body
}

// outingColumns reads one column from the outing table.
func (r *rig) outingColumn(t *testing.T, id, column string) string {
	t.Helper()
	var v string
	q := "select coalesce(" + column + "::text, '') from outing where id = $1"
	if err := r.pool.QueryRow(context.Background(), q, id).Scan(&v); err != nil {
		t.Fatalf("reading outing %s.%s: %v", id, column, err)
	}
	return v
}

// A drive, a hike and a camp each land with the right kind and the right detail
// row. Asserted against the database rather than the response, because the
// response only says "applied" and the claim being made is about what was
// written.
func TestEachKindOfOutingIsCreatedWithItsOwnDetail(t *testing.T) {
	r := start(t)
	ctx := "e2e-kinds"
	tok := r.token(0, ctx)

	drive := r.anOuting("op-out-drive", r.uuidFor("drive"), "drive", map[string]any{
		"duration_hours": 3.5, "guest_count": 6, "guide_id": "guide-1",
	})
	hike := r.anOuting("op-out-hike", r.uuidFor("hike"), "hike", map[string]any{
		"rifle_role": "first", "walk_length_km": 8.25, "hours_walked": 4,
		"lessons_learned": "Track discipline: hold fire until the whole herd passes.",
	})
	camp := r.anOuting("op-out-camp", r.uuidFor("camp"), "camp", map[string]any{
		"site_name": "Kopje", "facilities": "water, dry latrine",
	})

	status, body := r.push(tok, batch(drive, hike, camp))
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	got := results(t, body)
	if len(got) != 3 {
		t.Fatalf("want three results, got %d: %s", len(got), body)
	}
	for _, g := range got {
		if g.Outcome != "applied" {
			t.Fatalf("%s: outcome %q, code %q", g.OperationID, g.Outcome, g.ErrorCode)
		}
	}

	for _, c := range []struct{ kind, id, want string }{
		{"drive", r.uuidFor("drive"), "drive"},
		{"hike", r.uuidFor("hike"), "hike"},
		{"camp", r.uuidFor("camp"), "camp"},
	} {
		if got := r.outingColumn(t, c.id, "kind"); got != c.want {
			t.Errorf("outing kind = %q, want %q", got, c.want)
		}
		if got := r.outingColumn(t, c.id, "context_code"); got != ctx {
			t.Errorf("outing context = %q, want %q — the context comes from the token",
				got, ctx)
		}
	}

	// One detail row each, and none of them on the wrong table.
	for table, id := range map[string]string{
		"drive_detail": r.uuidFor("drive"),
		"hike_detail":  r.uuidFor("hike"),
		"camp_detail":  r.uuidFor("camp"),
	} {
		var n int
		q := "select count(*) from " + table + " where outing_id = $1"
		if err := r.pool.QueryRow(context.Background(), q, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s has %d rows for the outing, want 1", table, n)
		}
	}

	// A hike's required fields, read back, because "the row exists" is not the
	// same as "the rifle role was stored".
	var role string
	if err := r.pool.QueryRow(context.Background(),
		`select rifle_role from hike_detail where outing_id = $1`, r.uuidFor("hike")).
		Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "first" {
		t.Errorf("rifle_role = %q, want %q", role, "first")
	}
}

// Each kind is asked for its own required fields. A refusal naming the field is
// what lets a device prompt for it, so the codes are asserted, not just the
// outcome.
func TestEachKindIsAskedOnlyForWhatItRequires(t *testing.T) {
	r := start(t)
	ctx := "e2e-required"
	tok := r.token(0, ctx)

	cases := []struct {
		name, kind, wantCode string
		fields               map[string]any
	}{
		{"a drive with no duration", "drive", "drive_without_duration",
			map[string]any{"guest_count": 2}},
		{"a drive with no guest count", "drive", "drive_without_guest_count",
			map[string]any{"duration_hours": 2}},
		{"a drive with a zero duration", "drive", "drive_without_duration",
			map[string]any{"duration_hours": 0, "guest_count": 2}},
		{"a hike with no rifle role", "hike", "hike_without_rifle_role",
			map[string]any{"walk_length_km": 5}},
		{"a hike with a blank rifle role", "hike", "hike_without_rifle_role",
			map[string]any{"rifle_role": "", "walk_length_km": 5}},
		{"a hike with no walk length", "hike", "hike_without_walk_length",
			map[string]any{"rifle_role": "second"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := r.anOuting("op-req-"+c.name, r.uuidFor(c.name), c.kind, c.fields)
			oneRefused(t, r, tok, op, c.wantCode)

			// Nothing half-written. An outing whose detail was refused must not
			// exist, or the next attempt would collide with this one and the
			// failure would look like a duplicate id rather than a refusal.
			var n int
			if err := r.pool.QueryRow(context.Background(),
				"select count(*) from outing where id = $1", op["entity_id"]).
				Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%d outing rows written for a refused operation, want 0", n)
			}
		})
	}
}

// "neither" is a legitimate rifle role, not a missing value. A participant who was
// on neither rifle is a fact the programme counts separately from one the client
// forgot about, so collapsing them would lose a real record.
func TestANeitherRifleRoleIsAcceptedRatherThanRefused(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-neither")

	op := r.anOuting("op-neither", r.uuidFor("neither"), "hike", map[string]any{
		"rifle_role": "neither", "walk_length_km": 3.5,
	})
	if status, body := r.push(tok, batch(op)); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}

	var role string
	if err := r.pool.QueryRow(context.Background(),
		`select rifle_role from hike_detail where outing_id = $1`, op["entity_id"]).
		Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "neither" {
		t.Errorf("rifle_role = %q, want %q", role, "neither")
	}
}

// A camp is a place and a duration and does not move, so a route on one is
// refused. A LineString attached to a stationary record is a path somebody drove
// that is now attributed to the camp, and a report reading it back would place
// the camp somewhere it never was.
func TestACampCarryingARouteIsRefused(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-camproute")

	op := r.anOuting("op-camp-route", r.uuidFor("camproute"), "camp", map[string]any{
		"site_name": "Kopje",
		"route":     "LINESTRING(36.8219 -1.2921, 36.8220 -1.2922)",
	})
	oneRefused(t, r, tok, op, "camp_with_route")

	var n int
	if err := r.pool.QueryRow(context.Background(),
		"select count(*) from camp_detail where outing_id = $1", op["entity_id"]).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a camp detail row was written for a refused operation")
	}
}

// A drive may carry a route, and this is the control for the test above: the camp
// refusal has to be about the kind, not about the field being refused everywhere.
func TestADriveCarryingARouteIsAccepted(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-driveroute")

	op := r.anOuting("op-drive-route", r.uuidFor("driveroute"), "drive", map[string]any{
		"duration_hours": 2, "guest_count": 3,
		"route": "LINESTRING(36.8219 -1.2921, 36.8220 -1.2922)",
	})
	if status, body := r.push(tok, batch(op)); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var kind, route string
	if err := r.pool.QueryRow(context.Background(),
		"select kind, coalesce(st_astext(route), '') from outing where id = $1",
		op["entity_id"]).Scan(&kind, &route); err != nil {
		t.Fatal(err)
	}
	if kind != "drive" || route == "" {
		t.Errorf("kind = %q, route = %q; want a drive with a route stored", kind, route)
	}
}

// A kind nobody has heard of is refused with a code that says so, rather than
// reaching the database's check constraint and arriving as a 500.
func TestAnUnknownKindIsRefusedRatherThanAttempted(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-unknownkind")

	for _, kind := range []string{"", "walk", "Drive", "camping", "safari"} {
		op := r.anOuting("op-unknown-"+kind, r.uuidFor("unknown-"+kind), kind, map[string]any{
			"duration_hours": 2, "guest_count": 2,
		})
		oneRefused(t, r, tok, op, "unknown_outing_kind")
	}
}

// The change feed carries an outing's own row, not a sighting's.
//
// This exists because of a defect the other tests could not see. The feed read
// from sighting for every operation, so creating a hike found no sighting and the
// no-row error poisoned the transaction — the client was told "commit
// unexpectedly resulted in rollback" on a write that had succeeded, and would
// have retried it for ever. Every other test here passed while that was true,
// because none of them looked at the feed.
//
// Asserting the feed entity_type and that the body is the outing's own row is
// what makes the choice of table visible. Asserting only that a feed row exists
// would be satisfied by a sighting's body under an outing's name.
func TestTheChangeFeedCarriesTheOutingsOwnRow(t *testing.T) {
	r := start(t)
	tok := r.token(0, "e2e-feed")

	id := r.uuidFor("feed-hike")
	op := r.anOuting("op-feed", id, "hike", map[string]any{
		"rifle_role": "first", "walk_length_km": 7.5, "guide_id": "guide-1",
	})
	if status, body := r.push(tok, batch(op)); status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}

	var entityType string
	var body map[string]any
	err := r.pool.QueryRow(context.Background(),
		`select entity_type, body from change_feed where entity_id = $1`, id).
		Scan(&entityType, &body)
	if err != nil {
		t.Fatalf("no change-feed row for the outing: %v", err)
	}
	if entityType != "outing" {
		t.Errorf("entity_type = %q, want %q", entityType, "outing")
	}
	if body["id"] != id {
		t.Errorf("feed body carries id %v, want the outing's own %s — the feed read "+
			"the wrong table", body["id"], id)
	}
	if _, isSighting := body["species_code"]; isSighting {
		t.Error("the feed body is a sighting row, not an outing row")
	}
	if kind, _ := body["kind"].(string); kind != "hike" {
		t.Errorf("feed body kind = %q, want %q", kind, "hike")
	}
}

// A sighting hangs off an outing, so a hike's sighting is recorded the same way a
// drive's is. Without this the whole change would be a new table nothing writes
// to, and the sighting tests would pass against drives alone.
func TestASightingCanHangOffAHikeOrACamp(t *testing.T) {
	r := start(t)

	// The ids are scoped to this test rather than reusing "hike" and "camp".
	// uuidFor derives from the name, so a shared name is the same id, and
	// TestEachKindOfOutingIsCreatedWithItsOwnDetail had already created it — so
	// this test's create collided on the primary key and the sighting never got
	// as far as being the thing under test.
	for _, kind := range []struct{ k, id string }{
		{"hike", r.uuidFor("sighting-hike")},
		{"camp", r.uuidFor("sighting-camp")},
	} {
		t.Run(kind.k, func(t *testing.T) {
			ctx := "e2e-sight-" + kind.k
			tok := r.token(0, ctx)

			fields := map[string]any{}
			if kind.k == "hike" {
				fields = map[string]any{"rifle_role": "second", "walk_length_km": 6}
			} else {
				fields = map[string]any{"site_name": "Kopje"}
			}
			outing := r.anOuting("op-sight-"+kind.k, kind.id, kind.k, fields)
			if status, body := r.push(tok, batch(outing)); status != http.StatusOK {
				t.Fatalf("creating the %s: %d %s", kind.k, status, body)
			}

			op := r.sighting(ctx)
			op["payload"].(map[string]any)["outing_id"] = kind.id
			if status, body := r.push(tok, batch(op)); status != http.StatusOK {
				t.Fatalf("status %d: %s", status, body)
			}

			var gotKind string
			if err := r.pool.QueryRow(context.Background(),
				`select o.kind from sighting s join outing o on o.id = s.outing_id
				 where s.id = $1`, op["entity_id"]).Scan(&gotKind); err != nil {
				t.Fatal(err)
			}
			if gotKind != kind.k {
				t.Errorf("sighting's outing kind = %q, want %q", gotKind, kind.k)
			}
		})
	}
}

// uuidFor derives a stable uuid from a name, so each test can name its outings
// and refer to them again.
//
// The version and variant nibbles are overwritten in place rather than skipped.
// Slicing around them instead drops a character, and a uuid one character short is
// rejected by the column with a syntax error naming the value and nothing about
// the shape.
func (r *rig) uuidFor(name string) string {
	sum := sha256.Sum256([]byte("outing:" + name))
	h := hex(sum[:16])
	h = h[:12] + "4" + h[13:16] + "8" + h[17:]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
