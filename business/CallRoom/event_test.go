package business

import (
	"context"
	"errors"
	"testing"
	"time"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// eventRooms is the store behind the seams for events' calls: meetings by id,
// and each room's links, revoked or not, at the moment they were.
type eventRooms struct {
	meetings map[string]*model.Meeting
	links    map[string][]*guestModel.GuestGrant
	// taken is an address another live link has, which reviving refuses.
	taken    string
	removed  []string
	reopened []string
	txs      int
	// saved are the meetings made, and locked those whose rows were held.
	saved, locked []string
}

func withEventRooms(t *testing.T) *eventRooms {
	t.Helper()
	f := &eventRooms{meetings: map[string]*model.Meeting{}, links: map[string][]*guestModel.GuestGrant{}}
	oMeeting, oArchive, oReopen, oRevoke, oRevive := meetingOf, archiveMeeting, reopenMeeting, revokeLinks, reviveLinks
	oLinks, oRemove, oTx, oNow := liveLinks, removeGuests, inTx, now
	t.Cleanup(func() {
		meetingOf, archiveMeeting, reopenMeeting, revokeLinks, reviveLinks = oMeeting, oArchive, oReopen, oRevoke, oRevive
		liveLinks, removeGuests, inTx, now = oLinks, oRemove, oTx, oNow
	})
	clock := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	meetingOf = func(_ context.Context, id string) (*model.Meeting, error) {
		if m := f.meetings[id]; m != nil {
			c := *m
			return &c, nil
		}
		return nil, nil
	}
	archiveMeeting = func(_ context.Context, id string, at time.Time) (bool, error) {
		m := f.meetings[id]
		if m == nil || m.ArchivedAt != nil {
			return false, nil
		}
		m.ArchivedAt = &at
		return true, nil
	}
	reopenMeeting = func(_ context.Context, id string, at time.Time) (bool, error) {
		m := f.meetings[id]
		if m == nil || m.ArchivedAt == nil || !m.ArchivedAt.Equal(at) {
			return false, nil
		}
		m.ArchivedAt = nil
		f.reopened = append(f.reopened, id)
		return true, nil
	}
	revokeLinks = func(_ context.Context, _, room string, at time.Time) (int64, error) {
		var n int64
		for _, g := range f.links[room] {
			if g.RevokedAt == nil {
				g.RevokedAt = &at
				n++
			}
		}
		return n, nil
	}
	reviveLinks = func(_ context.Context, _, room string, at time.Time) (int64, error) {
		var back []*guestModel.GuestGrant
		for _, g := range f.links[room] {
			if g.RevokedAt != nil && g.RevokedAt.Equal(at) {
				if g.Slug != nil && *g.Slug == f.taken {
					return 0, &pq.Error{Code: "23505"}
				}
				back = append(back, g)
			}
		}
		for _, g := range back {
			g.RevokedAt = nil
		}
		return int64(len(back)), nil
	}
	liveLinks = func(_ context.Context, _, room string) ([]*guestModel.GuestGrant, error) {
		var out []*guestModel.GuestGrant
		for _, g := range f.links[room] {
			if g.RevokedAt == nil {
				out = append(out, g)
			}
		}
		return out, nil
	}
	removeGuests = func(_ context.Context, room string) { f.removed = append(f.removed, room) }
	oSave, oLock, oNewRoom, oGuestOn, oAllowed, oTitle := saveMeeting, lockMeeting, newEventRoom, guestOn, roomsAllowed, title
	t.Cleanup(func() {
		saveMeeting, lockMeeting, newEventRoom, guestOn, roomsAllowed, title = oSave, oLock, oNewRoom, oGuestOn, oAllowed, oTitle
	})
	guestOn, roomsAllowed = func() bool { return true }, func() bool { return true }
	title = func(_ context.Context, room string, _ uuid.UUID) string { return f.meetings[room[len("meet-"):]].Title }
	saveMeeting = func(_ context.Context, m model.Meeting) error {
		if f.meetings[m.ID] == nil {
			f.meetings[m.ID] = &m
			f.saved = append(f.saved, m.ID)
		}
		return nil
	}
	lockMeeting = func(ctx context.Context, id string) (*model.Meeting, error) {
		f.locked = append(f.locked, id)
		return meetingOf(ctx, id)
	}
	newEventRoom = func(_ context.Context, room string, owner uuid.UUID, title string) (*guestModel.GuestGrant, error) {
		slug := "call-" + uuid.NewString()[:8]
		g := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: room, Capability: guestModel.CapabilityJoin,
			Slug: &slug, CreatedBy: owner}
		f.links[room] = append(f.links[room], g)
		return g, nil
	}
	inTx = func(ctx context.Context, fn func(context.Context) error) error {
		f.txs++
		return fn(ctx)
	}
	return f
}

// call is a meeting of kind (an event's made for an event of its own) and its
// room's standing link at slug, live.
func (f *eventRooms) call(kind, slug string) (*model.Meeting, string) {
	m := &model.Meeting{ID: uuid.NewString(), Kind: kind, Owner: uuid.New()}
	if kind == model.KindEvent {
		m.Event = uuid.New()
	}
	f.meetings[m.ID] = m
	room := roomOf(m)
	s := slug
	f.links[room] = append(f.links[room], &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting,
		ResourceID: room, Capability: guestModel.CapabilityJoin, Slug: &s})
	return m, room
}

// address is the room's live standing link's address, "" for none.
func (f *eventRooms) address(room string) string {
	links, _ := liveLinks(context.Background(), guestModel.ResourceMeeting, room)
	if g := addressOf(links); g != nil {
		return *g.Slug
	}
	return ""
}

// calendar V-641: an event's call closed with its event opens again, as it
// was, when the event comes back from the trash: its meeting and the links
// that ended with it, at the same address, in one transaction. A link an
// admin turned off before the close stays off; a call whose address doesn't
// answer again (that link off, or its address taken since) stays closed; a
// person's room, or a call that is open, is left as it is.
func TestAnEventCallClosedWithItsEventOpensAgainAtItsAddress(t *testing.T) {
	f := withEventRooms(t)
	ctx := context.Background()
	m, room := f.call(model.KindEvent, "design-review-3f9a1c2e")
	// A link a host gave out, and one an admin turned off before.
	earlier := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	f.links[room] = append(f.links[room],
		&guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: room, Capability: guestModel.CapabilityJoin},
		&guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: room, Capability: guestModel.CapabilityJoin, RevokedAt: &earlier})

	if err := CloseEventCall(ctx, m.Event, room); err != nil {
		t.Fatal(err)
	}
	if f.meetings[m.ID].ArchivedAt == nil || f.address(room) != "" || len(f.removed) != 1 {
		t.Fatalf("closed: %+v at %q, guests put out of %v", f.meetings[m.ID], f.address(room), f.removed)
	}
	closedAt := *f.meetings[m.ID].ArchivedAt
	for _, g := range f.links[room][:2] {
		if g.RevokedAt == nil || !g.RevokedAt.Equal(closedAt) {
			t.Fatalf("a link revoked at %v, not with its meeting at %v", g.RevokedAt, closedAt)
		}
	}

	txs := f.txs
	if mine, err := ReopenEventCall(ctx, m.Event, room); err != nil || !mine {
		t.Fatalf("opened again: %v, %v", mine, err)
	}
	if f.txs != txs+1 || f.meetings[m.ID].ArchivedAt != nil || f.address(room) != "design-review-3f9a1c2e" {
		t.Fatalf("opened again: %d transactions, %+v at %q", f.txs-txs, f.meetings[m.ID], f.address(room))
	}
	if links := f.links[room]; links[1].RevokedAt != nil || links[2].RevokedAt == nil || !links[2].RevokedAt.Equal(earlier) {
		t.Fatalf("the host's link back, the admin's still off: %v, %v", links[1].RevokedAt, links[2].RevokedAt)
	}
	// Open, it is left as it is.
	if mine, err := ReopenEventCall(ctx, m.Event, room); err != nil || !mine || len(f.reopened) != 1 {
		t.Fatalf("an open call opened again: %v, %v, %v", mine, err, f.reopened)
	}

	// Its address taken since: it stays closed, and isn't a failure.
	if err := CloseEventCall(ctx, m.Event, room); err != nil {
		t.Fatal(err)
	}
	f.taken = "design-review-3f9a1c2e"
	if mine, err := ReopenEventCall(ctx, m.Event, room); err != nil || !mine || f.meetings[m.ID].ArchivedAt == nil || len(f.reopened) != 1 {
		t.Fatalf("its address taken: %v, %v, %+v", mine, err, f.meetings[m.ID])
	}

	// Its standing link turned off by an admin before the close: it stays
	// closed, so its event can be given a new call.
	other, otherRoom := f.call(model.KindEvent, "retro-9a8b7c6d")
	off := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	f.links[otherRoom][0].RevokedAt = &off
	if err := CloseEventCall(ctx, other.Event, otherRoom); err != nil {
		t.Fatal(err)
	}
	if _, err := ReopenEventCall(ctx, other.Event, otherRoom); err != nil || f.meetings[other.ID].ArchivedAt == nil || f.address(otherRoom) != "" {
		t.Fatalf("its link off before: %v, %+v at %q", err, f.meetings[other.ID], f.address(otherRoom))
	}

	// A person's room closed is closed for good; a room no meeting's is no call.
	personal, personalRoom := f.call(model.KindPersonal, "maya-chen")
	if err := closeMeeting(ctx, personal); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{personalRoom, guestBusiness.MeetingRoomPrefix + uuid.NewString(), "dm-123"} {
		if mine, err := ReopenEventCall(ctx, uuid.Nil, r); err != nil || mine {
			t.Errorf("%s: %v, %v", r, mine, err)
		}
	}
	if f.meetings[personal.ID].ArchivedAt == nil || len(f.reopened) != 1 {
		t.Errorf("a person's room opened again: %+v, %v", f.meetings[personal.ID], f.reopened)
	}
}

// calendar V-641 (main, 6 Nov): only the event a call was made for ends it or
// opens it again. Another event holding its room (a new series of "this and
// following" shares its series' call) leaves it as it is, open or closed, and
// is told the room isn't its call, so it comes back from the trash without it.
func TestOnlyTheEventACallWasMadeForEndsItOrOpensItAgain(t *testing.T) {
	f := withEventRooms(t)
	ctx := context.Background()
	m, room := f.call(model.KindEvent, "stand-up-5d1e2f3a")
	tail := uuid.New()

	if err := CloseEventCall(ctx, tail, room); err != nil {
		t.Fatal(err)
	}
	if f.meetings[m.ID].ArchivedAt != nil || f.address(room) != "stand-up-5d1e2f3a" || len(f.removed) != 0 {
		t.Fatalf("another event ended the call: %+v at %q, guests put out of %v", f.meetings[m.ID], f.address(room), f.removed)
	}
	if mine, err := ReopenEventCall(ctx, tail, room); err != nil || mine {
		t.Fatalf("another event's open call: %v, %v; want it not its own", mine, err)
	}

	if err := CloseEventCall(ctx, m.Event, room); err != nil || f.meetings[m.ID].ArchivedAt == nil {
		t.Fatalf("its event ended it: %v, %+v", err, f.meetings[m.ID])
	}
	if mine, err := ReopenEventCall(ctx, tail, room); err != nil || mine || f.meetings[m.ID].ArchivedAt == nil || f.address(room) != "" {
		t.Fatalf("another event back: %v, %v; the call %+v at %q, want it closed", mine, err, f.meetings[m.ID], f.address(room))
	}
	// A call that knows no event (its event purged) is no event's to reach.
	m.Event = uuid.Nil
	if mine, err := ReopenEventCall(ctx, uuid.Nil, room); err != nil || mine || len(f.reopened) != 0 {
		t.Fatalf("a call of no event: %v, %v, opened %v", mine, err, f.reopened)
	}
}

// gcal-15 head B: an event's own room ("meet-<its id>") is made ready the
// first time anyone joins it: its meeting, the event's and named for it, with
// no link, under no guest switch (members need no guest access), the same
// meeting however often it is asked; a room that is a call made before is
// left as it is. A guest link added later goes on the room members are in:
// its own room, or the event's call made before, opened again if it was
// closed, given a link only when none answers; never another event's call,
// which a new series of "this and following" shares.
func TestAnEventsRoomIsReadyOnFirstJoinAndItsGuestLinkAdoptsIt(t *testing.T) {
	f := withEventRooms(t)
	ctx := context.Background()
	guestOn = func() bool { return false }
	event, organiser := uuid.New(), uuid.New()
	own := guestBusiness.MeetingRoomPrefix + event.String()
	for range 2 {
		if err := ReadyEventRoom(ctx, event, organiser, "Weekly sync", own); err != nil {
			t.Fatal(err)
		}
	}
	m := f.meetings[event.String()]
	if m == nil || m.Kind != model.KindEvent || m.Event != event || m.Owner != organiser || m.Title != "Weekly sync" || len(f.saved) != 1 {
		t.Fatalf("its room's meeting: %+v (saved %v)", m, f.saved)
	}
	if len(f.links[own]) != 0 {
		t.Fatalf("readying its room made links: %v", f.links[own])
	}
	older, olderRoom := f.call(model.KindEvent, "retro-1a2b3c4d")
	if err := ReadyEventRoom(ctx, older.Event, older.Owner, "Retro", olderRoom); err != nil || len(f.saved) != 1 {
		t.Fatalf("a call made before, readied: %v, saved %v", err, f.saved)
	}

	// The guest link, under the guest switches as ever.
	if _, err := EventCall(ctx, event, organiser, "Weekly sync", own); !errors.Is(err, guestBusiness.ErrGuestDisabled) {
		t.Fatalf("a guest link with guest access off: %v", err)
	}
	guestOn = func() bool { return true }
	r, err := EventCall(ctx, event, organiser, "Weekly sync", own)
	if err != nil || r.Room != own || f.address(own) == "" || len(f.saved) != 1 || len(f.locked) != 1 {
		t.Fatalf("the guest link: %+v, %v; at %q, saved %v, locked %v", r, err, f.address(own), f.saved, f.locked)
	}
	links := len(f.links[own])
	if again, err := EventCall(ctx, event, organiser, "Weekly sync", own); err != nil || again.Room != own || len(f.links[own]) != links {
		t.Fatalf("asked again: %+v, %v; %d links, want the one", again, err, len(f.links[own]))
	}
	// Taken off (closed), then added again: the same room, open again, a new address.
	if err := CloseEventCall(ctx, event, own); err != nil {
		t.Fatal(err)
	}
	back, err := EventCall(ctx, event, organiser, "Weekly sync", own)
	if err != nil || back.Room != own || f.meetings[event.String()].ArchivedAt != nil || f.address(own) == "" || f.address(own) == r.Slug {
		t.Fatalf("added again: %+v, %v; meeting %+v at %q", back, err, f.meetings[event.String()], f.address(own))
	}
	// The event's call made before, its link off: a new link on that room.
	f.links[olderRoom][0].RevokedAt = func() *time.Time { at := now(); return &at }()
	if c, err := EventCall(ctx, older.Event, older.Owner, "Retro", olderRoom); err != nil || c.Room != olderRoom || f.address(olderRoom) == "" {
		t.Fatalf("its call made before: %+v, %v", c, err)
	}
	// Another event's call it shares: the event's own room instead.
	tail := uuid.New()
	if c, err := EventCall(ctx, tail, older.Owner, "Retro", olderRoom); err != nil || c.Room != guestBusiness.MeetingRoomPrefix+tail.String() {
		t.Fatalf("a tail sharing its series' call: %+v, %v; want its own room", c, err)
	}
}
