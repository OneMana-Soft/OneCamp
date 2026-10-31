package Calendar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
)

// liveGoogle is a Google calendar that changes: its events, a sync token
// that counts changes, and what OneCamp pushed (or tried to).
type liveGoogle struct {
	mu      sync.Mutex
	clock   func() time.Time
	events  map[string]calendarSyncBusiness.Change // by id
	order   []string
	version map[string]int // id → the change it was last touched at
	at      int
	pushes  []calendarSyncBusiness.Op
	next    int
	// shared are calendars shared with the person beside their own, each
	// read whole by the move, unchanged after.
	shared []sharedCalendar
	// refuse is Google's answer to a push, nil when it takes it.
	refuse func(calendarSyncBusiness.Op) error
	// revoked is an account Google no longer answers for.
	revoked bool
}

// sharedCalendar is a calendar shared with the person at Google, and its
// events.
type sharedCalendar struct {
	calendarSyncBusiness.Remote
	events []calendarSyncBusiness.Change
}

func newLiveGoogle(clock func() time.Time) *liveGoogle {
	return &liveGoogle{clock: clock, events: map[string]calendarSyncBusiness.Change{}, version: map[string]int{}}
}

// put is a change made at Google.
func (g *liveGoogle) put(ch calendarSyncBusiness.Change) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.at++
	if _, ok := g.events[ch.ExternalID]; !ok {
		g.order = append(g.order, ch.ExternalID)
	}
	ch.ETag = fmt.Sprintf("v%d", g.at)
	ch.Event.Stamp = g.clock()
	g.events[ch.ExternalID], g.version[ch.ExternalID] = ch, g.at
}

func (*liveGoogle) Provider() string { return "google" }

func (g *liveGoogle) Calendars(_ context.Context, l calendarSyncBusiness.Link) ([]calendarSyncBusiness.Remote, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []calendarSyncBusiness.Remote{{Link: l, ID: "sam@outlook.com", Name: "sam@outlook.com", Primary: true}}
	for _, c := range g.shared {
		r := c.Remote
		r.Link = l
		out = append(out, r)
	}
	return out, nil
}

func (g *liveGoogle) Pull(_ context.Context, cal calendarSyncBusiness.Remote, cursor string) ([]calendarSyncBusiness.Change, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.revoked {
		return nil, "", calendarSyncBusiness.ErrRevoked
	}
	for _, c := range g.shared {
		if c.ID == cal.ID {
			if cursor != "" {
				return nil, cursor, nil
			}
			return c.events, "read", nil
		}
	}
	since := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "t%d", &since)
	}
	var out []calendarSyncBusiness.Change
	for _, id := range g.order {
		if g.version[id] > since {
			out = append(out, g.events[id])
		}
	}
	return out, fmt.Sprintf("t%d", g.at), nil
}

func (g *liveGoogle) Push(_ context.Context, op calendarSyncBusiness.Op) (calendarSyncBusiness.Ref, error) {
	g.mu.Lock()
	g.pushes = append(g.pushes, op)
	revoked, refuse := g.revoked, g.refuse
	g.mu.Unlock()
	if revoked {
		return calendarSyncBusiness.Ref{}, calendarSyncBusiness.ErrRevoked
	}
	if refuse != nil {
		if err := refuse(op); err != nil {
			return calendarSyncBusiness.Ref{}, err
		}
	}
	switch op.Kind {
	case "create":
		g.next++
		id := fmt.Sprintf("made%d", g.next)
		g.put(calendarSyncBusiness.Change{ExternalID: id, Event: op.Event})
		return calendarSyncBusiness.Ref{ExternalID: id, ETag: g.events[id].ETag}, nil
	case "update":
		id := op.ExternalID
		ch := calendarSyncBusiness.Change{ExternalID: id, Event: op.Event}
		if op.Event.RecurrenceID != nil {
			ch.SeriesID, ch.ExternalID = id, id+"_"+op.Event.RecurrenceID.UTC().Format("20060102T150405Z")
		}
		g.put(ch)
		return calendarSyncBusiness.Ref{ExternalID: id, ETag: g.events[ch.ExternalID].ETag}, nil
	case "delete":
		g.put(calendarSyncBusiness.Change{ExternalID: op.ExternalID, Deleted: true})
	}
	return calendarSyncBusiness.Ref{ExternalID: op.ExternalID}, nil
}

func (*liveGoogle) Watch(context.Context, calendarSyncBusiness.Remote) (calendarSyncBusiness.Watch, error) {
	return calendarSyncBusiness.Watch{}, nil
}
func (*liveGoogle) Stop(context.Context, calendarSyncBusiness.Watch) error { return nil }

func (g *liveGoogle) pushed() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, p := range g.pushes {
		out = append(out, p.Kind+" "+p.Event.Summary)
	}
	g.pushes = nil
	return out
}

// syncWithGoogle moves who's calendars in from g, as a finished move leaves
// them, and answers the ledger and a sweep that runs a minute later.
func syncWithGoogle(t *testing.T, h *harness, who *model.UserInfo, g *liveGoogle) (*memLedger, func()) {
	t.Helper()
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger, h.s.moveNow = ledger, true
	prevProvider, prevAccount := syncProvider, googleAccountOf
	syncProvider = func(string) (calendarSyncBusiness.Adapter, bool) { return g, true }
	googleAccountOf = func(context.Context, *model.UserInfo) (string, bool, error) { return "sam@outlook.com", true, nil }
	t.Cleanup(func() { syncProvider, googleAccountOf = prevProvider, prevAccount })
	if st, err := h.s.MoveFromGoogle(context.Background(), who); err != nil || st.State != models.SyncSynced {
		t.Fatalf("move %+v %v", st, err)
	}
	return ledger, func() {
		t.Helper()
		h.clock = h.clock.Add(time.Minute)
		if err := h.s.syncAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

// eventFromGoogle is the event here that came from Google's uid, or nil.
func eventFromGoogle(h *harness, uid string) *models.Event {
	for _, e := range h.store.events {
		if e.SeriesID == nil && exportedUID(e.ICalUID) == uid {
			return e
		}
	}
	return nil
}

// moved is how many events the move brought.
func moved(t *testing.T, h *harness, who *model.UserInfo) int {
	t.Helper()
	st, err := h.s.SyncStatus(context.Background(), who)
	if err != nil {
		t.Fatal(err)
	}
	return st.Moved
}

// After the move, either side's change reaches the other within a sweep,
// both ways, with nothing echoed back: a change at Google, a new event and
// a change here, an instance cancelled there, a deletion on each side.
func TestTwoWaySyncWithGoogleKeepsBothSidesInStep(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "st", Event: ICS.Event{UID: "standup@google.com", Summary: "Standup", Start: start, End: start.Add(15 * time.Minute), Zone: "Europe/London", RRule: "FREQ=DAILY;COUNT=5"}})
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch", Start: start.Add(3 * time.Hour), End: start.Add(4 * time.Hour), Zone: "Europe/London"}})
	_, sweep := syncWithGoogle(t, h, sam, g)
	if n := moved(t, h, sam); n != 2 {
		t.Fatalf("moved %d, want 2", n)
	}
	local := func(uid string) *models.Event { return eventFromGoogle(h, uid) }

	// Straight after the move, nothing is sent back.
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("an echo after the move: %v", p)
	}

	// Google's change comes here, and isn't sent back.
	h.clock = h.clock.Add(time.Minute)
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch with Maya", Start: start.Add(3 * time.Hour), End: start.Add(4 * time.Hour), Zone: "Europe/London"}})
	sweep()
	if e := local("lunch@google.com"); e == nil || e.Title != "Lunch with Maya" {
		t.Fatalf("Google's change didn't come: %+v", e)
	}
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("Google's change was sent back: %v", p)
	}

	// An event made here goes to Google once, under its own UID.
	made := h.create(sam, "Pricing review")
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("pushed %v", p)
	}
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("the new event was sent twice: %v", p)
	}
	// A change to it here goes too.
	title := "Pricing review, final"
	seq := made.Sequence
	if _, err := h.s.Update(context.Background(), sam, made.ID, EventPatch{Title: &title, Sequence: &seq}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"update Pricing review, final"}) {
		t.Fatalf("pushed %v", p)
	}

	// An instance cancelled at Google is skipped here.
	rid := start.AddDate(0, 0, 2)
	g.put(calendarSyncBusiness.Change{ExternalID: "st_x", SeriesID: "st", Event: ICS.Event{UID: "standup@google.com", RecurrenceID: &rid, Status: "CANCELLED"}})
	sweep()
	if e := local("standup@google.com"); e == nil || !slices.ContainsFunc(e.Exdates, rid.Equal) {
		t.Fatalf("the cancelled instance is still on: %+v", e)
	}
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("Google's cancelled instance was sent back: %v", p)
	}

	// Deleted at Google: in the trash here. Deleted here: gone at Google.
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Deleted: true})
	sweep()
	if e := local("lunch@google.com"); e == nil || e.DeletedAt == nil {
		t.Fatalf("the event Google deleted is still here: %+v", e)
	}
	if err := h.s.Delete(context.Background(), sam, made.ID, "", nil); err != nil {
		t.Fatal(err)
	}
	g.pushed()
	sweep()
	got := g.pushed()
	if len(got) != 1 || !strings.HasPrefix(got[0], "delete") {
		t.Fatalf("pushed %v", got)
	}
}

// A page of Google's changes keeps where its events are at Google once, and
// drops the deleted ones' once, however many it holds: after the page's
// writes, so each ref is stamped after its event's last change. (Each change
// reads its event as the page's earlier writes left it.)
func TestAPageOfGooglesChangesKeepsItsRefsOnce(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	put := func(i int, title string) {
		at := start.Add(time.Duration(i) * time.Hour)
		g.put(calendarSyncBusiness.Change{ExternalID: fmt.Sprintf("e%d", i), Event: ICS.Event{UID: fmt.Sprintf("e%d@google.com", i), Summary: title, Start: at, End: at.Add(30 * time.Minute), Zone: "Europe/London"}})
	}
	for i := range 4 {
		put(i, fmt.Sprintf("Event %d", i))
	}
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	if n := moved(t, h, sam); n != 4 {
		t.Fatalf("moved %d, want 4", n)
	}
	sweep()

	// Three changed at Google and one deleted, in one page.
	h.clock = h.clock.Add(time.Minute)
	for i := range 3 {
		put(i, fmt.Sprintf("Event %d, moved", i))
	}
	g.put(calendarSyncBusiness.Change{ExternalID: "e3", Deleted: true})
	ledger.mu.Lock()
	ledger.keepCalls, ledger.dropCalls = 0, 0
	ledger.mu.Unlock()
	h.graphTold = nil
	sweep()

	var changed, trashed int
	var written []string
	for id, e := range h.store.events {
		switch {
		case strings.HasSuffix(e.Title, ", moved"):
			changed++
		case e.DeletedAt != nil:
			trashed++
		default:
			continue
		}
		written = append(written, id.String())
	}
	if changed != 3 || trashed != 1 {
		t.Fatalf("changed %d and trashed %d, want 3 and 1", changed, trashed)
	}
	// Google's writes announce nothing: the work graph is told of each.
	told := slices.Clone(h.graphTold)
	slices.Sort(told)
	slices.Sort(written)
	if !slices.Equal(told, written) {
		t.Errorf("the work graph was told of %v, want the %d written", told, len(written))
	}
	ledger.mu.Lock()
	keeps, drops := ledger.keepCalls, ledger.dropCalls
	ledger.mu.Unlock()
	if keeps != 1 || drops != 1 {
		t.Fatalf("where they are at Google was kept %d times and dropped %d times, want once each", keeps, drops)
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("Google's changes were sent back: %v", p)
	}
}

// Changes Google refuses (C9) don't hold back the ones after them: each
// sweep sends the oldest 200 changes, so 201 refused ones used to be sent
// again every minute ahead of the 202nd, which never went.
func TestChangesGoogleRefusesDontHoldBackTheOnesAfterThem(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	g.refuse = func(op calendarSyncBusiness.Op) error {
		if strings.HasPrefix(op.Event.Summary, "Refused") {
			return fmt.Errorf("%w: 400 invalid recurrence", calendarSyncBusiness.ErrRefused)
		}
		return nil
	}
	_, sweep := syncWithGoogle(t, h, sam, g)
	for i := range syncPushPerLink + 1 {
		h.create(sam, fmt.Sprintf("Refused %d", i))
	}
	h.clock = h.clock.Add(time.Second)
	h.create(sam, "Pricing review")
	sweep()
	sweep()
	if p := g.pushed(); !slices.Contains(p, "create Pricing review") {
		t.Fatalf("two sweeps sent %d changes, none of them the review", len(p))
	}
}

// A change Google couldn't take for now (an outage, a rate limit) is sent
// again after its wait, not every minute until then.
func TestAChangeGoogleCouldntTakeIsSentAgainAfterItsWait(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	down := true
	g.refuse = func(calendarSyncBusiness.Op) error {
		if down {
			return errors.New("503 backend error")
		}
		return nil
	}
	_, sweep := syncWithGoogle(t, h, sam, g)
	h.create(sam, "Pricing review")
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("pushed %v", p)
	}
	for range 3 {
		sweep()
	}
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("sent again within its wait: %v", p)
	}
	down = false
	h.clock = h.clock.Add(syncRetries[0] + syncRetries[0]/4)
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("after its wait, pushed %v", p)
	}
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("sent again once Google took it: %v", p)
	}
}

// A change Google refused syncRefusedTries times waits until the event
// changes, however long; a change to it is sent at once.
func TestAChangeGoogleRefusedWaitsUntilTheEventChanges(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	g.refuse = func(op calendarSyncBusiness.Op) error {
		if strings.HasSuffix(op.Event.Summary, "(draft)") {
			return fmt.Errorf("%w: 400 bad request", calendarSyncBusiness.ErrRefused)
		}
		return nil
	}
	_, sweep := syncWithGoogle(t, h, sam, g)
	e := h.create(sam, "Pricing review (draft)")
	tries := 0
	for range 20 {
		sweep()
		tries += len(g.pushed())
		h.clock = h.clock.Add(6 * time.Hour)
	}
	if tries != syncRefusedTries {
		t.Fatalf("sent %d times in 20 sweeps 6 h apart, want %d", tries, syncRefusedTries)
	}
	title, seq := "Pricing review", e.Sequence
	if _, err := h.s.Update(context.Background(), sam, e.ID, EventPatch{Title: &title, Sequence: &seq}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"create Pricing review"}) {
		t.Fatalf("after the change, pushed %v", p)
	}
}

// Nothing is sent to a calendar shared with the person to read only: not
// when Google lists it so, and, once Google refused a change to one listed
// as writable (moved before its access was kept), never again.
func TestNothingIsSentToACalendarThePersonMayOnlyRead(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	shared := func(id, name string, readOnly bool) sharedCalendar {
		return sharedCalendar{Remote: calendarSyncBusiness.Remote{ID: id, Name: name, ReadOnly: readOnly},
			events: []calendarSyncBusiness.Change{{ExternalID: id + "-1", ETag: "v1", Event: ICS.Event{UID: id + "-1@google.com", Summary: name + " day", Start: start, End: start.Add(time.Hour), Zone: "Europe/London"}}}}
	}
	g.shared = []sharedCalendar{shared("holidays", "Holidays", true), shared("team", "Team offsites", false)}
	g.refuse = func(op calendarSyncBusiness.Op) error {
		if op.Remote.ID == "team" {
			return fmt.Errorf("%w: 403 requiredAccessLevel", calendarSyncBusiness.ErrReadOnly)
		}
		return nil
	}
	ledger, sweep := syncWithGoogle(t, h, sam, g)
	// Sam renames both here, a second after the last sweep.
	edit := func(round int) {
		t.Helper()
		h.clock = h.clock.Add(time.Second)
		for uid, name := range map[string]string{"holidays-1@google.com": "Holidays day", "team-1@google.com": "Team offsites day"} {
			e := eventFromGoogle(h, uid)
			title, seq := fmt.Sprintf("%s, %d", name, round), e.Sequence
			if _, err := h.s.Update(context.Background(), sam, e.ID.String(), EventPatch{Title: &title, Sequence: &seq}); err != nil {
				t.Fatal(err)
			}
		}
	}
	edit(1)
	sweep()
	if p := g.pushed(); !slices.Equal(p, []string{"update Team offsites day, 1"}) {
		t.Fatalf("first edits pushed %v", p)
	}
	cals, _ := ledger.calendarsOf(context.Background(), ledger.links[personOf(sam).String()+"google"].ID)
	for _, c := range cals {
		if want := c.RemoteID != "sam@outlook.com"; c.ReadOnly != want {
			t.Errorf("%s read-only %v, want %v", c.RemoteID, c.ReadOnly, want)
		}
	}
	edit(2)
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("sent to a read-only calendar: %v", p)
	}
}

// When Google stops answering for the account, the link breaks and says so,
// and nothing more is read or sent until Google is connected again.
func TestARevokedGoogleAccountBreaksTheLink(t *testing.T) {
	h := newHarness(t)
	sam := h.member("Sam")
	g := newLiveGoogle(func() time.Time { return h.clock })
	start := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch", Start: start, End: start.Add(time.Hour), Zone: "Europe/London"}})
	_, sweep := syncWithGoogle(t, h, sam, g)
	g.revoked = true
	sweep()
	st, err := h.s.SyncStatus(context.Background(), sam)
	if err != nil || st.State != models.SyncBroken || st.Error != syncBrokenWhy {
		t.Fatalf("status %+v %v", st, err)
	}
	g.revoked = false
	h.create(sam, "Pricing review")
	g.put(calendarSyncBusiness.Change{ExternalID: "lunch", Event: ICS.Event{UID: "lunch@google.com", Summary: "Lunch with Maya", Start: start, End: start.Add(time.Hour), Zone: "Europe/London"}})
	g.pushed()
	sweep()
	if p := g.pushed(); len(p) != 0 {
		t.Fatalf("a broken link sent %v", p)
	}
	for _, e := range h.store.events {
		if e.Title == "Lunch with Maya" {
			t.Fatal("a broken link brought Google's change in")
		}
	}
}
