package business

import (
	"context"
	"errors"
	"testing"
	"time"

	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/CallState"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

// rooms is the store behind the seams: members' open rooms, each room's live
// links, and what was done to them.
type rooms struct {
	meetings  map[uuid.UUID]*model.Meeting
	links     map[string][]*guestModel.GuestGrant
	saved     []model.Meeting
	minted    []string
	published []guestBusiness.PublishSettings
	revoked   []string
	archived  []string
	removed   []string
	txs       int
}

func withRooms(t *testing.T) *rooms {
	t.Helper()
	f := &rooms{meetings: map[uuid.UUID]*model.Meeting{}, links: map[string][]*guestModel.GuestGrant{}}
	oPersonal, oSave, oArchive, oLobby, oTitle := personalMeetingOf, saveMeeting, archiveMeeting, setLobby, title
	oLinks, oRevoke, oNew, oPublish, oRemove := liveLinks, revokeLinks, newRoom, publishLink, removeGuests
	oResolve, oUnlocks, oMint, oTry, oTx := resolve, unlocks, mintUnlock, tryAllowed, inTx
	oGuest, oAllowed, oOn := guestOn, roomsAllowed, roomsOn
	t.Cleanup(func() {
		personalMeetingOf, saveMeeting, archiveMeeting, setLobby, title = oPersonal, oSave, oArchive, oLobby, oTitle
		liveLinks, revokeLinks, newRoom, publishLink, removeGuests = oLinks, oRevoke, oNew, oPublish, oRemove
		resolve, unlocks, mintUnlock, tryAllowed, inTx = oResolve, oUnlocks, oMint, oTry, oTx
		guestOn, roomsAllowed, roomsOn = oGuest, oAllowed, oOn
	})
	personalMeetingOf = func(_ context.Context, owner uuid.UUID) (*model.Meeting, error) {
		if m := f.meetings[owner]; m != nil {
			c := *m
			return &c, nil
		}
		return nil, nil
	}
	saveMeeting = func(_ context.Context, m model.Meeting) error {
		f.saved = append(f.saved, m)
		c := m
		f.meetings[m.Owner] = &c
		return nil
	}
	archiveMeeting = func(_ context.Context, id string, _ time.Time) (bool, error) {
		f.archived = append(f.archived, id)
		for owner, m := range f.meetings {
			if m.ID == id {
				delete(f.meetings, owner)
			}
		}
		return true, nil
	}
	setLobby = func(_ context.Context, id string, guestsWait bool) (bool, error) {
		for _, m := range f.meetings {
			if m.ID == id {
				m.Lobby = model.LobbyNobody
				if guestsWait {
					m.Lobby = model.LobbyGuests
				}
				return true, nil
			}
		}
		return false, nil
	}
	title = func(context.Context, string, uuid.UUID) string { return "Maya Chen's room" }
	liveLinks = func(_ context.Context, _, room string) ([]*guestModel.GuestGrant, error) { return f.links[room], nil }
	revokeLinks = func(_ context.Context, _, room string) (int64, error) {
		f.revoked = append(f.revoked, room)
		n := len(f.links[room])
		delete(f.links, room)
		return int64(n), nil
	}
	newRoom = func(_ context.Context, room string, owner uuid.UUID) (*guestModel.GuestGrant, error) {
		f.minted = append(f.minted, room)
		slug := "maya-chen"
		g := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: room,
			Capability: guestModel.CapabilityJoin, CreatedBy: owner, Slug: &slug}
		f.links[room] = append(f.links[room], g)
		return g, nil
	}
	publishLink = func(_ context.Context, _ uuid.UUID, id uuid.UUID, in guestBusiness.PublishSettings) (*guestModel.GuestGrant, error) {
		f.published = append(f.published, in)
		for _, gs := range f.links {
			for _, g := range gs {
				if g.Id != id {
					continue
				}
				if in.Slug != "" {
					s := in.Slug
					g.Slug = &s
				}
				if in.Password != nil {
					g.PasswordHash = nil
					if *in.Password != "" {
						h := "hashed"
						g.PasswordHash = &h
					}
				}
				return g, nil
			}
		}
		return nil, guestBusiness.ErrInvalidGrant
	}
	removeGuests = func(_ context.Context, room string) { f.removed = append(f.removed, room) }
	inTx = func(ctx context.Context, fn func(context.Context) error) error {
		f.txs++
		return fn(ctx)
	}
	guestOn = func() bool { return true }
	roomsAllowed = func() bool { return true }
	roomsOn = func() bool { return true }
	return f
}

// A member's room is made once, at their name: its meeting and its link in
// one transaction, guests waiting by default; opening it again is the same room.
func TestOpeningARoomMakesOneAtTheOwnersName(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()

	if r, err := Mine(ctx, maya); err != nil || r != nil {
		t.Fatalf("a member with no room: %+v, %v", r, err)
	}
	r, err := Open(ctx, maya)
	if err != nil {
		t.Fatal(err)
	}
	if r.Slug != "maya-chen" || r.Address != "/r/maya-chen" || r.Room != "meet-"+r.MeetingID || !r.GuestsWait || r.Passcode || !r.On || r.Title != "Maya Chen's room" {
		t.Errorf("a new room: %+v", r)
	}
	if len(f.saved) != 1 || f.saved[0].Kind != model.KindPersonal || f.saved[0].Lobby != model.LobbyGuests || f.saved[0].Owner != maya {
		t.Errorf("its meeting: %+v", f.saved)
	}
	if len(f.minted) != 1 || f.txs != 1 {
		t.Errorf("its link is made with its meeting in one transaction: %v, %d", f.minted, f.txs)
	}
	again, err := Open(ctx, maya)
	if err != nil || again.MeetingID != r.MeetingID || len(f.saved) != 1 || len(f.minted) != 1 {
		t.Errorf("opening again is the same room: %+v, %v (%d meetings, %d links)", again, err, len(f.saved), len(f.minted))
	}
}

// A room whose link an admin turned off has no address until its owner
// opens it again, which gives the same meeting a new link.
func TestOpeningAgainGivesARoomItsAddressBack(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()
	r, _ := Open(ctx, maya)
	delete(f.links, r.Room) // revoked in the admin's guest links list

	if mine, err := Mine(ctx, maya); err != nil || mine == nil || mine.Slug != "" || mine.Address != "" {
		t.Fatalf("a room with its link off: %+v, %v", mine, err)
	}
	back, err := Open(ctx, maya)
	if err != nil || back.MeetingID != r.MeetingID || back.Address != "/r/maya-chen" || len(f.minted) != 2 || len(f.saved) != 1 {
		t.Errorf("opened again: %+v, %v (%d links, %d meetings)", back, err, len(f.minted), len(f.saved))
	}
}

// Two Opens at once: the second's meeting is refused by the one-room index,
// and it answers the room the first made.
func TestTwoOpensAtOnceGiveOneRoom(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()
	first := &model.Meeting{ID: uuid.NewString(), Kind: model.KindPersonal, Owner: maya, Lobby: model.LobbyGuests}
	slug := "maya-chen"
	saveMeeting = func(context.Context, model.Meeting) error {
		// The other Open committed while this one was deciding.
		f.meetings[maya] = first
		f.links["meet-"+first.ID] = []*guestModel.GuestGrant{{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: "meet-" + first.ID, Slug: &slug}}
		return &pq.Error{Code: "23505"}
	}
	r, err := Open(ctx, maya)
	if err != nil || r == nil || r.MeetingID != first.ID || r.Address != "/r/maya-chen" || len(f.minted) != 0 {
		t.Errorf("the second Open: %+v, %v (%d links made)", r, err, len(f.minted))
	}
}

// Two people of one name open rooms at once: both see the bare name free,
// the other's room takes it first, and this one's publish is refused, which
// ends its transaction. Open tries once more in a fresh one, which sees the
// name taken and adds letters (DOCS-LEAD-2's review of c9a5485c, 20 Oct).
func TestAnOpenWhoseNameIsTakenMeanwhileTriesAgain(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()
	taken := map[string]bool{}
	newRoom = func(_ context.Context, room string, owner uuid.UUID) (*guestModel.GuestGrant, error) {
		f.minted = append(f.minted, room)
		slug := "maya-chen"
		if taken[slug] {
			slug = "maya-chen-3f9a1c2e"
		} else {
			// Asked and free; the other Maya's room takes it before this
			// one is published.
			taken[slug] = true
			return nil, guestBusiness.ErrSlugTaken
		}
		g := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, ResourceID: room, Capability: guestModel.CapabilityJoin, CreatedBy: owner, Slug: &slug}
		f.links[room] = append(f.links[room], g)
		return g, nil
	}
	// A refused transaction keeps nothing it made.
	inTx = func(ctx context.Context, fn func(context.Context) error) error {
		f.txs++
		before := map[uuid.UUID]*model.Meeting{}
		for k, v := range f.meetings {
			before[k] = v
		}
		err := fn(ctx)
		if err != nil {
			f.meetings = before
		}
		return err
	}
	r, err := Open(ctx, maya)
	if err != nil || r.Slug != "maya-chen-3f9a1c2e" || r.Address != "/r/maya-chen-3f9a1c2e" {
		t.Fatalf("the second try: %+v, %v", r, err)
	}
	if f.txs != 2 || len(f.minted) != 2 || len(f.saved) != 2 {
		t.Errorf("one more try, in a fresh transaction: %d transactions, %d links, %d meetings", f.txs, len(f.minted), len(f.saved))
	}

	// A name taken twice over is said so, not tried for ever.
	newRoom = func(context.Context, string, uuid.UUID) (*guestModel.GuestGrant, error) {
		return nil, guestBusiness.ErrSlugTaken
	}
	if _, err := Open(ctx, uuid.New()); !errors.Is(err, guestBusiness.ErrSlugTaken) {
		t.Errorf("taken on both tries: %v", err)
	}
}

// No room is opened while guest access or personal rooms are off.
func TestARoomIsNotOpenedWhileItsSwitchesAreOff(t *testing.T) {
	f := withRooms(t)
	ctx := context.Background()
	guestOn = func() bool { return false }
	if _, err := Open(ctx, uuid.New()); !errors.Is(err, guestBusiness.ErrGuestDisabled) {
		t.Errorf("guest access off: %v", err)
	}
	guestOn = func() bool { return true }
	roomsAllowed = func() bool { return false }
	if _, err := Open(ctx, uuid.New()); !errors.Is(err, guestBusiness.ErrRoomsOff) {
		t.Errorf("rooms off: %v", err)
	}
	if len(f.saved) != 0 || len(f.minted) != 0 || f.txs != 0 {
		t.Errorf("something was made: %v %v %d", f.saved, f.minted, f.txs)
	}
}

// A room's owner changes its address, its passcode and its lobby, all of
// them in one transaction; a passcode is checked before anything is touched.
func TestChangingARoom(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()
	str := func(s string) *string { return &s }
	no := false

	if _, err := Change(ctx, maya, Changes{GuestsWait: &no}); !errors.Is(err, ErrNoRoom) {
		t.Errorf("a member with no room: %v", err)
	}
	r, _ := Open(ctx, maya)
	var invalid *InvalidChange
	// The rule a page's password keeps too (business/Guest SecretProblem).
	for _, short := range []string{"12", "43219"} {
		if _, err := Change(ctx, maya, Changes{Passcode: str(short)}); !errors.As(err, &invalid) || invalid.Reason != "A passcode is 6 to 128 characters." || len(f.published) != 0 {
			t.Errorf("a %d-character passcode: %v (%d published)", len(short), err, len(f.published))
		}
	}

	txs := f.txs
	got, err := Change(ctx, maya, Changes{Slug: str("standup"), Passcode: str("432198"), GuestsWait: &no})
	if err != nil {
		t.Fatal(err)
	}
	if f.txs != txs+1 || len(f.published) != 1 || f.published[0].Slug != "standup" || f.published[0].Password == nil || *f.published[0].Password != "432198" {
		t.Errorf("one transaction, one publish: %d, %+v", f.txs-txs, f.published)
	}
	if got.Slug != "standup" || got.Address != "/r/standup" || !got.Passcode || got.GuestsWait || got.MeetingID != r.MeetingID {
		t.Errorf("the changed room: %+v", got)
	}
	if got, _ = Change(ctx, maya, Changes{Passcode: str("")}); got.Passcode || f.published[1].Slug != "" {
		t.Errorf("the passcode taken off, the address kept: %+v, %+v", got, f.published[1])
	}
	yes := true
	if got, _ = Change(ctx, maya, Changes{GuestsWait: &yes}); !got.GuestsWait || len(f.published) != 2 {
		t.Errorf("the lobby alone publishes nothing: %+v, %d", got, len(f.published))
	}

	// An address another link has.
	publishLink = func(context.Context, uuid.UUID, uuid.UUID, guestBusiness.PublishSettings) (*guestModel.GuestGrant, error) {
		return nil, guestBusiness.ErrSlugTaken
	}
	if _, err := Change(ctx, maya, Changes{Slug: str("launch-notes")}); !errors.Is(err, guestBusiness.ErrSlugTaken) {
		t.Errorf("a taken address: %v", err)
	}
	// A room whose link an admin turned off: its lobby still changes, its
	// address and passcode wait for it to be opened again.
	delete(f.links, r.Room)
	if _, err := Change(ctx, maya, Changes{Slug: str("standup-2")}); !errors.Is(err, ErrNoAddress) {
		t.Errorf("an address for a room with its link off: %v", err)
	}
	if _, err := Change(ctx, maya, Changes{GuestsWait: &no}); err != nil {
		t.Errorf("its lobby: %v", err)
	}
}

// Closing a room ends its links and its meeting in one transaction, then
// puts its guests out; opening again makes a new room.
func TestClosingARoom(t *testing.T) {
	f := withRooms(t)
	ctx, maya := context.Background(), uuid.New()
	if err := Close(ctx, maya); !errors.Is(err, ErrNoRoom) {
		t.Errorf("closing no room: %v", err)
	}
	r, _ := Open(ctx, maya)
	txs := f.txs
	if err := Close(ctx, maya); err != nil {
		t.Fatal(err)
	}
	if f.txs != txs+1 || len(f.revoked) != 1 || f.revoked[0] != r.Room || len(f.archived) != 1 || f.archived[0] != r.MeetingID {
		t.Errorf("its links and meeting: %d, %v, %v", f.txs-txs, f.revoked, f.archived)
	}
	if len(f.removed) != 1 || f.removed[0] != r.Room {
		t.Errorf("its guests: %v", f.removed)
	}
	if mine, _ := Mine(ctx, maya); mine != nil {
		t.Errorf("a closed room is still theirs: %+v", mine)
	}
	if again, err := Open(ctx, maya); err != nil || again.MeetingID == r.MeetingID {
		t.Errorf("opened after closing: %+v, %v", again, err)
	}
}

// What a room's address answers: live, locked until its passcode's unlock
// opens it, and for anything else (a page's address, off, gone, never a
// room) what every address answers; a failed read is never a state.
func TestARoomsAddressAnswersByTheOneRule(t *testing.T) {
	withRooms(t)
	ctx := context.Background()
	slug, hash := "maya-chen", "hashed"
	room := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, Capability: guestModel.CapabilityJoin, Slug: &slug}
	locked := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, Capability: guestModel.CapabilityJoin, Slug: &slug, PasswordHash: &hash}
	page := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, Slug: &slug}
	unlocks = func(token string, g *guestModel.GuestGrant, _ time.Time) bool { return token == "opens-"+g.Id.String() }
	failed := errors.New("the database is away")

	for _, c := range []struct {
		name      string
		g         *guestModel.GuestGrant
		err       error
		unlock    string
		state     string
		enterErr  error
		readError bool
	}{
		{name: "a live room", g: room, state: guestBusiness.StateLive},
		{name: "a room with a passcode", g: locked, state: guestBusiness.StateLocked, enterErr: ErrLocked},
		{name: "another room's unlock", g: locked, unlock: "opens-" + room.Id.String(), state: guestBusiness.StateLocked, enterErr: ErrLocked},
		{name: "unlocked", g: locked, unlock: "opens-" + locked.Id.String(), state: guestBusiness.StateLive},
		{name: "a page's address", g: page, state: guestBusiness.StateMissing, enterErr: guestBusiness.ErrInvalidGrant},
		{name: "rooms off", err: guestBusiness.ErrGuestDisabled, state: guestBusiness.StateMissing, enterErr: guestBusiness.ErrInvalidGrant},
		{name: "never a room", err: guestBusiness.ErrInvalidGrant, state: guestBusiness.StateMissing, enterErr: guestBusiness.ErrInvalidGrant},
		{name: "gone", err: guestBusiness.ErrGone, state: guestBusiness.StateGone, enterErr: guestBusiness.ErrInvalidGrant},
		{name: "a failed read", err: failed, readError: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resolve = func(_ context.Context, ref guestBusiness.External) (*guestModel.GuestGrant, error) {
				if ref.Slug != slug || ref.Token != "" {
					t.Fatalf("resolved %+v", ref)
				}
				return c.g, c.err
			}
			state, g, err := At(ctx, slug, c.unlock)
			if c.readError {
				if err == nil || state != "" {
					t.Fatalf("a failed read said %q", state)
				}
				if _, err := Enter(ctx, slug, c.unlock); !errors.Is(err, failed) {
					t.Errorf("entering through a failed read: %v", err)
				}
				return
			}
			if err != nil || state != c.state || (state == guestBusiness.StateLive) != (g != nil) {
				t.Errorf("at the address: %q %+v %v, want %q", state, g, err, c.state)
			}
			got, err := Enter(ctx, slug, c.unlock)
			if !errors.Is(err, c.enterErr) || (c.enterErr == nil && got != c.g) {
				t.Errorf("entering: %+v %v, want %v", got, err, c.enterErr)
			}
		})
	}
}

// A passcode opens its own room only, within the address's tries.
func TestUnlockingARoom(t *testing.T) {
	withRooms(t)
	ctx := context.Background()
	hash, err := bcrypt.GenerateFromPassword([]byte("4321"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	slug, h := "maya-chen", string(hash)
	room := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, Capability: guestModel.CapabilityJoin, Slug: &slug, PasswordHash: &h}
	resolve = func(context.Context, guestBusiness.External) (*guestModel.GuestGrant, error) { return room, nil }
	tries := 0
	tryAllowed = func(_ context.Context, ip string, g *guestModel.GuestGrant) bool {
		tries++
		return ip == helpers.RateKeyIP("203.0.113.9") && g == room && tries <= 2
	}
	mintUnlock = func(g *guestModel.GuestGrant, _ time.Time) (string, time.Duration) {
		return "opens-" + g.Id.String(), 12 * time.Hour
	}

	if _, _, err := Unlock(ctx, slug, "1234", "203.0.113.9"); !errors.Is(err, ErrWrongPasscode) {
		t.Errorf("a wrong passcode: %v", err)
	}
	token, lasts, err := Unlock(ctx, slug, "4321", "203.0.113.9")
	if err != nil || token != "opens-"+room.Id.String() || lasts != 12*time.Hour {
		t.Errorf("the right passcode: %q %v %v", token, lasts, err)
	}
	if _, _, err := Unlock(ctx, slug, "4321", "203.0.113.9"); !errors.Is(err, ErrBusy) {
		t.Errorf("past the address's tries: %v", err)
	}

	tryAllowed = func(context.Context, string, *guestModel.GuestGrant) bool { return true }
	pageSlug := "launch-notes"
	resolve = func(context.Context, guestBusiness.External) (*guestModel.GuestGrant, error) {
		return &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, Slug: &pageSlug, PasswordHash: &h}, nil
	}
	if _, _, err := Unlock(ctx, pageSlug, "4321", "203.0.113.9"); !errors.Is(err, guestBusiness.ErrInvalidGrant) {
		t.Errorf("a page's password at a room's address: %v", err)
	}
	resolve = func(context.Context, guestBusiness.External) (*guestModel.GuestGrant, error) { return room, nil }
	mintUnlock = func(*guestModel.GuestGrant, time.Time) (string, time.Duration) { return "", 0 }
	if _, _, err := Unlock(ctx, slug, "4321", "203.0.113.9"); err == nil {
		t.Error("an unlock with no signing secret")
	}
}
