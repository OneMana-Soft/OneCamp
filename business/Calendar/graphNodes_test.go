package Calendar

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	planDoc  = "11111111-1111-4111-8111-111111111111"
	launch   = "33333333-3333-4333-8333-333333333333"
	roadmapP = "44444444-4444-4444-8444-444444444444"
)

func docChip(kind, id string) string {
	return `<p>See <span data-type="entityMention" data-id="` + kind + `:` + id + `"></span></p>`
}

// meeting is an event by `by` on calendar cal (onCalendar's), then tweaked
// and given member guests in the store.
func (h *harness) meeting(by *model.UserInfo, cal uuid.UUID, tweak func(*models.Event), guests ...*model.UserInfo) *models.Event {
	h.t.Helper()
	e := h.onCalendar(cal, by, "Weekly sync", h.clock.Add(time.Hour), "")
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	stored := h.store.events[e.ID]
	if tweak != nil {
		tweak(stored)
	}
	for _, g := range guests {
		id := personOf(g)
		h.store.guests[e.ID] = append(h.store.guests[e.ID], models.Attendee{EventID: e.ID, UserID: &id})
	}
	return copyEvent(stored)
}

func (h *harness) nodes(ids ...uuid.UUID) map[string]workGraph.Projection {
	h.t.Helper()
	asked := make([]string, len(ids))
	for i, id := range ids {
		asked[i] = id.String()
	}
	got, err := h.s.eventNodes(context.Background(), asked)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

// TestAMeetingsNodeIsItsPeopleItsCalendarAndItsLinks: a meeting's node is
// organised by its organiser and with its member guests (not a guest by
// email, one who declined, or the organiser twice), on its project's
// calendar, and links what its notes link; it changed when the event did.
func TestAMeetingsNodeIsItsPeopleItsCalendarAndItsLinks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	cal, err := h.s.projectCalendar(ctx, roadmapP, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	e := h.meeting(sam, cal, func(e *models.Event) {
		e.Notes, e.NotesFormat = docChip("doc", planDoc)+docChip("task", launch), notesHTML
	}, maya, sam)
	declined := personOf(jonas)
	h.store.mu.Lock()
	h.store.guests[e.ID] = append(h.store.guests[e.ID], models.Attendee{EventID: e.ID, Email: "client@example.com"},
		models.Attendee{EventID: e.ID, UserID: &declined, Response: string(Declined)})
	h.store.mu.Unlock()

	p, ok := h.nodes(e.ID)[e.ID.String()]
	if !ok {
		t.Fatal("a meeting has no node")
	}
	person := func(u *model.UserInfo) workGraph.Ref {
		return workGraph.Ref{Kind: workGraph.KindPerson, UUID: personOf(u).String()}
	}
	want := map[string][]workGraph.Ref{
		"cal_event_organiser": {person(sam)},
		"cal_event_with":      {person(maya)},
		"cal_event_on":        {{Kind: workGraph.KindProject, UUID: roadmapP}},
		workGraph.RefersTo:    {{Kind: workGraph.KindDoc, UUID: planDoc}, {Kind: workGraph.KindTask, UUID: launch}},
	}
	for pred, refs := range want {
		got := slices.Clone(p.Edges[pred])
		slices.SortFunc(got, func(a, b workGraph.Ref) int { return strings.Compare(string(a.Kind)+a.UUID, string(b.Kind)+b.UUID) })
		if !slices.Equal(got, refs) {
			t.Errorf("%s: %v, want %v", pred, got, refs)
		}
	}
	if !p.Changed.Equal(e.UpdatedAt) || !p.Deleted.IsZero() {
		t.Errorf("times %+v", p)
	}
}

// TestOnlyAMeetingHasANode: an event connects something beyond its
// organiser or has no node; an instance changed on its own, an event read
// from an address, and focus, away and working-location time have none
// whatever they hold; a meeting in the trash or cancelled keeps a node
// marked gone.
func TestOnlyAMeetingHasANode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya := h.member("Sam"), h.member("Maya")
	own := uuid.New()
	cal, err := h.s.projectCalendar(ctx, roadmapP, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	links := func(e *models.Event) { e.Notes = "Agenda: " + docChip("doc", planDoc) }
	cases := map[string]struct {
		e    *models.Event
		node bool
	}{
		"alone":                  {h.meeting(sam, own, nil), false},
		"with someone":           {h.meeting(sam, own, nil, maya), true},
		"on a project":           {h.meeting(sam, cal, nil), true},
		"linking a doc":          {h.meeting(sam, own, links), true},
		"focus":                  {h.meeting(sam, own, func(e *models.Event) { e.Kind = string(EventFocus) }, maya), false},
		"away":                   {h.meeting(sam, own, func(e *models.Event) { e.Kind = string(EventAway) }, maya), false},
		"working somewhere":      {h.meeting(sam, own, func(e *models.Event) { e.Kind = string(EventWorkingLocation) }, maya), false},
		"a reminder":             {h.meeting(sam, own, func(e *models.Event) { e.Kind = string(EventReminder) }, maya), false},
		"a task block":           {h.meeting(sam, cal, func(e *models.Event) { e.Kind = string(EventTaskBlock) }), false},
		"read from an address":   {h.meeting(sam, own, func(e *models.Event) { e.Source = sourceSubscribed }, maya), false},
		"an instance on its own": {h.meeting(sam, own, func(e *models.Event) { s := uuid.New(); e.SeriesID = &s }, maya), false},
	}
	var ids []uuid.UUID
	for _, c := range cases {
		ids = append(ids, c.e.ID)
	}
	got := h.nodes(ids...)
	for name, c := range cases {
		if _, ok := got[c.e.ID.String()]; ok != c.node {
			t.Errorf("%s: node %v, want %v", name, ok, c.node)
		}
	}

	trashed := h.meeting(sam, own, func(e *models.Event) { at := h.clock.Add(-time.Hour); e.DeletedAt = &at }, maya)
	cancelled := h.meeting(sam, own, func(e *models.Event) { e.Status = statusCancelled }, maya)
	got = h.nodes(trashed.ID, cancelled.ID)
	if p := got[trashed.ID.String()]; !p.Deleted.Equal(*trashed.DeletedAt) {
		t.Errorf("a trashed meeting's node: %+v", p)
	}
	if p := got[cancelled.ID.String()]; !p.Deleted.Equal(cancelled.UpdatedAt) {
		t.Errorf("a cancelled meeting's node: %+v", p)
	}
}

// TestAnAllHandsNodeIsWithItsFirst200: a meeting's node names at most
// maxWith people it's with, the first invited, and never its organiser.
func TestAnAllHandsNodeIsWithItsFirst200(t *testing.T) {
	organiser := uuid.New()
	e := &models.Event{ID: uuid.New(), CreatedBy: organiser, UpdatedAt: time.Now()}
	guests := []uuid.UUID{organiser}
	for range maxWith + 50 {
		guests = append(guests, uuid.New())
	}
	p, ok := meetingNode(e, guests, nil)
	if !ok || len(p.Edges["cal_event_with"]) != maxWith || p.Edges["cal_event_with"][0].UUID != guests[1].String() {
		t.Fatalf("an all-hands: %d with, node %v", len(p.Edges["cal_event_with"]), ok)
	}
}

// TestTheNightlyPassPagesTheEventsThatMayBeMeetings: pages by id from the
// cursor, leaving out what can never be a meeting, with the cursor after a
// full page and "" at the end.
func TestTheNightlyPassPagesTheEventsThatMayBeMeetings(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	own := uuid.New()
	var want []string
	for i := range 5 {
		want = append(want, h.meeting(sam, own, nil).ID.String())
		h.meeting(sam, own, func(e *models.Event) { e.Kind = string(EventFocus) })
		if i == 0 {
			h.meeting(sam, own, func(e *models.Event) { e.Source = sourceSubscribed })
		}
	}
	slices.Sort(want)
	ctx := context.Background()
	first, next, err := h.s.eventNodesAfter(ctx, "", 3)
	if err != nil || !slices.Equal(first, want[:3]) || next != want[2] {
		t.Fatalf("first page %v next %q (%v)", first, next, err)
	}
	rest, next, err := h.s.eventNodesAfter(ctx, next, 3)
	if err != nil || !slices.Equal(rest, want[3:]) || next != "" {
		t.Fatalf("last page %v next %q (%v)", rest, next, err)
	}
}

// TestAPrivateEventOpensThroughItsCalendarOnlyToThoseWhoMayChangeIt: as
// Google does it, a private event shows as busy to everyone on its calendar
// but those who may change the calendar's events, so through the calendar
// only they may open it; its guests always may. An event that isn't private
// opens to anyone who sees the calendar's details.
func TestAPrivateEventOpensThroughItsCalendarOnlyToThoseWhoMayChangeIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	jonas, guest := h.member("Jonas"), h.member("Guest")
	shared := map[string]*model.UserInfo{}
	for _, level := range []string{models.ShareManage, models.ShareEdit, models.ShareDetails} {
		shared[level] = h.member("Reader " + level)
	}
	busyOnly := h.member("Teammate")
	cal, err := h.s.PersonCalendar(ctx, jonas, personOf(jonas).String())
	if err != nil {
		t.Fatal(err)
	}
	for level, who := range shared {
		if err := h.s.Share(ctx, jonas, cal.String(), personOf(who).String(), level); err != nil {
			t.Fatal(err)
		}
	}
	mk := func(visibility string) string {
		start, end := timed(h.clock.Add(time.Hour), 60, "UTC")
		e, err := h.s.Create(ctx, jonas, EventInput{Title: "Doctor", Start: start, End: end, Visibility: visibility,
			Attendees: []GuestInput{{UUID: guest.UserDgraphInfo.Uuid}}})
		if err != nil {
			t.Fatal(err)
		}
		return e.ID
	}
	private, open := mk(visibilityPrivate), mk(visibilityDefault)
	level := func(who *model.UserInfo, id string) accessBusiness.Level {
		t.Helper()
		d, err := h.s.decideMany(ctx, accessBusiness.Subject{UserUUID: who.UserDgraphInfo.Uuid}, []string{id})
		if err != nil {
			t.Fatal(err)
		}
		return d[id].Level
	}
	for name, c := range map[string]struct {
		who      *model.UserInfo
		id       string
		atLeast  accessBusiness.Level
		seesNone bool
	}{
		"private, its manager":   {who: shared[models.ShareManage], id: private, atLeast: accessBusiness.LevelView},
		"private, its editor":    {who: shared[models.ShareEdit], id: private, atLeast: accessBusiness.LevelView},
		"private, details":       {who: shared[models.ShareDetails], id: private, seesNone: true},
		"private, busy only":     {who: busyOnly, id: private, seesNone: true},
		"private, its guest":     {who: guest, id: private, atLeast: accessBusiness.LevelView},
		"private, its organiser": {who: jonas, id: private, atLeast: accessBusiness.LevelFull},
		"not private, details":   {who: shared[models.ShareDetails], id: open, atLeast: accessBusiness.LevelView},
		"not private, busy only": {who: busyOnly, id: open, seesNone: true},
	} {
		got := level(c.who, c.id)
		if c.seesNone && got != accessBusiness.LevelNone || !c.seesNone && got < c.atLeast {
			t.Errorf("%s: %v", name, got)
		}
	}

	// A project's calendar: its editors may change its events, its viewers see them.
	editor, viewer, owner := h.member("Editor"), h.member("Viewer"), h.member("Owner")
	projectsDecide(t, map[string]map[string]accessBusiness.Level{
		editor.UserDgraphInfo.Uuid: {roadmapP: accessBusiness.LevelEdit},
		viewer.UserDgraphInfo.Uuid: {roadmapP: accessBusiness.LevelView},
	}, nil)
	pcal, err := h.s.projectCalendar(ctx, roadmapP, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	onProject := func(visibility string) string {
		return h.meeting(owner, pcal, func(e *models.Event) { e.Visibility = visibility }).ID.String()
	}
	privateP, openP := onProject(visibilityPrivate), onProject(visibilityDefault)
	if got := level(editor, privateP); got != accessBusiness.LevelView {
		t.Errorf("a project's editor on its private event: %v", got)
	}
	if got := level(viewer, privateP); got != accessBusiness.LevelNone {
		t.Errorf("a project's viewer on its private event: %v", got)
	}
	if got := level(viewer, openP); got != accessBusiness.LevelView {
		t.Errorf("a project's viewer on its event: %v", got)
	}
}

// TestAnEventsChipSaysWhatAndWhenToThoseWhoMayOpenIt: a guest reads its
// title and start (an instance's own start for an instance), a stranger
// reads nothing, and once it's in the trash a guest reads it gone.
func TestAnEventsChipSaysWhatAndWhenToThoseWhoMayOpenIt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, jonas := h.member("Sam"), h.member("Maya"), h.member("Jonas")
	e := h.create(sam, "Board", maya)
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	weekly := h.series(sam, "Weekly sync", "FREQ=WEEKLY", start, 30)
	instance := fmt.Sprintf("%s_%s", weekly.ID, start.AddDate(0, 0, 7).Format(ridTimed))
	read := func(who *model.UserInfo, ids ...string) map[string]entityLinkBusiness.Entity {
		t.Helper()
		got, err := h.s.eventChips(ctx, accessBusiness.Subject{UserUUID: who.UserDgraphInfo.Uuid}, ids)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := read(maya, e.ID)
	if c := got[e.ID]; c.Access != entityLinkBusiness.AccessOK || c.Title != "Board" || c.Data["start"].(When).At == nil || c.Data["repeats"] != false {
		t.Errorf("Maya's chip %+v", c)
	}
	if c := read(sam, instance)[instance]; c.Access != entityLinkBusiness.AccessOK || c.Data["repeats"] != true || !c.Data["start"].(When).At.Equal(start.AddDate(0, 0, 7)) {
		t.Errorf("an instance's chip %+v", c)
	}
	if got := read(jonas, e.ID); len(got) != 0 {
		t.Errorf("a stranger read %+v", got)
	}
	if err := h.s.Delete(ctx, sam, e.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	if c := read(maya, e.ID)[e.ID]; c.Access != entityLinkBusiness.AccessGone || c.Title != "" {
		t.Errorf("a trashed event's chip %+v", c)
	}
}

// TestAnImportTellsTheWorkGraphOfItsEvents: an import announces nothing, so
// it tells the work graph of the events it made, once for the file; an
// import that makes nothing tells it of nothing.
func TestAnImportTellsTheWorkGraphOfItsEvents(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	file := googleFile("jonas@example.com")
	made, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {})
	if err != nil || len(made.Created) != 2 {
		t.Fatalf("import %+v %v", made, err)
	}
	if want := []string{made.Created[0].UUID, made.Created[1].UUID}; !slices.Equal(h.graphTold, want) {
		t.Errorf("the graph was told of %v, want %v", h.graphTold, want)
	}
	if _, err := h.s.importCalendars(context.Background(), sam, readFile(t, file), func(int, int) {}); err != nil || len(h.graphTold) != 2 {
		t.Errorf("an import that made nothing told the graph of %v (%v)", h.graphTold[2:], err)
	}
}
