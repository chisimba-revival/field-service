// Package outings provides the outings catalogue.
// Outings are drives, hikes, and camps. They are reference data scoped to the
// caller's context grants, so a guide in the northern reserve sees only the
// outings that belong to that reserve's context.
//
// The store acquires a dedicated connection from the pool for each call and
// sets the row-level security session variable on it before querying. This
// keeps the pool connection clean for the next caller: the setting is
// explicitly reset (via set_config with is_local=false) before the connection
// is returned.
package outings

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store reads outings from the database.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a store over a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Outing is one outing (drive, hike, or camp) as the service returns it.
type Outing struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"` // "drive" | "hike" | "camp"
	ContextCode  string   `json:"context_code"`
	GuideID      string   `json:"guide_id"`
	TraineeIDs   []string `json:"trainee_ids"`
	Status       string   `json:"status"`                  // "planned" | "active" | "completed" | "sealed"
	PlannedStart *string  `json:"planned_start,omitempty"` // RFC3339
	EndedAt      *string  `json:"ended_at,omitempty"`      // RFC3339
	SealedAt     *string  `json:"sealed_at,omitempty"`     // RFC3339
	Notes        *string  `json:"notes,omitempty"`
	Revision     int64    `json:"revision"`
}

// DriveDetail holds the fields specific to a drive.
type DriveDetail struct {
	OutingID      string   `json:"outing_id"`
	DurationHours *float64 `json:"duration_hours,omitempty"`
	GuestCount    *int     `json:"guest_count,omitempty"`
}

// HikeDetail holds the fields specific to a hike.
type HikeDetail struct {
	OutingID       string   `json:"outing_id"`
	RifleRole      string   `json:"rifle_role"` // "first" | "second" | "neither"
	WalkLengthKm   float64  `json:"walk_length_km"`
	HoursWalked    *float64 `json:"hours_walked,omitempty"`
	Description    *string  `json:"description,omitempty"`
	LessonsLearned *string  `json:"lessons_learned,omitempty"`
}

// CampDetail holds the fields specific to a camp.
type CampDetail struct {
	OutingID   string  `json:"outing_id"`
	SiteName   *string `json:"site_name,omitempty"`
	Facilities *string `json:"facilities,omitempty"`
}

// OutingWithDetail is an outing with its kind-specific detail.
type OutingWithDetail struct {
	Outing
	DriveDetail *DriveDetail `json:"drive_detail,omitempty"`
	HikeDetail  *HikeDetail  `json:"hike_detail,omitempty"`
	CampDetail  *CampDetail  `json:"camp_detail,omitempty"`
}

// List returns all outings visible to the caller's grants.
func (s *Store) List(ctx context.Context, grants []string) ([]OutingWithDetail, error) {
	if len(grants) == 0 {
		return nil, nil
	}

	tx, err := s.withGrants(ctx, grants)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		SELECT o.id, o.kind, o.context_code, o.guide_id, o.trainee_ids,
		       o.status, o.planned_start_time, o.end_time, o.sealed_at, o.notes, 0 as revision
		FROM outing o
		WHERE o.context_code = ANY($1)
		ORDER BY o.planned_start_time DESC
	`

	rows, err := tx.Query(ctx, query, grants)
	if err != nil {
		return nil, fmt.Errorf("outings: list: %w", err)
	}
	defer rows.Close()

	var result []OutingWithDetail
	for rows.Next() {
		var o OutingWithDetail
		if err := rows.Scan(&o.ID, &o.Kind, &o.ContextCode, &o.GuideID, &o.TraineeIDs,
			&o.Status, &o.PlannedStart, &o.EndedAt, &o.SealedAt, &o.Notes, &o.Revision); err != nil {
			return nil, fmt.Errorf("outings: list scan: %w", err)
		}
		result = append(result, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("outings: list rows: %w", err)
	}

	// Fetch detail for each outing using the same granted transaction.
	for i := range result {
		o := &result[i]
		switch o.Kind {
		case "drive":
			dd, err := s.fetchDriveDetail(ctx, tx, o.ID)
			if err == nil && dd != nil {
				o.DriveDetail = dd
			}
		case "hike":
			hd, err := s.fetchHikeDetail(ctx, tx, o.ID)
			if err == nil && hd != nil {
				o.HikeDetail = hd
			}
		case "camp":
			cd, err := s.fetchCampDetail(ctx, tx, o.ID)
			if err == nil && cd != nil {
				o.CampDetail = cd
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outings: commit: %w", err)
	}
	return result, nil
}

// InContext reports whether the outing exists in the given context.
// This is used by signoffs to verify the outing is in the caller's context.
func (s *Store) InContext(ctx context.Context, outingID, contextCode string) (bool, error) {
	tx, err := s.withGrants(ctx, []string{contextCode})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM outing WHERE id = $1 AND context_code = $2)
	`, outingID, contextCode).Scan(&exists)
	if err != nil {
		return false, err
	}
	err = tx.Commit(ctx)
	return exists, err
}

// Get returns one outing by id, only if it is in a context the caller may read.
// The check against grants is done here rather than in the route so that no
// code path can return an outing from an ungranted context.
func (s *Store) Get(ctx context.Context, grants []string, outingID string) (*OutingWithDetail, error) {
	if len(grants) == 0 {
		return nil, nil
	}

	tx, err := s.withGrants(ctx, grants)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	query := `
		SELECT o.id, o.kind, o.context_code, o.guide_id, o.trainee_ids,
		       o.status, o.planned_start_time, o.end_time, o.sealed_at, o.notes, 0 as revision
		FROM outing o
		WHERE o.id = $1 AND o.context_code = ANY($2)
	`

	var o OutingWithDetail
	err = tx.QueryRow(ctx, query, outingID, grants).Scan(
		&o.ID, &o.Kind, &o.ContextCode, &o.GuideID, &o.TraineeIDs,
		&o.Status, &o.PlannedStart, &o.EndedAt, &o.SealedAt, &o.Notes, &o.Revision)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outings: get: %w", err)
	}

	// Fetch kind-specific detail using the same granted transaction.
	switch o.Kind {
	case "drive":
		dd, err := s.fetchDriveDetail(ctx, tx, o.ID)
		if err == nil && dd != nil {
			o.DriveDetail = dd
		}
	case "hike":
		hd, err := s.fetchHikeDetail(ctx, tx, o.ID)
		if err == nil && hd != nil {
			o.HikeDetail = hd
		}
	case "camp":
		cd, err := s.fetchCampDetail(ctx, tx, o.ID)
		if err == nil && cd != nil {
			o.CampDetail = cd
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("outings: commit: %w", err)
	}
	return &o, nil
}

// withGrants acquires a connection from the pool and begins a transaction
// with the row-level security session variable set for the given grants.
// The transaction is returned so the caller can defer Rollback after Commit.
// The setting is transaction-local: it never leaks to the next pool user.
func (s *Store) withGrants(ctx context.Context, grants []string) (pgx.Tx, error) {
	if len(grants) == 0 {
		return nil, fmt.Errorf("outings: no context grants")
	}

	// Validate grants: no empty strings, no commas (they would break the
	// RLS policy's string_to_array parsing).
	for _, g := range grants {
		if g == "" || strings.Contains(g, ",") {
			return nil, fmt.Errorf("outings: invalid context grant %q", g)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("outings: begin transaction: %w", err)
	}

	// set_config with is_local=true scopes the setting to this transaction.
	// When the transaction ends (commit or rollback), the pool connection
	// has no trace of this caller's grants.
	joined := strings.Join(grants, ",")
	if _, err := tx.Exec(ctx, `select set_config('app.context_grants', $1, true)`, joined); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("outings: set grants: %w", err)
	}

	return tx, nil
}

// fetchDriveDetail returns the drive detail for an outing, or nil if not found.
func (s *Store) fetchDriveDetail(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, outingID string) (*DriveDetail, error) {
	var d DriveDetail
	err := q.QueryRow(ctx, `
		SELECT outing_id, duration_hours, guest_count
		FROM drive_detail
		WHERE outing_id = $1
	`, outingID).Scan(&d.OutingID, &d.DurationHours, &d.GuestCount)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// fetchHikeDetail returns the hike detail for an outing, or nil if not found.
func (s *Store) fetchHikeDetail(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, outingID string) (*HikeDetail, error) {
	var h HikeDetail
	err := q.QueryRow(ctx, `
		SELECT outing_id, rifle_role, walk_length_km, hours_walked, description, lessons_learned
		FROM hike_detail
		WHERE outing_id = $1
	`, outingID).Scan(&h.OutingID, &h.RifleRole, &h.WalkLengthKm, &h.HoursWalked, &h.Description, &h.LessonsLearned)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// fetchCampDetail returns the camp detail for an outing, or nil if not found.
func (s *Store) fetchCampDetail(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, outingID string) (*CampDetail, error) {
	var c CampDetail
	err := q.QueryRow(ctx, `
		SELECT outing_id, site_name, facilities
		FROM camp_detail
		WHERE outing_id = $1
	`, outingID).Scan(&c.OutingID, &c.SiteName, &c.Facilities)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}
