package pull_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"field-service/internal/pull"
)

// The rule these tests exist for: a resync must be distinguishable from an empty
// page. An empty page and a refusal both carry no changes and no cursor, and a
// client reading them alike stops receiving updates while looking healthy.

type fakeStore struct {
	lowest    int64
	changes   []pull.Change
	hasMore   bool
	lowestErr error
	pageErr   error

	askedCursor int64
	askedLimit  int
	pageCalls   int
}

func (f *fakeStore) LowestRetainedSeq(context.Context) (int64, error) {
	if f.lowestErr != nil {
		return 0, f.lowestErr
	}
	return f.lowest, nil
}

func (f *fakeStore) Page(_ context.Context, cursor int64, limit int) ([]pull.Change, bool, error) {
	f.pageCalls++
	f.askedCursor, f.askedLimit = cursor, limit
	if f.pageErr != nil {
		return nil, false, f.pageErr
	}
	var out []pull.Change
	for _, c := range f.changes {
		if c.Seq > cursor {
			out = append(out, c)
		}
	}
	return out, f.hasMore, nil
}

func aChange(seq int64, note string) pull.Change {
	return pull.Change{
		Seq: seq, Entity: "sighting", EntityID: "e" + note, Revision: seq,
		ChangedAt: time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC),
		Body:      map[string]any{"notes": note},
	}
}

func aService(store *fakeStore) *pull.Service {
	at := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	return pull.New(store).WithClock(func() time.Time { return at })
}

func TestAPageOfChangesIsServedWithACursorToContinueFrom(t *testing.T) {
	store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(4, "four"), aChange(5, "five")}}
	got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(3))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusOK {
		t.Errorf("status %q, want ok", got.Status)
	}
	if len(got.Changes) != 2 {
		t.Fatalf("%d changes, want 2", len(got.Changes))
	}
	if got.NextCursor != "5" {
		t.Errorf("next cursor %q, want 5. A client that cannot resume without "+
			"it will re-pull the same page forever.", got.NextCursor)
	}
	if store.askedCursor != 3 {
		t.Errorf("store asked from %d, want 3", store.askedCursor)
	}
}

// THE rule. A cursor below the retention window must be refused with a status,
// not answered with an empty page.
func TestACursorOlderThanTheFeedWindowIsRefusedRatherThanAnsweredWithAnEmptyPage(t *testing.T) {
	store := &fakeStore{lowest: 100, changes: nil}
	got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(10))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusResyncRequired {
		t.Fatalf("status %q, want resync_required. An empty page here is the "+
			"worst failure this system has: the client would believe it was up "+
			"to date while receiving nothing for data it has never seen.", got.Status)
	}
	if len(got.Changes) != 0 || got.NextCursor != "" || got.HasMore {
		t.Errorf("a resync carried changes=%d cursor=%q more=%v. It must carry "+
			"nothing: a partly applied page leaves the client holding some of what "+
			"the service has and none of the rest, with no way to tell which.",
			len(got.Changes), got.NextCursor, got.HasMore)
	}
}

// And the other half: an empty page must NOT read as a resync, or every device
// would throw away its state whenever nothing had changed.
func TestAnEmptyPageIsNotAResync(t *testing.T) {
	store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(1, "one")}}
	got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusOK {
		t.Errorf("status %q for an empty page. Nothing changed is the most "+
			"ordinary answer there is, and answering it with a resync would have "+
			"every device discard its cursors whenever the reserve was quiet.",
			got.Status)
	}
	if len(got.Changes) != 0 {
		t.Errorf("%d changes", len(got.Changes))
	}
}

func TestACursorExactlyAtTheRetentionBoundaryIsStillHonoured(t *testing.T) {
	// The client has everything up to and including `at`, and `lowest` is the
	// oldest row still retained. The row it has NOT seen is at+1, so the
	// boundary is: at+1 >= lowest is fine, at+1 < lowest has lost a row.
	//
	// This table was written the other way round first, and the implementation
	// was right: at 99 the client needs row 100, which is retained, so it is
	// served. At 98 it needs row 99, which has been pruned, so it is resynced.
	// Had the table been believed rather than checked, the fix would have
	// resynced a device that was exactly caught up — which is the quiet
	// direction to be wrong in, because the device keeps working and keeps
	// rebuilding state nobody asked it to rebuild.
	for _, tc := range []struct {
		cursor int64
		want   pull.Status
	}{
		{cursor: 99, want: pull.StatusOK},
		{cursor: 98, want: pull.StatusResyncRequired},
	} {
		store := &fakeStore{lowest: 100, changes: []pull.Change{aChange(100, "kept")}}
		got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(tc.cursor))
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != tc.want {
			t.Errorf("cursor %d with lowest retained %d gave %q, want %q. An "+
				"off-by-one here either loses changes or resyncs a client that was "+
				"caught up.", tc.cursor, store.lowest, got.Status, tc.want)
		}
	}
}

// A cursor this service cannot interpret is a resync, per the contract, because
// the honest repair is to start again rather than guess.
func TestACursorThisServiceCannotInterpretIsAResync(t *testing.T) {
	for _, cursor := range []string{"not-a-number", "-4", "12.5", "0x10", " 7"} {
		store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(1, "one")}}
		got, err := aService(store).Pull(context.Background(), "user-1", cursor)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != pull.StatusResyncRequired {
			t.Errorf("cursor %q gave %q, want resync_required", cursor, got.Status)
		}
	}
}

// A device that has never pulled is a full first pull, not a resync.
func TestADeviceThatHasNeverPulledGetsTheWholeFeedRatherThanAResync(t *testing.T) {
	store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(1, "one"), aChange(2, "two")}}
	got, err := aService(store).Pull(context.Background(), "user-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != pull.StatusOK {
		t.Errorf("status %q, want ok. An empty cursor means this device has never "+
			"pulled, which is ordinary and must not be answered as damage.", got.Status)
	}
	if len(got.Changes) != 2 {
		t.Errorf("%d changes on a first pull", len(got.Changes))
	}
	if store.askedCursor != 0 {
		t.Errorf("first pull asked from %d, want 0", store.askedCursor)
	}
}

// A feed that cannot be read must not be dressed up as a resync: that would
// tell the client to discard its cursors for the service's benefit.
func TestAFeedThatCannotBeReadIsAnErrorNotAResync(t *testing.T) {
	store := &fakeStore{lowestErr: errors.New("connection reset")}
	_, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(1))
	if err == nil {
		t.Fatal("an unreadable feed returned no error")
	}
}

func TestAPageThatCannotBeReadIsAnError(t *testing.T) {
	store := &fakeStore{lowest: 1, pageErr: errors.New("statement timeout")}
	_, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(1))
	if err == nil {
		t.Fatal("an unreadable page returned no error")
	}
}

// A pull with no caller has no boundary to filter by, so defaulting to
// everything would be the one answer that is certainly wrong.
func TestAPullWithNoCallerIsRefusedBeforeTheFeedIsRead(t *testing.T) {
	store := &fakeStore{lowest: 1}
	_, err := aService(store).Pull(context.Background(), "", pull.Cursor(1))
	if !errors.Is(err, pull.ErrNoCaller) {
		t.Errorf("got %v, want ErrNoCaller", err)
	}
	if store.pageCalls != 0 {
		t.Errorf("a pull with no caller reached the feed %d times", store.pageCalls)
	}
}

// The page is bounded, because an unbounded page over a marginal connection is
// what the client is most exposed to.
func TestAPageIsBounded(t *testing.T) {
	store := &fakeStore{lowest: 1}
	if _, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(1)); err != nil {
		t.Fatal(err)
	}
	if store.askedLimit != pull.MaxPage {
		t.Errorf("asked for %d rows, want the bound %d", store.askedLimit, pull.MaxPage)
	}
}

func TestHasMoreTellsTheClientToComeStraightBack(t *testing.T) {
	store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(2, "two")}, hasMore: true}
	got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(1))
	if err != nil {
		t.Fatal(err)
	}
	if !got.HasMore {
		t.Error("has_more false while the store reported more")
	}
}

// The server's clock is sent so a client with a wrong one can still reason
// about ordering, and it never arbitrates anything.
func TestTheServerTimeIsSentAndIsTheServersNotTheClients(t *testing.T) {
	store := &fakeStore{lowest: 1}
	want := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	got, err := pull.New(store).WithClock(func() time.Time { return want }).
		Pull(context.Background(), "user-1", pull.Cursor(1))
	if err != nil {
		t.Fatal(err)
	}
	if !got.ServerTime.Equal(want) {
		t.Errorf("server time %v, want %v", got.ServerTime, want)
	}
}

// The cursor is opaque to the client by design: a client that parses it has taken
// a dependency on how this service numbers its feed, which is exactly what makes
// an uninterpretable cursor meaningful.
func TestACursorRoundTrips(t *testing.T) {
	for _, seq := range []int64{0, 1, 42, 1 << 40} {
		at, ok := pull.ParseCursor(pull.Cursor(seq))
		if !ok || at != seq {
			t.Errorf("cursor for %d parsed back as %d, ok=%v", seq, at, ok)
		}
	}
}

// The change body travels whole rather than as an identity to fetch afterwards.
func TestTheBodyTravelsWithTheChangeRatherThanAsAnIdentityToFetch(t *testing.T) {
	store := &fakeStore{lowest: 1, changes: []pull.Change{aChange(1, "a spoor track")}}
	got, err := aService(store).Pull(context.Background(), "user-1", pull.Cursor(0))
	if err != nil {
		t.Fatal(err)
	}
	if got.Changes[0].Body["notes"] != "a spoor track" {
		t.Errorf("body %v carried no content", got.Changes[0].Body)
	}
}
