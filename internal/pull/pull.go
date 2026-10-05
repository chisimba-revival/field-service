// Package pull answers a device's question: what changed while I was away?
//
// The contract's requirement here is unusual, and the whole package exists to
// satisfy it. A cursor older than the feed window is answered with a distinct
// "resync required" status rather than an empty page, because an empty page
// carries no changes and no cursor: a client cannot tell it from "nothing
// changed" and will sit there healthy, believing it is up to date, while
// receiving nothing for data it has never seen.
//
// The contract calls that the worst possible failure mode for an offline
// client, and it is worse than an error. An error is visible; this looks like
// success. So the status is a first-class part of the answer and a resync is a
// normal path, not a failure — a device that has been out of coverage long
// enough will eventually receive one, and treating it as an error would mean a
// device that has worked correctly for weeks suddenly stops receiving changes
// with nothing to tell the trainee why.
//
// What pull does NOT do is decide what a resync means for the client's data.
// That is the client's call, and it owns the decision because only it knows what
// it holds. This package's job is to say clearly "I cannot tell you what you
// missed", and to be unable to say anything else when it does not know.
package pull

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Status is what the answer turned out to be.
//
// Modelled as a named type rather than a bare string because the difference
// between ok and resync_required is the entire reason this package exists, and
// a bare string is one typo away from collapsing them.
type Status string

const (
	// StatusOK means here is a page of changes.
	StatusOK Status = "ok"

	// StatusResyncRequired means the cursor cannot be honoured. The client must
	// discard its cursors and re-pull its whole accessible scope. No changes
	// were applied and no cursor was issued.
	StatusResyncRequired Status = "resync_required"
)

// Changes is a delivered change.
//
// The body is carried whole rather than as an identity the client then fetches.
// Identity-only would turn one page into N round trips over a connection that is
// already marginal, which is the situation this whole system exists for.
type Change struct {
	Seq       int64          `json:"seq"`
	Entity    string         `json:"entity_type"`
	EntityID  string         `json:"entity_id"`
	Revision  int64          `json:"revision"`
	ChangedAt time.Time      `json:"changed_at"`
	Body      map[string]any `json:"body"`
}

// Page is the answer. One type for both statuses, deliberately.
type Page struct {
	Status Status `json:"status"`

	Changes []Change `json:"changes"`

	// NextCursor is what the client sends next time. It is a string rather than
	// an int64 because a client that has been told to resync has to be able to
	// carry something it does not understand yet; see Service.Cursor.
	NextCursor string `json:"next_cursor,omitempty"`

	// HasMore says whether more changes exist beyond this page. Present so a
	// client knows to come straight back rather than waiting for something to
	// prompt it.
	HasMore bool `json:"has_more"`

	// ServerTime is sent so a client with a wrong clock can still reason about
	// ordering using the server's. It never arbitrates a conflict: the contract
	// is explicit that captured_at and recorded_at are both retained, never
	// interchangeable, and neither settles a disagreement.
	ServerTime time.Time `json:"server_time"`
}

// ErrNoCaller is returned when a pull carries no caller.
//
// The feed is filtered by the caller's grants, so a pull with no caller has no
// boundary to filter by. Defaulting to "everything" would be the one answer
// that is certainly wrong.
var ErrNoCaller = errors.New("pull: no caller")

// MaxPage bounds one page.
//
// Bounded because an unbounded page over a marginal connection is the failure
// the client is most exposed to, and because a page must fit in memory on a
// phone that has been recording all day.
const MaxPage = 200

// Store is the database work a pull needs.
type Store interface {
	// LowestRetainedSeq reports the lowest seq still present in the feed.
	//
	// Everything below it has been pruned by retention. A client whose cursor is
	// below it has missed changes it cannot ask for again, which is the
	// resync condition.
	LowestRetainedSeq(ctx context.Context) (int64, error)

	// Page returns changes after the cursor, in the caller's granted contexts,
	// ordered by seq. It returns at most limit rows.
	//
	// Filtering by context belongs here rather than in this package: the row
	// level policies already enforce it, and a filter written again in Go would
	// be a second answer to a question the database is already answering.
	Page(ctx context.Context, cursor int64, limit int) ([]Change, bool, error)
}

// Service answers pulls.
type Service struct {
	store Store
	now   func() time.Time
	quota int
}

// New builds a service over a store.
func New(store Store) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }, quota: MaxPage}
}

// WithClock replaces the clock, so a test is not asserting against whatever the
// wall clock happened to be.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// WithQuerier returns a copy of this service that reads through q.
//
// A service is normally built once over a connection. That is wrong under a
// pooled server, because the grants that decide what a caller may read are
// transaction-local: a store built over a connection from the pool would find
// none of them and read nothing. So the service is bound to the handle inside the
// transaction that set them.
//
// Returning a copy rather than mutating matters more than it looks. A service
// whose store is swapped in place would be unsafe the moment two requests are in
// flight, and the failure would be a pull reading somebody else's grants.
func (s *Service) WithQuerier(q Querier) *Service {
	s.store = NewPgxStore(q)
	return s
}

// Cursor renders the cursor a client should send next.
//
// A string, and deliberately opaque: a client that starts parsing it has taken
// a dependency on how this service happens to number its feed. The contract
// requires a cursor "that cannot be interpreted" to trigger a resync rather
// than an empty page, which only helps if the client does not reverse-engineer
// the format. This is the one place the format leaks, and it leaks to the
// server, not to the client.
func Cursor(lastSeq int64) string { return strconv.FormatInt(lastSeq, 10) }

// Pull answers one pull request.
//
// The status is decided BEFORE any changes are read, and a resync returns no
// changes at all. A page that is partly applied and partly refused is the one
// state a replica must never be in: the client would hold some of what the
// service has and none of the rest, and would have no way to know which.
func (s *Service) Pull(ctx context.Context, caller string, cursor string) (Page, error) {
	if caller == "" {
		return Page{}, ErrNoCaller
	}

	page := Page{Status: StatusOK, ServerTime: s.now()}

	at, ok := ParseCursor(cursor)
	if !ok {
		return s.resync(page), nil
	}

	if cursor == "" {
		// A device that has never pulled cannot have missed anything, so the
		// retention question does not apply to it and is not asked.
		//
		// Asking it anyway was a real bug, found by running against the database
		// and impossible to find with a fake: seq is a global sequence, so after
		// any pruning the oldest retained row is far above 1, and a brand new
		// device arriving with no cursor was told it had fallen behind and
		// resynced. It could not have fallen behind. Worse, the resync would not
		// have helped it — the rows it had "missed" were gone — so it would have
		// been told to do something that could not work, for ever.
	} else {
		lowest, err := s.store.LowestRetainedSeq(ctx)
		if err != nil {
			// A feed that cannot be read is not a feed with no changes. Reporting
			// resync here would tell the client to discard cursors for the
			// service's benefit, which is the one thing this package must never do.
			return Page{}, err
		}
		if hasMissed(at, lowest) {
			return s.resync(page), nil
		}
	}

	changes, more, err := s.store.Page(ctx, at, s.quota)
	if err != nil {
		return Page{}, err
	}
	page.Changes = changes
	page.HasMore = more
	if len(changes) > 0 {
		page.NextCursor = Cursor(changes[len(changes)-1].Seq)
	}
	return page, nil
}

// ParseCursor reads a cursor this service issued, reporting whether it could.
//
// An uninterpretable cursor is a resync rather than an error, because the
// contract requires it: it means the client is holding something this service
// cannot use, and the honest repair is to start again rather than to guess.
// The second return reports whether the cursor could be read at all. An empty
// cursor reads successfully and means "never pulled" — a full first pull, which
// is ordinary and is not damage.
func ParseCursor(cursor string) (at int64, ok bool) {
	if cursor == "" {
		return 0, true
	}
	at, err := strconv.ParseInt(cursor, 10, 64)
	if err != nil || at < 0 {
		return 0, false
	}
	return at, true
}

// hasMissed reports whether a change between the cursor and the feed could have
// been pruned.
//
// The boundary is exclusive at both ends: the client has everything up to and
// including `at`, and `lowest` is the oldest row that still exists. If the row
// the client has not yet seen — `at + 1` — is older than the oldest row
// retained, it is gone.
func hasMissed(at, lowest int64) bool {
	return at+1 < lowest
}

func (s *Service) resync(page Page) Page {
	page.Status = StatusResyncRequired
	page.Changes = nil
	page.NextCursor = ""
	page.HasMore = false
	return page
}
