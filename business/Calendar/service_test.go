package Calendar

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	outboxBusiness "github.com/akashc777/OneCamp/business/Outbox"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// recordedEffects records what a write would have done beyond the store.
type recordedEffects struct {
	mu                           sync.Mutex
	nCreated, nUpdated, nDeleted int
	nLeft                        int
	synced, guestsSent           bool
	removed                      []string
	bookingAfterStart            *time.Time
}

func (r *recordedEffects) created(_ context.Context, _ *model.UserInfo, _ *models.Event, _ []string, sync bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nCreated++
	r.synced = sync
}

func (r *recordedEffects) updated(_ context.Context, _ *model.UserInfo, _, after *models.Event, _, _ []models.Attendee, guestsSent bool, removed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nUpdated++
	r.guestsSent = guestsSent
	r.removed = append(r.removed, removed...)
	start, _ := spanOf(after)
	r.bookingAfterStart = &start
}

func (r *recordedEffects) deleted(context.Context, *model.UserInfo, *models.Event, []models.Attendee) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nDeleted++
}

func (r *recordedEffects) left(context.Context, *model.UserInfo, *models.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nLeft++
}

// recordedTells records who a write would have told, and of what.
type recordedTells struct {
	mu    sync.Mutex
	tells []telling
}

func (r *recordedTells) tell(_ context.Context, _ *model.UserInfo, t telling) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tells = append(r.tells, t)
}

// of is the tellings of one kind.
func (r *recordedTells) of(what tellWhat) []telling {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []telling
	for _, t := range r.tells {
		if t.what == what {
			out = append(out, t)
		}
	}
	return out
}

// harness is the service over a store in memory, with a clock, people and
// announcements of its own.
type harness struct {
	t       *testing.T
	s       *service
	store   *memStore
	effects *recordedEffects
	tells   *recordedTells
	clock   time.Time
	people  map[string]*dgraphStruct.DgraphUser
	active  map[string]bool
	mu      sync.Mutex
	emitted []resourceEventBusiness.Event
	// graphTold is the events an import or Google's changes told the work
	// graph of (graphNodes.go), in order; searchTold those search was asked
	// to write the entries of (searchEntries.go).
	graphTold  []string
	searchTold []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// Guests by email have their links sealed with the server's key.
	t.Setenv("APP_SECRET_KEK", "calendar-unit-tests")
	h := &harness{t: t, store: newMemStore(), effects: &recordedEffects{}, tells: &recordedTells{},
		clock:  time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC),
		people: map[string]*dgraphStruct.DgraphUser{}, active: map[string]bool{}}
	s := newService(h.store)
	s.now = func() time.Time { return h.clock }
	s.people = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, id := range ids {
			if p := h.people[id]; p != nil {
				out = append(out, p)
			}
		}
		return out, nil
	}
	s.members = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, id := range ids {
			if h.active[id] {
				out = append(out, h.people[id])
			}
		}
		return out, nil
	}
	s.emit = func(_ context.Context, e resourceEventBusiness.Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.emitted = append(h.emitted, e)
	}
	s.effects = h.effects
	s.tells = h.tells
	s.ready = func(context.Context) error { return nil }
	// Reminders in memory too (reminders_test.go).
	s.rem = newMemReminders()
	h.s = s
	prev, prevNodes := upcomingBookings, nodesChanged
	upcomingBookings = func(uuid.UUID, time.Time) ([]bookingModel.BookedEvent, error) { return nil, nil }
	nodesChanged = func(_ context.Context, k workGraph.Kind, ids []string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if k == workGraph.KindEvent {
			h.graphTold = append(h.graphTold, ids...)
		}
	}
	prevSearch := enqueueSearch
	enqueueSearch = func(_ context.Context, es ...outboxBusiness.Entry) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, e := range es {
			h.searchTold = append(h.searchTold, strings.Split(e.Entities["ids"], ",")...)
		}
		return nil
	}
	t.Cleanup(func() { upcomingBookings, nodesChanged, enqueueSearch = prev, prevNodes, prevSearch })
	return h
}

// member is someone in the workspace, by name.
func (h *harness) member(name string) *model.UserInfo {
	id := uuid.New()
	h.people[id.String()] = &dgraphStruct.DgraphUser{Uid: "0x" + id.String()[:6], Uuid: id.String(), UserName: name, UserFullName: name + " Full"}
	h.active[id.String()] = true
	return &model.UserInfo{UserPostgresInfo: model.User{Id: id}, UserDgraphInfo: dgraphStruct.DgraphUser{Uuid: id.String(), UserName: name}}
}

func (h *harness) zone(who *model.UserInfo, quietHours string) {
	h.store.hints[personOf(who)] = models.ZoneHints{QuietHours: quietHours}
}

func timed(start time.Time, minutes int, zone string) (When, When) {
	end := start.Add(time.Duration(minutes) * time.Minute)
	return When{At: &start, Zone: zone}, When{At: &end}
}

func (h *harness) create(who *model.UserInfo, title string, guests ...*model.UserInfo) *Event {
	h.t.Helper()
	start, end := timed(time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC), 15, "Europe/London")
	in := EventInput{Title: title, Notes: "Draft terms", Start: start, End: end}
	for _, g := range guests {
		in.Attendees = append(in.Attendees, GuestInput{UUID: g.UserDgraphInfo.Uuid})
	}
	e, err := h.s.Create(context.Background(), who, in)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

func guestNames(e *Event) string {
	var out []string
	for _, a := range e.Attendees {
		out = append(out, a.Name)
	}
	return strings.Join(out, ",")
}

func seq(n int) *int       { return &n }
func str(s string) *string { return &s }

// CAL5 as A1 keeps it: the organiser and the guests see an event; anyone
// else, a workspace admin included, is told it isn't there, as for no event.
func TestAnEventIsSeenByItsPeopleOnly(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	admin := h.member("Admin")
	admin.UserPostgresInfo.IsAdmin = true
	e := h.create(sam, "Acquisition talks", maya)
	for _, who := range []*model.UserInfo{sam, maya} {
		got, err := h.s.Get(ctx, who, e.ID)
		if err != nil || got.Title != "Acquisition talks" || got.Organiser.Name != "Sam" || guestNames(got) != "Maya" {
			t.Errorf("%s: %+v %v", who.UserDgraphInfo.UserName, got, err)
		}
	}
	for _, who := range []*model.UserInfo{jonas, admin} {
		if _, err := h.s.Get(ctx, who, e.ID); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("%s: %v", who.UserDgraphInfo.UserName, err)
		}
	}
	if _, err := h.s.Get(ctx, sam, uuid.NewString()); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("no event: %v", err)
	}
	var input *EventInputError
	if _, err := h.s.Get(ctx, sam, "not an id"); !errors.As(err, &input) {
		t.Errorf("not an id: %v", err)
	}
}

// CAL2: a write that names an older sequence is refused with the event as it
// is now, and nothing is written; the current one goes through.
func TestAStaleWriteIsRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "Launch review")
	if e.Sequence != 0 {
		t.Fatalf("a new event's sequence is %d", e.Sequence)
	}
	got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Title: str("Launch review, final")})
	if err != nil || got.Sequence != 1 {
		t.Fatalf("first write: %+v %v", got, err)
	}
	writes := h.store.writes
	_, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Title: str("Overwritten")})
	var stale *StaleError
	if !errors.As(err, &stale) || stale.Current.Title != "Launch review, final" || stale.Current.Sequence != 1 {
		t.Fatalf("stale write: %v", err)
	}
	if h.store.writes != writes {
		t.Error("a stale write was written")
	}
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Title: str("No sequence")}); err == nil {
		t.Error("a write naming no sequence went through")
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", seq(0)); !errors.As(err, &stale) {
		t.Errorf("a stale delete: %v", err)
	}
}

// CAL2: a field left out stays as it is, a field sent is set ("" clears it),
// and a guest list sent replaces the list, keeping the answers of the guests
// it keeps.
func TestAWriteChangesWhatItSendsAndOnlyThat(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas, ana := h.member("Sam"), h.member("Maya"), h.member("Jonas"), h.member("Ana")
	e := h.create(sam, "Copy sync", maya, jonas)
	jonasID := personOf(jonas)
	for i, a := range h.store.guests[uuid.MustParse(e.ID)] {
		if *a.UserID == jonasID {
			h.store.guests[uuid.MustParse(e.ID)][i].Response = responseAccepted
		}
	}

	got, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Location: str("Room 2")})
	if err != nil || got.Notes != "Draft terms" || got.Title != "Copy sync" || guestNames(got) != "Maya,Jonas" || got.Location != "Room 2" {
		t.Fatalf("only the location: %+v %v", got, err)
	}
	got, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(1), Notes: str("")})
	if err != nil || got.Notes != "" {
		t.Fatalf("notes cleared: %+v %v", got, err)
	}
	list := []GuestInput{{UUID: jonas.UserDgraphInfo.Uuid}, {UUID: ana.UserDgraphInfo.Uuid, Role: roleOptional}}
	got, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(2), Attendees: &list})
	if err != nil || guestNames(got) != "Jonas,Ana" {
		t.Fatalf("guests replaced: %+v %v", got, err)
	}
	for _, a := range got.Attendees {
		if a.Name == "Jonas" && a.Response != responseAccepted {
			t.Errorf("Jonas's answer was lost: %+v", a)
		}
		if a.Name == "Ana" && (a.Response != responseNeedsAction || a.Role != roleOptional) {
			t.Errorf("Ana: %+v", a)
		}
	}
	if len(h.effects.removed) != 1 || h.effects.removed[0] != maya.UserDgraphInfo.Uuid || !h.effects.guestsSent {
		t.Errorf("Maya, taken off, is told: %+v", h.effects)
	}
	none := []GuestInput{}
	got, err = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(3), Attendees: &none})
	if err != nil || len(got.Attendees) != 0 {
		t.Fatalf("an empty list takes everyone off: %+v %v", got, err)
	}
}

// Someone who can't be invited (not an active member, or with no account) isn't
// added; nobody is their own guest, and nobody is added twice.
func TestOnlyActiveMembersAreInvitedOnce(t *testing.T) {
	h := newHarness(t)
	sam, maya, gone, ghost := h.member("Sam"), h.member("Maya"), h.member("Gone"), h.member("Ghost")
	h.active[gone.UserDgraphInfo.Uuid] = false
	h.store.noAccount[personOf(ghost)] = true
	start, end := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 30, "UTC")
	e, err := h.s.Create(context.Background(), sam, EventInput{Title: "Board", Start: start, End: end, Attendees: []GuestInput{
		{UUID: maya.UserDgraphInfo.Uuid}, {UUID: gone.UserDgraphInfo.Uuid}, {UUID: sam.UserDgraphInfo.Uuid},
		{UUID: maya.UserDgraphInfo.Uuid}, {UUID: "not a person"}, {UUID: ghost.UserDgraphInfo.Uuid}}})
	if err != nil || guestNames(e) != "Maya" {
		t.Fatalf("%+v %v", e, err)
	}
}

// A guest changes an event only when its organiser lets guests change it,
// and never its guest permissions.
func TestGuestsChangeAnEventOnlyWhenLetTo(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Design review", maya)
	if _, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(0), Title: str("Mine")}); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Fatalf("a guest changed it: %v", err)
	}
	yes := true
	if _, err := h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), GuestsCanModify: &yes}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(1), Title: str("Design review, round 2")}); err != nil || got.Title != "Design review, round 2" {
		t.Fatalf("let to: %+v %v", got, err)
	}
	no := false
	if _, err := h.s.Update(ctx, maya, e.ID, EventPatch{Sequence: seq(2), GuestsCanSeeGuests: &no}); !errors.Is(err, ErrOnlyCreatorChanges) {
		t.Errorf("a guest changed the guest permissions: %v", err)
	}
}

// With the guest list hidden, a guest sees only themselves; the organiser
// sees everyone.
func TestAHiddenGuestListShowsAGuestOnlyThemselves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	start, end := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 30, "UTC")
	no := false
	e, err := h.s.Create(ctx, sam, EventInput{Title: "Offsite", Start: start, End: end, GuestsCanSeeGuests: &no,
		Attendees: []GuestInput{{UUID: maya.UserDgraphInfo.Uuid}, {UUID: jonas.UserDgraphInfo.Uuid}}})
	if err != nil || guestNames(e) != "Maya,Jonas" {
		t.Fatalf("%+v %v", e, err)
	}
	got, _ := h.s.Get(ctx, maya, e.ID)
	if guestNames(got) != "Maya" {
		t.Errorf("Maya sees %q", guestNames(got))
	}
	list, _ := h.s.legacyRange(ctx, maya, start.At.Add(-time.Hour), end.At.Add(time.Hour))
	if len(list) != 1 || len(list[0].Participants) != 1 || list[0].Participants[0].UserName != "Maya" {
		t.Errorf("the old list shows Maya %+v", list)
	}
}

// Deleting puts an event in its organiser's trash: gone for everyone,
// listed in the trash, and back with Restore. Only the organiser deletes or
// restores it; nothing is purged.
func TestDeletingPutsAnEventInTheTrash(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Stand-up", maya)
	if err := h.s.Delete(ctx, maya, e.ID, "", nil); !errors.Is(err, ErrNotEventCreator) {
		t.Errorf("a guest deleted it: %v", err)
	}
	if err := h.s.Delete(ctx, jonas, e.ID, "", nil); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("someone not on it: %v", err)
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", seq(0)); err != nil {
		t.Fatal(err)
	}
	for _, who := range []*model.UserInfo{sam, maya} {
		if _, err := h.s.Get(ctx, who, e.ID); !errors.Is(err, ErrEventNotFound) {
			t.Errorf("deleted, %s: %v", who.UserDgraphInfo.UserName, err)
		}
	}
	trash, err := h.s.Trash(ctx, sam, nil)
	if err != nil || len(trash) != 1 || trash[0].ID != e.ID || trash[0].DeletedAt == nil {
		t.Fatalf("the trash: %+v %v", trash, err)
	}
	if mine, _ := h.s.Trash(ctx, maya, nil); len(mine) != 0 {
		t.Error("a guest's trash holds the organiser's event")
	}
	if _, err := h.s.Restore(ctx, maya, e.ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("a guest restored it: %v", err)
	}
	back, err := h.s.Restore(ctx, sam, e.ID)
	if err != nil || back.DeletedAt != nil || guestNames(back) != "Maya" {
		t.Fatalf("restored: %+v %v", back, err)
	}
	if _, err := h.s.Get(ctx, maya, e.ID); err != nil {
		t.Errorf("Maya, after the restore: %v", err)
	}
	if h.effects.nDeleted != 1 {
		t.Errorf("deleted effects %d", h.effects.nDeleted)
	}
}

// A guest leaves an event, which then is no longer theirs; the organiser
// can't leave their own. Saying no is an answer, and keeps them on it.
func TestAGuestLeavesAndTheOrganiserCant(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Pricing sync", maya)
	if err := h.s.legacyLeave(ctx, uuid.MustParse(e.ID), sam); !errors.Is(err, ErrCreatorCannotLeave) {
		t.Errorf("the organiser: %v", err)
	}
	if err := h.s.legacyLeave(ctx, uuid.MustParse(e.ID), maya); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Get(ctx, maya, e.ID); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("Maya still reads it: %v", err)
	}
	if got, _ := h.s.Get(ctx, sam, e.ID); len(got.Attendees) != 0 || got.Sequence != 1 {
		t.Errorf("Sam's event: %+v", got)
	}
	if err := h.s.legacyLeave(ctx, uuid.MustParse(e.ID), maya); !errors.Is(err, ErrEventNotFound) {
		t.Errorf("leaving twice: %v", err)
	}
	if h.effects.nLeft != 1 {
		t.Errorf("left %d", h.effects.nLeft)
	}
}

// CAL1: every write is announced once, as the kind of change it was, naming
// what changed and never what the event says.
func TestEveryWriteIsAnnouncedOnceWithoutItsWords(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	e := h.create(sam, "Acquisition talks", maya)
	_, _ = h.s.Update(ctx, sam, e.ID, EventPatch{Sequence: seq(0), Title: str("Acquisition talks II")})
	_ = h.s.Delete(ctx, sam, e.ID, "", nil)
	_, _ = h.s.Restore(ctx, sam, e.ID)
	_ = h.s.Respond(ctx, maya, e.ID, Declined)
	want := []resourceEventBusiness.Kind{resourceEventBusiness.CalendarEventCreated, resourceEventBusiness.CalendarEventUpdated,
		resourceEventBusiness.CalendarEventDeleted, resourceEventBusiness.CalendarEventRestored, resourceEventBusiness.CalendarEventRSVP}
	if len(h.emitted) != len(want) {
		t.Fatalf("%d announcements, want %d: %+v", len(h.emitted), len(want), h.emitted)
	}
	for i, got := range h.emitted {
		if got.Kind != want[i] || got.ResourceKind != KindEvent || got.ResourceUUID != e.ID || got.Actor.UserUUID == "" {
			t.Errorf("%d: %+v", i, got)
		}
		for k, v := range got.Data {
			if strings.Contains(v, "Acquisition") || strings.Contains(v, "Draft terms") {
				t.Errorf("%s says what the event says: %s=%s", got.Kind, k, v)
			}
		}
	}
	if h.emitted[1].Data["changed"] != "title" {
		t.Errorf("the update names %q", h.emitted[1].Data["changed"])
	}
	if h.emitted[0].Subjects[0] != maya.UserDgraphInfo.Uuid {
		t.Errorf("the invitation is about Maya: %v", h.emitted[0].Subjects)
	}
}

// Notes in HTML are kept in the codec's one form: what a doc may hold, so a
// script or a handler never reaches the store.
func TestHTMLNotesAreCanonicalAndSafe(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	start, end := timed(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), 30, "UTC")
	e, err := h.s.Create(context.Background(), sam, EventInput{Title: "Agenda", NotesFormat: notesHTML,
		Notes: `<p onclick="steal()">Bring <b>numbers</b><script>alert(1)</script></p><iframe src="https://evil.example"></iframe>`,
		Start: start, End: end})
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"script", "onclick", "iframe", "alert"} {
		if strings.Contains(e.Notes, bad) {
			t.Errorf("the notes keep %s: %s", bad, e.Notes)
		}
	}
	if !strings.Contains(e.Notes, "numbers") || e.NotesFormat != notesHTML {
		t.Errorf("the notes lost their words: %s", e.Notes)
	}
}

// A read of a moment finds what is on at that moment, and a read finds an
// event whose end or start falls on its edge, as the graph's read did.
func TestAReadCountsItsEdgesIn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam := h.member("Sam")
	e := h.create(sam, "Stand-up") // 09:15 to 09:30
	at := time.Date(2026, 10, 16, 9, 20, 0, 0, time.UTC)
	for name, w := range map[string][2]time.Time{
		"a moment inside":     {at, at},
		"ending at its start": {at.Add(-time.Hour), time.Date(2026, 10, 16, 9, 15, 0, 0, time.UTC)},
		"starting at its end": {time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC), at.Add(time.Hour)},
	} {
		got, _, err := h.s.Range(ctx, sam, w[0], w[1])
		if err != nil || len(got) != 1 || got[0].ID != e.ID {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	if got, _, _ := h.s.Range(ctx, sam, at.Add(time.Hour), at.Add(2*time.Hour)); len(got) != 0 {
		t.Errorf("after it: %+v", got)
	}
}

// One read answers at most maxInstances events, and says there were more.
func TestAReadSaysWhenItWasCut(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= maxInstances; i++ {
		s := start.Add(time.Duration(i) * time.Minute)
		st, en := timed(s, 1, "UTC")
		if _, _, err := h.s.create(context.Background(), sam, eventAt(st, en), nil, false); err != nil {
			t.Fatal(err)
		}
	}
	got, cut, err := h.s.Range(context.Background(), sam, start, start.AddDate(0, 0, 30))
	if err != nil || len(got) != maxInstances || !cut {
		t.Fatalf("%d events, cut %v, %v", len(got), cut, err)
	}
}

func eventAt(start, end When) *models.Event {
	e := blank("UTC")
	_ = setWhen(e, start, end)
	return e
}

// Focus time runs from a time to a time: the push gate reads instants, so a
// focus event of whole days is refused.
func TestFocusTimeIsNeverWholeDays(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	_, err := h.s.Create(context.Background(), sam, EventInput{Kind: EventFocus, Title: "Focus",
		Start: When{Date: "2026-10-16"}, End: When{Date: "2026-10-17"}})
	var input *EventInputError
	if !errors.As(err, &input) {
		t.Errorf("an all-day focus block: %v", err)
	}
}
